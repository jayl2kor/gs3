// Command probe checks which S3 behaviours an S3-compatible store supports,
// focusing on the ones gs3's design depends on.
//
// It was written for Naver Cloud's Object Storage and Ncloud Storage, whose
// documentation leaves several of these points open, but works against any
// endpoint.
//
// Usage (credentials and endpoint come from the usual AWS_* variables):
//
//	export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
//	export AWS_ENDPOINT_URL=https://kr.object.ncloudstorage.com AWS_DEFAULT_REGION=kr-standard
//	go run ./tools/probe -bucket my-test-bucket
//
// Everything it writes goes under a unique "gs3-probe-<random>/" prefix of the
// given bucket and is deleted again at the end. The bucket itself is never
// created, deleted or reconfigured.
package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"gs3/internal/awscfg"
	"gs3/internal/output"
	"gs3/internal/s3client"
)

type probe struct {
	ctx    context.Context
	bucket string
	prefix string
	client *s3client.Client
	rows   [][3]string
}

// record adds one line to the report.
func (p *probe) record(check string, err error, detail string) {
	status := "ok"
	if err != nil {
		status = "FAILED"
		if detail != "" {
			detail += "; "
		}
		detail += output.ErrorMessage(err)
	}
	p.rows = append(p.rows, [3]string{check, status, detail})
	fmt.Fprintf(os.Stderr, "  %-52s %s %s\n", check, status, detail)
}

func (p *probe) key(name string) *string { return aws.String(p.prefix + name) }

// newClient builds a client like gs3's, with optional adjustments.
func newClient(ctx context.Context, adjust func(*awscfg.Settings), env map[string]string) (*s3client.Client, error) {
	for k, v := range env {
		old, had := os.LookupEnv(k)
		os.Setenv(k, v)
		defer func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		}()
	}
	settings, err := awscfg.Load(os.Getenv, awscfg.Overrides{})
	if err != nil {
		return nil, err
	}
	if adjust != nil {
		adjust(settings)
	}
	return s3client.New(ctx, s3client.Config{
		Settings: settings, VerifySSL: true, SignRequest: true,
		ReadTimeout: 60 * time.Second, ConnectTimeout: 20 * time.Second, MaxConns: 16,
	})
}

func main() {
	bucket := flag.String("bucket", "", "existing bucket to test in (required)")
	keep := flag.Bool("keep", false, "do not delete the probe objects afterwards")
	flag.Parse()
	if *bucket == "" {
		fmt.Fprintln(os.Stderr, "usage: probe -bucket <existing-test-bucket>")
		os.Exit(2)
	}
	// Do not wait long on a store that is simply unreachable.
	if os.Getenv("AWS_MAX_ATTEMPTS") == "" {
		os.Setenv("AWS_MAX_ATTEMPTS", "2")
	}

	ctx := context.Background()
	client, err := newClient(ctx, nil, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", output.ErrorMessage(err))
		os.Exit(1)
	}
	suffix := make([]byte, 4)
	rand.Read(suffix)
	p := &probe{ctx: ctx, bucket: *bucket, client: client, prefix: "gs3-probe-" + hex.EncodeToString(suffix) + "/"}
	fmt.Fprintf(os.Stderr, "endpoint=%s region=%s bucket=%s prefix=%s\n\n", client.Endpoint, client.Region, p.bucket, p.prefix)

	p.addressing()
	p.uploads()
	p.listing()
	p.downloads()
	p.copies()
	p.multipart()
	p.deletes()
	if !*keep {
		p.cleanup()
	}

	fmt.Println("| check | result | detail |")
	fmt.Println("|---|---|---|")
	for _, r := range p.rows {
		fmt.Printf("| %s | %s | %s |\n", r[0], r[1], strings.ReplaceAll(r[2], "|", "\\|"))
	}
}

func (p *probe) addressing() {
	_, err := p.client.ListObjectsV2(p.ctx, &s3.ListObjectsV2Input{Bucket: &p.bucket, MaxKeys: aws.Int32(1)})
	p.record("path-style addressing (gs3 default)", err, "")

	virtual, err := newClient(p.ctx, func(s *awscfg.Settings) { s.S3.AddressingStyle = "virtual" }, nil)
	if err == nil {
		_, err = virtual.ListObjectsV2(p.ctx, &s3.ListObjectsV2Input{Bucket: &p.bucket, MaxKeys: aws.Int32(1)})
	}
	p.record("virtual-hosted-style addressing", err, "")
}

func md5b64(data []byte) *string {
	sum := md5.Sum(data)
	return aws.String(base64.StdEncoding.EncodeToString(sum[:]))
}

func (p *probe) uploads() {
	body := []byte("gs3 probe\n")
	put := func(name string, c *s3client.Client, withMD5 bool, opts ...func(*s3.Options)) error {
		in := &s3.PutObjectInput{Bucket: &p.bucket, Key: p.key(name), Body: bytes.NewReader(body)}
		if withMD5 {
			in.ContentMD5 = md5b64(body)
		}
		_, err := c.PutObject(p.ctx, in, opts...)
		return err
	}

	// What gs3 sends: Content-MD5, payload unsigned over HTTPS.
	p.record("PutObject: Content-MD5 + gs3 payload signing", put("put-md5", p.client, true, p.client.UploadOptions()...),
		fmt.Sprintf("unsigned payload=%v", p.client.UnsignedUploads))
	p.record("PutObject: no Content-MD5", put("put-nomd5", p.client, false, p.client.UploadOptions()...), "")
	p.record("PutObject: SHA-256 signed payload", put("put-sha256", p.client, true), "")

	start := time.Now()
	err := put("put-expect", p.client, true, func(o *s3.Options) { o.ContinueHeaderThresholdBytes = 1 })
	p.record("PutObject: Expect: 100-continue", err, fmt.Sprintf("took %s (about 1s means the header is ignored)", time.Since(start).Round(10*time.Millisecond)))

	// What a current SDK sends by default; the reason old CLIs are pinned.
	modern, err := newClient(p.ctx, nil, map[string]string{"AWS_REQUEST_CHECKSUM_CALCULATION": "when_supported"})
	if err == nil {
		err = put("put-crc32", modern, false)
	}
	p.record("PutObject: SDK default CRC32 checksum (aws-chunked)", err, "not used by gs3; shows whether newer SDKs work")
}

func (p *probe) listing() {
	// Keys chosen so that byte order differs from locale- or case-insensitive
	// order.
	names := []string{"order/a-b", "order/a.txt", "order/a/x", "order/a0", "order/B", "order/a b", "order/한글", "order/a+b", "order/_"}
	for _, n := range names {
		if _, err := p.client.PutObject(p.ctx, &s3.PutObjectInput{Bucket: &p.bucket, Key: p.key(n), Body: bytes.NewReader([]byte("x")), ContentMD5: md5b64([]byte("x"))}, p.client.UploadOptions()...); err != nil {
			p.record("listing: seed objects", err, n)
			return
		}
	}
	want := make([]string, len(names))
	for i, n := range names {
		want[i] = p.prefix + n
	}
	sort.Strings(want)

	var got []string
	input := &s3.ListObjectsV2Input{Bucket: &p.bucket, Prefix: p.key("order/"), MaxKeys: aws.Int32(4), EncodingType: types.EncodingTypeUrl}
	pages := 0
	var listErr error
	for {
		out, err := p.client.ListObjectsV2(p.ctx, input)
		if err != nil {
			listErr = err
			break
		}
		pages++
		for _, c := range out.Contents {
			got = append(got, decode(aws.ToString(c.Key), out.EncodingType == types.EncodingTypeUrl))
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		input.ContinuationToken = out.NextContinuationToken
	}
	detail := fmt.Sprintf("%d pages", pages)
	if listErr == nil && strings.Join(got, "\n") != strings.Join(want, "\n") {
		listErr = errors.New("keys are not returned in byte order")
		detail += fmt.Sprintf("; got %q", got)
	}
	p.record("ListObjectsV2: paging, encoding-type=url, byte order", listErr, detail)

	out, err := p.client.ListObjectsV2(p.ctx, &s3.ListObjectsV2Input{Bucket: &p.bucket, Prefix: p.key("order/"), StartAfter: aws.String(want[len(want)-2])})
	if err == nil && (len(out.Contents) != 1 || aws.ToString(out.Contents[0].Key) != want[len(want)-1]) {
		err = fmt.Errorf("StartAfter ignored: %d keys returned", len(out.Contents))
	}
	p.record("ListObjectsV2: StartAfter", err, "needed for parallel listing")

	dl, err := p.client.ListObjectsV2(p.ctx, &s3.ListObjectsV2Input{Bucket: &p.bucket, Prefix: p.key("order/"), Delimiter: aws.String("/")})
	if err == nil && len(dl.CommonPrefixes) != 1 {
		err = fmt.Errorf("expected 1 common prefix, got %d", len(dl.CommonPrefixes))
	}
	p.record("ListObjectsV2: delimiter", err, "")

	v1, err := p.client.ListObjects(p.ctx, &s3.ListObjectsInput{Bucket: &p.bucket, Prefix: p.key("order/"), MaxKeys: aws.Int32(2)})
	detail = ""
	if err == nil {
		detail = fmt.Sprintf("truncated=%v", aws.ToBool(v1.IsTruncated))
	}
	p.record("ListObjects (V1)", err, detail)

	if listErr == nil && len(got) > 0 {
		lo, err := p.client.ListObjectsV2(p.ctx, &s3.ListObjectsV2Input{Bucket: &p.bucket, Prefix: p.key("put-md5")})
		if err == nil && len(lo.Contents) == 1 {
			lm := aws.ToTime(lo.Contents[0].LastModified)
			p.record("LastModified precision in listings", nil, fmt.Sprintf("%s (sub-second part: %dms)", lm.Format(time.RFC3339Nano), lm.Nanosecond()/1e6))
		}
	}
}

func decode(s string, encoded bool) string {
	if !encoded {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '+':
			b.WriteByte(' ')
		case s[i] == '%' && i+2 < len(s):
			raw, err := hex.DecodeString(s[i+1 : i+3])
			if err != nil {
				b.WriteByte(s[i])
				continue
			}
			b.WriteByte(raw[0])
			i += 2
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func (p *probe) downloads() {
	head, err := p.client.HeadObject(p.ctx, &s3.HeadObjectInput{Bucket: &p.bucket, Key: p.key("put-md5")})
	detail := ""
	if err == nil {
		detail = fmt.Sprintf("ETag=%s LastModified=%s", aws.ToString(head.ETag), aws.ToTime(head.LastModified).Format(time.RFC3339Nano))
	}
	p.record("HeadObject", err, detail)

	_, err = p.client.HeadObject(p.ctx, &s3.HeadObjectInput{Bucket: &p.bucket, Key: p.key("does-not-exist")})
	if err != nil && strings.Contains(output.ErrorMessage(err), "(404)") {
		p.record("HeadObject on a missing key returns 404", nil, "")
	} else {
		p.record("HeadObject on a missing key returns 404", errors.New("unexpected answer"), output.ErrorMessage(err))
	}

	get, err := p.client.GetObject(p.ctx, &s3.GetObjectInput{Bucket: &p.bucket, Key: p.key("put-md5"), Range: aws.String("bytes=4-8")})
	if err == nil {
		data, _ := io.ReadAll(get.Body)
		get.Body.Close()
		if string(data) != "probe" {
			err = fmt.Errorf("range returned %q", data)
		}
	}
	p.record("GetObject: Range", err, "needed for parallel downloads")

	if head != nil {
		ok, err := p.client.GetObject(p.ctx, &s3.GetObjectInput{Bucket: &p.bucket, Key: p.key("put-md5"), IfMatch: head.ETag})
		if err == nil {
			ok.Body.Close()
		}
		p.record("GetObject: If-Match with the current ETag", err, "")
		_, err = p.client.GetObject(p.ctx, &s3.GetObjectInput{Bucket: &p.bucket, Key: p.key("put-md5"), IfMatch: aws.String(`"0123456789abcdef0123456789abcdef"`)})
		if err != nil && strings.Contains(output.ErrorMessage(err), "PreconditionFailed") {
			p.record("GetObject: If-Match with a wrong ETag is refused", nil, "")
		} else {
			p.record("GetObject: If-Match with a wrong ETag is refused", errors.New("the precondition was ignored"), "")
		}
	}

	url, err := s3.NewPresignClient(p.client.Client).PresignGetObject(p.ctx,
		&s3.GetObjectInput{Bucket: &p.bucket, Key: p.key("put-md5")}, s3.WithPresignExpires(time.Minute))
	if err == nil {
		resp, getErr := http.Get(url.URL)
		if getErr != nil {
			err = getErr
		} else {
			resp.Body.Close()
			if resp.StatusCode != 200 {
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}
	}
	p.record("presigned GET URL (SigV4)", err, "")
}

func (p *probe) copies() {
	src := p.bucket + "/" + p.prefix + "put-md5"
	_, err := p.client.CopyObject(p.ctx, &s3.CopyObjectInput{Bucket: &p.bucket, Key: p.key("copy"), CopySource: &src})
	p.record("CopyObject", err, "")
	_, err = p.client.CopyObject(p.ctx, &s3.CopyObjectInput{
		Bucket: &p.bucket, Key: p.key("copy-replace"), CopySource: &src,
		MetadataDirective: types.MetadataDirectiveReplace, Metadata: map[string]string{"k": "v"}, ContentType: aws.String("text/x-probe"),
	})
	if err == nil {
		head, herr := p.client.HeadObject(p.ctx, &s3.HeadObjectInput{Bucket: &p.bucket, Key: p.key("copy-replace")})
		if herr != nil {
			err = herr
		} else if head.Metadata["k"] != "v" || aws.ToString(head.ContentType) != "text/x-probe" {
			err = fmt.Errorf("metadata not replaced: %v %s", head.Metadata, aws.ToString(head.ContentType))
		}
	}
	p.record("CopyObject: MetadataDirective=REPLACE", err, "")
}

func (p *probe) multipart() {
	const partSize = 5 << 20
	part := make([]byte, partSize)
	rand.Read(part)
	small := part[:1<<20]

	create := func(name string) (string, error) {
		out, err := p.client.CreateMultipartUpload(p.ctx, &s3.CreateMultipartUploadInput{Bucket: &p.bucket, Key: p.key(name)})
		if err != nil {
			return "", err
		}
		return aws.ToString(out.UploadId), nil
	}
	upload := func(name, id string, n int32, data []byte) (types.CompletedPart, error) {
		out, err := p.client.UploadPart(p.ctx, &s3.UploadPartInput{
			Bucket: &p.bucket, Key: p.key(name), UploadId: &id, PartNumber: aws.Int32(n),
			Body: bytes.NewReader(data), ContentMD5: md5b64(data),
		}, p.client.UploadOptions()...)
		if err != nil {
			return types.CompletedPart{}, err
		}
		return types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(n)}, nil
	}
	complete := func(name, id string, parts ...types.CompletedPart) error {
		_, err := p.client.CompleteMultipartUpload(p.ctx, &s3.CompleteMultipartUploadInput{
			Bucket: &p.bucket, Key: p.key(name), UploadId: &id,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		})
		return err
	}
	abort := func(name, id string) {
		p.client.AbortMultipartUpload(p.ctx, &s3.AbortMultipartUploadInput{Bucket: &p.bucket, Key: p.key(name), UploadId: &id})
	}

	// A normal two-part upload: 5 MiB + 1 MiB.
	id, err := create("mpu")
	if err == nil {
		var p1, p2 types.CompletedPart
		if p1, err = upload("mpu", id, 1, part); err == nil {
			if p2, err = upload("mpu", id, 2, small); err == nil {
				err = complete("mpu", id, p1, p2)
			}
		}
		if err != nil {
			abort("mpu", id)
		}
	}
	p.record("multipart upload: 5 MiB + 1 MiB parts", err, "")

	// Is the 5 MiB minimum enforced for non-final parts?
	if id, cerr := create("mpu-small"); cerr == nil {
		p1, e1 := upload("mpu-small", id, 1, small)
		p2, e2 := upload("mpu-small", id, 2, small)
		err := errors.Join(e1, e2)
		if err == nil {
			err = complete("mpu-small", id, p1, p2)
		}
		if err != nil {
			abort("mpu-small", id)
			p.record("multipart: 1 MiB non-final part is refused", nil, "minimum part size is enforced")
		} else {
			p.record("multipart: 1 MiB non-final part is refused", nil, "NOT enforced: parts smaller than 5 MiB are accepted")
		}
	}

	// Which part numbers are accepted? Documentation mentions both 1,000 and
	// 10,000 as the limit.
	if id, cerr := create("mpu-numbers"); cerr == nil {
		var accepted []string
		for _, n := range []int32{1000, 1001, 10000} {
			if _, err := upload("mpu-numbers", id, n, []byte("x")); err == nil {
				accepted = append(accepted, fmt.Sprint(n))
			}
		}
		abort("mpu-numbers", id)
		p.record("multipart: accepted part numbers", nil, "accepted: "+strings.Join(accepted, ", ")+" (of 1000, 1001, 10000)")
	}

	// UploadPartCopy, used for server-side copies of large objects.
	src := p.bucket + "/" + p.prefix + "mpu"
	id, err = create("mpu-copy")
	if err == nil {
		var out *s3.UploadPartCopyOutput
		out, err = p.client.UploadPartCopy(p.ctx, &s3.UploadPartCopyInput{
			Bucket: &p.bucket, Key: p.key("mpu-copy"), UploadId: &id, PartNumber: aws.Int32(1),
			CopySource: &src, CopySourceRange: aws.String(fmt.Sprintf("bytes=0-%d", partSize-1)),
		})
		if err == nil && out.CopyPartResult != nil {
			err = complete("mpu-copy", id, types.CompletedPart{ETag: out.CopyPartResult.ETag, PartNumber: aws.Int32(1)})
		}
		if err != nil {
			abort("mpu-copy", id)
		}
	}
	p.record("UploadPartCopy (multipart server-side copy)", err, "if this fails gs3 falls back to CopyObject")
}

func (p *probe) deletes() {
	seed := func(names ...string) []types.ObjectIdentifier {
		var ids []types.ObjectIdentifier
		for _, n := range names {
			p.client.PutObject(p.ctx, &s3.PutObjectInput{Bucket: &p.bucket, Key: p.key(n), Body: bytes.NewReader([]byte("x")), ContentMD5: md5b64([]byte("x"))}, p.client.UploadOptions()...)
			ids = append(ids, types.ObjectIdentifier{Key: p.key(n)})
		}
		return ids
	}

	_, err := p.client.DeleteObject(p.ctx, &s3.DeleteObjectInput{Bucket: &p.bucket, Key: p.key("put-nomd5")})
	p.record("DeleteObject", err, "")

	// gs3's batch delete: Content-MD5, no x-amz-checksum header.
	out, err := p.client.DeleteObjects(p.ctx, &s3.DeleteObjectsInput{
		Bucket: &p.bucket, Delete: &types.Delete{Objects: seed("del/a", "del/b"), Quiet: aws.Bool(true)},
	})
	if err == nil && len(out.Errors) > 0 {
		err = fmt.Errorf("%d keys failed: %s", len(out.Errors), aws.ToString(out.Errors[0].Message))
	}
	p.record("DeleteObjects with Content-MD5 (gs3 batch delete)", err, "if this fails gs3 deletes key by key")
}

// cleanup removes everything under the probe prefix, key by key, which works
// on every store.
func (p *probe) cleanup() {
	input := &s3.ListObjectsV2Input{Bucket: &p.bucket, Prefix: &p.prefix}
	removed := 0
	for {
		out, err := p.client.ListObjectsV2(p.ctx, input)
		if err != nil {
			p.record("cleanup", err, "objects may remain under "+p.prefix)
			return
		}
		for _, c := range out.Contents {
			if _, err := p.client.DeleteObject(p.ctx, &s3.DeleteObjectInput{Bucket: &p.bucket, Key: c.Key}); err == nil {
				removed++
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		input.ContinuationToken = out.NextContinuationToken
	}
	uploads, err := p.client.ListMultipartUploads(p.ctx, &s3.ListMultipartUploadsInput{Bucket: &p.bucket, Prefix: &p.prefix})
	if err == nil {
		for _, u := range uploads.Uploads {
			p.client.AbortMultipartUpload(p.ctx, &s3.AbortMultipartUploadInput{Bucket: &p.bucket, Key: u.Key, UploadId: u.UploadId})
		}
	}
	p.record("cleanup", nil, fmt.Sprintf("removed %d probe objects", removed))
}
