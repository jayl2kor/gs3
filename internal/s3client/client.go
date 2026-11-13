// Package s3client builds the S3 client gs3 uses, configured to behave on the
// wire like botocore 1.33 (the library behind AWS CLI 1.31.13) rather than
// like a default aws-sdk-go-v2 client.
package s3client

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"gs3/internal/awscfg"
)

// DefaultMaxAttempts is how many times a request is tried in total. The AWS
// CLI tries 5 times; gs3 tries a little longer because its callers wrap the
// CLI in retry loops precisely to ride out short storage hiccups.
const DefaultMaxAttempts = 8

// slowOperationTimeout is the minimum idle timeout for operations where the
// server works for a long time before sending anything (large CopyObject,
// CompleteMultipartUpload).
const slowOperationTimeout = 300 * time.Second

// Config is everything needed to build a client.
type Config struct {
	Settings *awscfg.Settings

	// Region, when non-empty, overrides Settings.Region (--source-region).
	Region string
	// EndpointURL, when non-empty, overrides Settings.EndpointURL.
	EndpointURL string

	VerifySSL      bool
	SignRequest    bool
	Debug          bool
	ReadTimeout    time.Duration
	ConnectTimeout time.Duration

	// MaxConns sizes the connection pool.
	MaxConns int
	// OnThrottle, when set, is called for every 429/503 response so the
	// transfer engine can reduce its concurrency.
	OnThrottle func()
	// Stderr receives --debug logging.
	Stderr io.Writer
}

// Client is an S3 client plus the facts about it that commands need.
type Client struct {
	*s3.Client

	// Region is the signing region.
	Region string
	// Endpoint is the custom endpoint URL, or "" for AWS.
	Endpoint string
	// UnsignedUploads reports whether upload bodies are sent with
	// UNSIGNED-PAYLOAD instead of a SHA-256 of the body.
	UnsignedUploads bool

	slowHTTP *http.Client
}

// New builds a client.
func New(ctx context.Context, cfg Config) (*Client, error) {
	settings := cfg.Settings
	region := firstNonEmpty(cfg.Region, settings.Region)
	if region == "" {
		// botocore signs for us-east-1 when nothing configures a region,
		// both for custom endpoints and for the legacy global S3 endpoint.
		region = "us-east-1"
	}

	transportCfg := TransportConfig{
		MaxConns:       cfg.MaxConns,
		ConnectTimeout: cfg.ConnectTimeout,
		IdleTimeout:    cfg.ReadTimeout,
		VerifySSL:      cfg.VerifySSL,
		CABundle:       settings.CABundle,
	}
	transport, err := NewTransport(transportCfg)
	if err != nil {
		return nil, err
	}
	slowCfg := transportCfg
	if slowCfg.IdleTimeout > 0 && slowCfg.IdleTimeout < slowOperationTimeout {
		slowCfg.IdleTimeout = slowOperationTimeout
	}
	slowTransport, err := NewTransport(slowCfg)
	if err != nil {
		return nil, err
	}

	maxAttempts := DefaultMaxAttempts
	if settings.MaxAttempts > 0 {
		maxAttempts = settings.MaxAttempts
	}
	onThrottle := cfg.OnThrottle
	newRetryer := func() aws.Retryer {
		return retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = maxAttempts
			o.MaxBackoff = 20 * time.Second
			// The SDK's client-side retry quota is shared by every request.
			// Under high concurrency a burst of throttling drains it and
			// every later retry fails instantly with "retry quota exceeded".
			// gs3 instead lowers its own concurrency when throttled.
			o.RateLimiter = ratelimit.None
			// Setting Retryables replaces the SDK's defaults, so they are
			// appended explicitly. The custom check goes first so that it
			// sees every error, including the ones the defaults retry.
			custom := retry.IsErrorRetryableFunc(func(err error) aws.Ternary {
				status, code := errorStatus(err)
				if onThrottle != nil && (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) {
					onThrottle()
				}
				// botocore also retries these two.
				if status == http.StatusTooManyRequests || code == "BadDigest" {
					return aws.TrueTernary
				}
				return aws.UnknownTernary
			})
			o.Retryables = append([]retry.IsErrorRetryable{custom}, retry.DefaultRetryables...)
		})
	}

	loadOpts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
		config.WithSharedConfigFiles([]string{settings.ConfigFile}),
		config.WithSharedCredentialsFiles([]string{settings.CredentialsFile}),
		config.WithHTTPClient(&http.Client{Transport: transport}),
		config.WithRetryer(newRetryer),
	}
	if settings.ProfileExplicit {
		loadOpts = append(loadOpts, config.WithSharedConfigProfile(settings.Profile))
	}
	// botocore 1.33 predates the "integrity protections" defaults: no CRC32
	// request checksums, no aws-chunked trailers, no checksum-mode on GET.
	// Naver Cloud's object stores reject some of those, so "when required" is
	// the default unless the user sets the standard variables themselves.
	if os.Getenv("AWS_REQUEST_CHECKSUM_CALCULATION") == "" {
		loadOpts = append(loadOpts, config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired))
	}
	if os.Getenv("AWS_RESPONSE_CHECKSUM_VALIDATION") == "" {
		loadOpts = append(loadOpts, config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired))
	}
	if !cfg.SignRequest {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(aws.AnonymousCredentials{}))
	}
	if cfg.Debug {
		stderr := cfg.Stderr
		if stderr == nil {
			stderr = os.Stderr
		}
		loadOpts = append(loadOpts,
			config.WithLogger(logging.NewStandardLogger(stderr)),
			config.WithClientLogMode(aws.LogRequest|aws.LogResponse|aws.LogRetries))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		var notExist config.SharedConfigProfileNotExistError
		if errors.As(err, &notExist) {
			return nil, &awscfg.ConfigError{Msg: "The config profile (" + settings.Profile + ") could not be found"}
		}
		return nil, err
	}
	// LoadDefaultConfig honours AWS_REGION; AWS CLI 1.31.13 does not.
	awsCfg.Region = region

	endpoint := firstNonEmpty(cfg.EndpointURL, settings.EndpointURL)
	secure := true
	if endpoint != "" {
		if u, err := url.Parse(endpoint); err == nil && u.Scheme == "http" {
			secure = false
		}
	}

	c := &Client{
		Region:   region,
		Endpoint: endpoint,
		slowHTTP: &http.Client{Transport: slowTransport},
	}
	// botocore: sign the payload over plain HTTP, skip it over HTTPS (where
	// Content-MD5 or TLS already protects the body), unless configured.
	if p := settings.S3.PayloadSigningEnabled; p != nil {
		c.UnsignedUploads = !*p
	} else {
		c.UnsignedUploads = secure
	}

	c.Client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		style := settings.S3.AddressingStyle
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		if endpoint != "" && !isAccelerateEndpoint(endpoint) {
			// botocore uses path-style addressing for custom endpoints
			// unless virtual-hosted style is requested explicitly. The Go
			// SDK defaults to virtual-hosted, which S3-compatible stores
			// behind a single hostname cannot serve.
			o.UsePathStyle = style != "virtual"
		} else {
			o.UsePathStyle = style == "path"
		}
		o.UseAccelerate = settings.S3.UseAccelerateEndpoint
		if settings.S3.UseDualstackEndpoint {
			o.EndpointOptions.UseDualStackEndpoint = aws.DualStackEndpointStateEnabled
		}
		o.APIOptions = append(o.APIOptions, removeOperationIDQuery, removeDefaultContentType, contentMD5ForRequiredChecksum)
		// Never send "Expect: 100-continue": a server that ignores it costs
		// a one-second stall on every upload.
		o.ContinueHeaderThresholdBytes = -1
		o.DisableLogOutputChecksumValidationSkipped = true
	})
	return c, nil
}

// removeOperationIDQuery drops the "x-id=<Operation>" query parameter the Go
// SDK adds to many S3 requests (and to presigned URLs). AWS ignores it, but
// botocore never sends it, and an S3-compatible store has no reason to expect
// it; removing it keeps gs3's requests shaped like the AWS CLI's.
func removeOperationIDQuery(stack *middleware.Stack) error {
	return stack.Serialize.Add(middleware.SerializeMiddlewareFunc("gs3RemoveOperationID",
		func(ctx context.Context, in middleware.SerializeInput, next middleware.SerializeHandler) (
			middleware.SerializeOutput, middleware.Metadata, error,
		) {
			if req, ok := in.Request.(*smithyhttp.Request); ok && strings.Contains(req.URL.RawQuery, "x-id=") {
				q := req.URL.Query()
				q.Del("x-id")
				req.URL.RawQuery = q.Encode()
			}
			return next.HandleSerialize(ctx, in)
		}), middleware.After)
}

// removeDefaultContentType stops the SDK from sending
// "Content-Type: application/octet-stream" when the caller set no content
// type. botocore sends no header in that case and S3 then records its own
// default (binary/octet-stream); with the SDK's header the stored type would
// differ from what the AWS CLI produces for the same upload.
func removeDefaultContentType(stack *middleware.Stack) error {
	return stack.Build.Add(middleware.BuildMiddlewareFunc("gs3NoDefaultContentType",
		func(ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler) (
			middleware.BuildOutput, middleware.Metadata, error,
		) {
			if req, ok := in.Request.(*smithyhttp.Request); ok && smithyhttp.GetIsContentTypeDefaultValue(ctx) {
				req.Header.Del("Content-Type")
			}
			return next.HandleBuild(ctx, in)
		}), middleware.After)
}

// md5Operations are the operations S3 requires a body checksum for, among
// those gs3 uses.
var md5Operations = map[string]bool{
	"DeleteObjects":    true,
	"PutBucketWebsite": true,
}

// contentMD5ForRequiredChecksum makes the operations that require a checksum
// carry Content-MD5, as botocore 1.33 does, instead of the x-amz-checksum-crc32
// header current SDKs send. Stores that predate the CRC32 header (Naver
// Cloud's guides pin an old AWS CLI for this reason) accept only Content-MD5.
func contentMD5ForRequiredChecksum(stack *middleware.Stack) error {
	// The stack is named after its operation. Registering only where needed
	// also keeps this out of presigning stacks, which have no "Signing" step
	// to insert before.
	if !md5Operations[stack.ID()] {
		return nil
	}
	return stack.Finalize.Insert(middleware.FinalizeMiddlewareFunc("gs3ContentMD5",
		func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
			middleware.FinalizeOutput, middleware.Metadata, error,
		) {
			req, ok := in.Request.(*smithyhttp.Request)
			if !ok || !md5Operations[middleware.GetOperationName(ctx)] {
				return next.HandleFinalize(ctx, in)
			}
			if stream := req.GetStream(); stream != nil && req.IsStreamSeekable() {
				h := md5.New()
				if _, err := io.Copy(h, stream); err != nil {
					return middleware.FinalizeOutput{}, middleware.Metadata{}, err
				}
				if err := req.RewindStream(); err != nil {
					return middleware.FinalizeOutput{}, middleware.Metadata{}, err
				}
				req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(h.Sum(nil)))
				req.Header.Del("X-Amz-Checksum-Crc32")
				req.Header.Del("X-Amz-Sdk-Checksum-Algorithm")
			}
			return next.HandleFinalize(ctx, in)
		}), "Signing", middleware.Before)
}

// isAccelerateEndpoint is botocore's is_s3_accelerate_url, reduced to the
// hostnames that matter.
func isAccelerateEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return strings.HasPrefix(host, "s3-accelerate.") && strings.HasSuffix(host, ".amazonaws.com")
}

// UploadOptions are the per-call options for PutObject and UploadPart.
func (c *Client) UploadOptions() []func(*s3.Options) {
	if !c.UnsignedUploads {
		return nil
	}
	return []func(*s3.Options){func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	}}
}

// SlowOptions are the per-call options for operations where the server may
// stay silent for minutes before answering.
func (c *Client) SlowOptions() []func(*s3.Options) {
	return []func(*s3.Options){func(o *s3.Options) { o.HTTPClient = c.slowHTTP }}
}

// errorStatus extracts the HTTP status and service error code from an SDK
// error, when present.
func errorStatus(err error) (status int, code string) {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) {
		status = re.HTTPStatusCode()
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code = ae.ErrorCode()
	}
	return status, code
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
