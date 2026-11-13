// Package output produces everything gs3 prints: listing lines, transfer
// results, progress, and error text in the wording of the AWS CLI.
package output

import (
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"gs3/internal/pyfmt"
)

var humanizeSuffixes = []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// HumanReadableSize is awscli's human_readable_size: base-1024 units with one
// decimal, "1 Byte" and "N Bytes" below 1024.
func HumanReadableSize(value int64) string {
	const base = 1024.0
	v := float64(value)
	if v == 1 {
		return "1 Byte"
	}
	if v < base {
		return fmt.Sprintf("%d Bytes", value)
	}
	for i, suffix := range humanizeSuffixes {
		unit := math.Pow(base, float64(i+2))
		// Python's round() rounds half to even.
		if math.RoundToEven(v/unit*base) < base {
			return fmt.Sprintf("%.1f %s", base*v/unit, suffix)
		}
	}
	return fmt.Sprintf("%.1f EiB", v/math.Pow(base, 6))
}

// ErrorMessage renders an error the way the AWS CLI would print it.
//
// Service errors use botocore's ClientError wording,
//
//	An error occurred (NoSuchBucket) when calling the ListObjectsV2 operation: ...
//
// and connection-level failures use botocore's endpoint error messages.
// Anything else falls back to the error's own text.
func ErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var op *smithy.OperationError
	operation := ""
	if errors.As(err, &op) {
		operation = op.OperationName
	}

	var apiErr smithy.APIError
	var respErr *smithyhttp.ResponseError
	hasResp := errors.As(err, &respErr)
	if errors.As(err, &apiErr) {
		code, message := apiErr.ErrorCode(), apiErr.ErrorMessage()
		if hasResp {
			status := respErr.HTTPStatusCode()
			// HEAD responses carry no body, so botocore reports the status
			// line itself: code "404", message "Not Found".
			if strings.HasPrefix(operation, "Head") || code == "" || code == "UnknownError" {
				code, message = strconv.Itoa(status), http.StatusText(status)
			}
		}
		retryInfo := ""
		var maxAttempts *retry.MaxAttemptsError
		if errors.As(err, &maxAttempts) && maxAttempts.Attempt > 1 {
			retryInfo = fmt.Sprintf(" (reached max retries: %d)", maxAttempts.Attempt-1)
		}
		return fmt.Sprintf("An error occurred (%s) when calling the %s operation%s: %s",
			code, operation, retryInfo, message)
	}

	if msg, ok := connectionErrorMessage(err); ok {
		return msg
	}
	if msg, ok := pyfmt.OSError(err); ok {
		return msg
	}
	if isCredentialsError(err) {
		return "Unable to locate credentials"
	}
	return err.Error()
}

// IsNoCredentials reports whether err means no credentials could be found.
func IsNoCredentials(err error) bool { return isCredentialsError(err) }

func isCredentialsError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "failed to retrieve credentials") ||
		strings.Contains(msg, "failed to refresh cached credentials") ||
		strings.Contains(msg, "no EC2 IMDS role found")
}

// connectionErrorMessage maps transport failures to botocore's wording.
func connectionErrorMessage(err error) (string, bool) {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return "", false
	}
	endpoint := urlErr.URL

	var certErr *x509.CertificateInvalidError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuthority) || errors.As(err, &hostnameErr) {
		return fmt.Sprintf("SSL validation failed for %s %s", endpoint, urlErr.Err), true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return fmt.Sprintf("Connect timeout on endpoint URL: %q", endpoint), true
		}
		return fmt.Sprintf("Read timeout on endpoint URL: %q", endpoint), true
	}

	var opErr *net.OpError
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) || (errors.As(err, &opErr) && opErr.Op == "dial") {
		return fmt.Sprintf("Could not connect to the endpoint URL: %q", endpoint), true
	}
	return "", false
}
