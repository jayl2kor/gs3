package s3client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// TransportConfig sizes and tunes the HTTP layer.
type TransportConfig struct {
	// MaxConns is the number of idle connections kept per host; it should be
	// at least the number of concurrent requests, otherwise connections are
	// torn down and re-established continuously.
	MaxConns int
	// ConnectTimeout bounds establishing a TCP connection; 0 means no limit.
	ConnectTimeout time.Duration
	// IdleTimeout fails a connection when no byte is read or written for
	// this long while a request is in flight (botocore's read timeout);
	// 0 means no limit.
	IdleTimeout time.Duration
	// VerifySSL false disables certificate verification (--no-verify-ssl).
	VerifySSL bool
	// CABundle, when set, is the only trusted CA file (--ca-bundle).
	CABundle string
}

// NewTransport builds the process-wide transport.
//
// Choices that differ from net/http and the SDK defaults, and why:
//   - HTTP/1.1 only: S3 endpoints speak HTTP/1.1, and multiplexing many
//     transfers over one HTTP/2 connection caps throughput.
//   - MaxIdleConnsPerHost = concurrency: the SDK default is 10, which makes a
//     64-way transfer open and close connections all the time.
//   - 64 KiB socket buffers instead of 4 KiB: fewer syscalls per megabyte.
//   - DisableCompression: object bytes must never be transparently decoded.
func NewTransport(cfg TransportConfig) (*http.Transport, error) {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: !cfg.VerifySSL,
		ClientSessionCache: tls.NewLRUClientSessionCache(256),
	}
	if cfg.VerifySSL && cfg.CABundle != "" {
		pem, err := os.ReadFile(cfg.CABundle)
		if err != nil {
			return nil, fmt.Errorf("Could not find a suitable TLS CA certificate bundle, invalid path: %s", cfg.CABundle)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("Could not find a suitable TLS CA certificate bundle, invalid path: %s", cfg.CABundle)
		}
		tlsConfig.RootCAs = pool
	}

	maxConns := max(cfg.MaxConns, 16)
	dialer := &cachingDialer{
		dialer:      &net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second},
		idleTimeout: cfg.IdleTimeout,
		ttl:         30 * time.Second,
		hosts:       map[string]*hostEntry{},
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   cfg.ConnectTimeout,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // disables HTTP/2
		MaxIdleConns:          maxConns,
		MaxIdleConnsPerHost:   maxConns,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
		ReadBufferSize:        64 << 10,
		WriteBufferSize:       64 << 10,
	}, nil
}

// cachingDialer resolves each host at most once per ttl and spreads
// connections over the returned addresses.
//
// A transfer opens its N connections at the same instant. Without this, that
// is N simultaneous DNS lookups, which on Kubernetes (ndots:5, no resolver
// cache) multiplies into several queries each and can stall start-up.
type cachingDialer struct {
	dialer      *net.Dialer
	idleTimeout time.Duration
	ttl         time.Duration

	mu    sync.Mutex
	hosts map[string]*hostEntry
}

type hostEntry struct {
	ready   chan struct{} // closed when the lookup finished
	addrs   []string
	err     error
	expires time.Time
	next    atomic.Uint32
}

func (d *cachingDialer) lookup(ctx context.Context, host string) ([]string, *hostEntry, error) {
	d.mu.Lock()
	e, ok := d.hosts[host]
	if ok {
		select {
		case <-e.ready:
			if e.err != nil || time.Now().After(e.expires) {
				ok = false
			}
		default: // lookup in flight: wait for it below
		}
	}
	if !ok {
		e = &hostEntry{ready: make(chan struct{})}
		d.hosts[host] = e
		d.mu.Unlock()
		ips, err := net.DefaultResolver.LookupHost(ctx, host)
		e.addrs, e.err, e.expires = ips, err, time.Now().Add(d.ttl)
		close(e.ready)
	} else {
		d.mu.Unlock()
	}

	select {
	case <-e.ready:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	return e.addrs, e, e.err
}

func (d *cachingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := d.dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if d.idleTimeout > 0 {
		return &idleTimeoutConn{Conn: conn, timeout: d.idleTimeout}, nil
	}
	return conn, nil
}

func (d *cachingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) != nil {
		return d.dialer.DialContext(ctx, network, addr)
	}
	addrs, entry, err := d.lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		// Let the standard dialer produce its usual error.
		return d.dialer.DialContext(ctx, network, addr)
	}
	start := int(entry.next.Add(1))
	var lastErr error
	for i := range addrs {
		ip := addrs[(start+i)%len(addrs)]
		conn, err := d.dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// idleTimeoutConn fails I/O when the connection makes no progress for
// `timeout`, which is what botocore's read timeout means: a per-socket-read
// timeout, not a limit on the whole request.
//
// The deadline is pushed forward on writes as well as reads. http.Transport
// keeps a Read pending on pooled connections, so when a new request is written
// to a connection that sat idle, the read deadline must restart from that
// moment; changing a deadline applies to I/O already in progress.
type idleTimeoutConn struct {
	net.Conn
	timeout time.Duration
	// lastExtend avoids a timer update on every small read: the deadline is
	// only moved once it is more than a second stale.
	lastExtend atomic.Int64
}

func (c *idleTimeoutConn) extend() {
	now := time.Now()
	if last := c.lastExtend.Load(); now.UnixNano()-last < int64(time.Second) {
		return
	}
	c.lastExtend.Store(now.UnixNano())
	_ = c.Conn.SetDeadline(now.Add(c.timeout + time.Second))
}

func (c *idleTimeoutConn) Read(p []byte) (int, error) {
	c.extend()
	return c.Conn.Read(p)
}

func (c *idleTimeoutConn) Write(p []byte) (int, error) {
	c.extend()
	return c.Conn.Write(p)
}
