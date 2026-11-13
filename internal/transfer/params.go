package transfer

import (
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Params are the request parameters derived from command-line flags. It plays
// the role of awscli's RequestParamsMapper: each apply method below sets the
// same fields on the same operations as the mapper does.
type Params struct {
	ACL                string
	StorageClass       string
	WebsiteRedirect    string
	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	ContentLanguage    string
	Expires            *time.Time

	GrantRead        string
	GrantFullControl string
	GrantReadACP     string
	GrantWriteACP    string

	Metadata          map[string]string
	MetadataDirective string

	SSE         string
	SSEKMSKeyID string

	SSECAlgorithm           string
	SSECKey                 string
	CopySourceSSECAlgorithm string
	CopySourceSSECKey       string

	RequestPayer string
}

// SetGrants parses --grants values ("permission=grantee").
func (p *Params) SetGrants(grants []string) error {
	for _, g := range grants {
		permission, grantee, ok := strings.Cut(g, "=")
		if !ok {
			return errors.New("grants should be of the form permission=principal")
		}
		switch permission {
		case "read":
			p.GrantRead = grantee
		case "full":
			p.GrantFullControl = grantee
		case "readacl":
			p.GrantReadACP = grantee
		case "writeacl":
			p.GrantWriteACP = grantee
		default:
			return errors.New("permission must be one of: read|readacl|writeacl|full")
		}
	}
	return nil
}

// ParseExpires converts an --expires value the way botocore's timestamp
// serializer does: epoch seconds or an ISO 8601 / RFC 1123 date, with a date
// lacking a zone taken as UTC.
func ParseExpires(value string) (time.Time, error) {
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9)).UTC(), nil
	}
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02", time.RFC1123, time.RFC1123Z,
		time.RFC850, time.ANSIC, "20060102T150405Z",
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("Invalid timestamp %q: Unknown string format: %s", value, value)
}

func str(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

// sseKey returns the base64 key and its base64 MD5. botocore derives these
// from the raw key; the Go SDK expects the caller to.
func sseKey(raw string) (key, digest *string) {
	if raw == "" {
		return nil, nil
	}
	sum := md5.Sum([]byte(raw))
	return aws.String(base64.StdEncoding.EncodeToString([]byte(raw))),
		aws.String(base64.StdEncoding.EncodeToString(sum[:]))
}

func (p *Params) payer() types.RequestPayer { return types.RequestPayer(p.RequestPayer) }

func (p *Params) applyPut(in *s3.PutObjectInput) {
	in.ACL = types.ObjectCannedACL(p.ACL)
	in.StorageClass = types.StorageClass(p.StorageClass)
	in.WebsiteRedirectLocation = str(p.WebsiteRedirect)
	if p.ContentType != "" {
		in.ContentType = aws.String(p.ContentType)
	}
	in.CacheControl = str(p.CacheControl)
	in.ContentDisposition = str(p.ContentDisposition)
	in.ContentEncoding = str(p.ContentEncoding)
	in.ContentLanguage = str(p.ContentLanguage)
	in.Expires = p.Expires
	in.GrantRead, in.GrantFullControl = str(p.GrantRead), str(p.GrantFullControl)
	in.GrantReadACP, in.GrantWriteACP = str(p.GrantReadACP), str(p.GrantWriteACP)
	if len(p.Metadata) > 0 {
		in.Metadata = p.Metadata
	}
	in.ServerSideEncryption = types.ServerSideEncryption(p.SSE)
	in.SSEKMSKeyId = str(p.SSEKMSKeyID)
	if p.SSECAlgorithm != "" {
		in.SSECustomerAlgorithm = aws.String(p.SSECAlgorithm)
		in.SSECustomerKey, in.SSECustomerKeyMD5 = sseKey(p.SSECKey)
	}
	in.RequestPayer = p.payer()
}

func (p *Params) applyCreateMultipart(in *s3.CreateMultipartUploadInput) {
	in.ACL = types.ObjectCannedACL(p.ACL)
	in.StorageClass = types.StorageClass(p.StorageClass)
	in.WebsiteRedirectLocation = str(p.WebsiteRedirect)
	if p.ContentType != "" {
		in.ContentType = aws.String(p.ContentType)
	}
	in.CacheControl = str(p.CacheControl)
	in.ContentDisposition = str(p.ContentDisposition)
	in.ContentEncoding = str(p.ContentEncoding)
	in.ContentLanguage = str(p.ContentLanguage)
	in.Expires = p.Expires
	in.GrantRead, in.GrantFullControl = str(p.GrantRead), str(p.GrantFullControl)
	in.GrantReadACP, in.GrantWriteACP = str(p.GrantReadACP), str(p.GrantWriteACP)
	if len(p.Metadata) > 0 {
		in.Metadata = p.Metadata
	}
	in.ServerSideEncryption = types.ServerSideEncryption(p.SSE)
	in.SSEKMSKeyId = str(p.SSEKMSKeyID)
	if p.SSECAlgorithm != "" {
		in.SSECustomerAlgorithm = aws.String(p.SSECAlgorithm)
		in.SSECustomerKey, in.SSECustomerKeyMD5 = sseKey(p.SSECKey)
	}
	in.RequestPayer = p.payer()
}

func (p *Params) applyUploadPart(in *s3.UploadPartInput) {
	if p.SSECAlgorithm != "" {
		in.SSECustomerAlgorithm = aws.String(p.SSECAlgorithm)
		in.SSECustomerKey, in.SSECustomerKeyMD5 = sseKey(p.SSECKey)
	}
	in.RequestPayer = p.payer()
}

func (p *Params) applyCopy(in *s3.CopyObjectInput) {
	in.ACL = types.ObjectCannedACL(p.ACL)
	in.StorageClass = types.StorageClass(p.StorageClass)
	in.WebsiteRedirectLocation = str(p.WebsiteRedirect)
	if p.ContentType != "" {
		in.ContentType = aws.String(p.ContentType)
	}
	in.CacheControl = str(p.CacheControl)
	in.ContentDisposition = str(p.ContentDisposition)
	in.ContentEncoding = str(p.ContentEncoding)
	in.ContentLanguage = str(p.ContentLanguage)
	in.Expires = p.Expires
	in.GrantRead, in.GrantFullControl = str(p.GrantRead), str(p.GrantFullControl)
	in.GrantReadACP, in.GrantWriteACP = str(p.GrantReadACP), str(p.GrantWriteACP)

	in.MetadataDirective = types.MetadataDirective(p.MetadataDirective)
	if len(p.Metadata) > 0 {
		in.Metadata = p.Metadata
		// Supplying metadata implies replacing it.
		if p.MetadataDirective == "" {
			in.MetadataDirective = types.MetadataDirectiveReplace
		}
	}
	in.ServerSideEncryption = types.ServerSideEncryption(p.SSE)
	in.SSEKMSKeyId = str(p.SSEKMSKeyID)
	if p.SSECAlgorithm != "" {
		in.SSECustomerAlgorithm = aws.String(p.SSECAlgorithm)
		in.SSECustomerKey, in.SSECustomerKeyMD5 = sseKey(p.SSECKey)
	}
	if p.CopySourceSSECAlgorithm != "" {
		in.CopySourceSSECustomerAlgorithm = aws.String(p.CopySourceSSECAlgorithm)
		in.CopySourceSSECustomerKey, in.CopySourceSSECustomerKeyMD5 = sseKey(p.CopySourceSSECKey)
	}
	in.RequestPayer = p.payer()
}

func (p *Params) applyUploadPartCopy(in *s3.UploadPartCopyInput) {
	if p.SSECAlgorithm != "" {
		in.SSECustomerAlgorithm = aws.String(p.SSECAlgorithm)
		in.SSECustomerKey, in.SSECustomerKeyMD5 = sseKey(p.SSECKey)
	}
	if p.CopySourceSSECAlgorithm != "" {
		in.CopySourceSSECustomerAlgorithm = aws.String(p.CopySourceSSECAlgorithm)
		in.CopySourceSSECustomerKey, in.CopySourceSSECustomerKeyMD5 = sseKey(p.CopySourceSSECKey)
	}
	in.RequestPayer = p.payer()
}

func (p *Params) applyGet(in *s3.GetObjectInput) {
	if p.SSECAlgorithm != "" {
		in.SSECustomerAlgorithm = aws.String(p.SSECAlgorithm)
		in.SSECustomerKey, in.SSECustomerKeyMD5 = sseKey(p.SSECKey)
	}
	in.RequestPayer = p.payer()
}

// ApplyHead sets the parameters of a HeadObject on the source of a transfer.
// For a copy, the source is read with the copy-source SSE-C key.
func (p *Params) ApplyHead(in *s3.HeadObjectInput, copySource bool) {
	algorithm, key := p.SSECAlgorithm, p.SSECKey
	if copySource {
		algorithm, key = p.CopySourceSSECAlgorithm, p.CopySourceSSECKey
	}
	if algorithm != "" {
		in.SSECustomerAlgorithm = aws.String(algorithm)
		in.SSECustomerKey, in.SSECustomerKeyMD5 = sseKey(key)
	}
	in.RequestPayer = p.payer()
}

// copySourceValue renders the X-Amz-Copy-Source value for "bucket/key": the
// key percent-encoded except for unreserved characters and "/".
func copySourceValue(path string) string {
	bucket, key, _ := strings.Cut(path, "/")
	var b strings.Builder
	b.Grow(len(path) + 8)
	b.WriteString(bucket)
	b.WriteByte('/')
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
