// Package transfer is gs3's transfer engine: it turns the files chosen by the
// listing and sync stages into S3 requests, running as many at once as the
// concurrency budget allows.
package transfer

import (
	"context"
	"sync"
	"time"
)

// Limiter is a counting semaphore whose limit can change while it is in use.
// Waiters are served first-come first-served, so a large file's parts and the
// small files queued around it share the budget fairly.
//
// One token is one in-flight HTTP request.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	inUse   int
	waiters []chan struct{}
}

// NewLimiter returns a limiter allowing n concurrent holders.
func NewLimiter(n int) *Limiter { return &Limiter{limit: max(n, 1)} }

// Acquire takes a token, waiting if none is free. It returns the context's
// error if ctx ends first.
func (l *Limiter) Acquire(ctx context.Context) error {
	l.mu.Lock()
	if l.inUse < l.limit && len(l.waiters) == 0 {
		l.inUse++
		l.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	l.waiters = append(l.waiters, ch)
	l.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		for i, w := range l.waiters {
			if w == ch {
				l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
				l.mu.Unlock()
				return ctx.Err()
			}
		}
		l.mu.Unlock()
		// The token was granted just as the context ended; hand it back.
		l.Release()
		return ctx.Err()
	}
}

// Release returns a token.
func (l *Limiter) Release() {
	l.mu.Lock()
	l.inUse--
	l.grant()
	l.mu.Unlock()
}

// grant wakes as many waiters as the limit allows. The caller holds mu.
func (l *Limiter) grant() {
	for l.inUse < l.limit && len(l.waiters) > 0 {
		ch := l.waiters[0]
		l.waiters = l.waiters[1:]
		l.inUse++
		close(ch)
	}
}

// SetLimit changes the number of concurrent holders. Lowering it does not
// interrupt current holders; it only delays new ones.
func (l *Limiter) SetLimit(n int) {
	l.mu.Lock()
	l.limit = max(n, 1)
	l.grant()
	l.mu.Unlock()
}

// Limit returns the current limit.
func (l *Limiter) Limit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// Adaptive lowers the concurrency limit when the service throttles and
// restores it gradually (additive increase, multiplicative decrease).
//
// gs3's default concurrency is several times the AWS CLI's. If a store pushes
// back with 429/503, piling on retries at full width makes it worse; halving
// the width lets the retries succeed.
type Adaptive struct {
	limiter *Limiter
	max     int
	min     int

	mu        sync.Mutex
	lastDrop  time.Time
	lastRaise time.Time
}

const (
	// adaptiveHold is how long the limit stays down after a throttle.
	adaptiveHold = 5 * time.Second
	// adaptiveStep is how often the limit grows by one afterwards.
	adaptiveStep = time.Second
)

// NewAdaptive manages l, never raising it above maxLimit.
func NewAdaptive(l *Limiter, maxLimit int) *Adaptive {
	return &Adaptive{limiter: l, max: maxLimit, min: min(4, maxLimit)}
}

// Throttled is called for every 429/503 response.
func (a *Adaptive) Throttled() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	// A burst of throttled responses counts once.
	if now.Sub(a.lastDrop) < time.Second {
		return
	}
	a.lastDrop, a.lastRaise = now, now
	a.limiter.SetLimit(max(a.min, a.limiter.Limit()/2))
}

// Run starts the background recovery and returns a function that stops it.
func (a *Adaptive) Run() (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(adaptiveStep)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				a.recover()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

// recover raises the limit by one step when it has been quiet long enough.
func (a *Adaptive) recover() {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := a.limiter.Limit()
	if cur >= a.max {
		return
	}
	now := time.Now()
	if now.Sub(a.lastDrop) < adaptiveHold || now.Sub(a.lastRaise) < adaptiveStep {
		return
	}
	a.lastRaise = now
	a.limiter.SetLimit(cur + 1)
}
