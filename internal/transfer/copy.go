package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"gs3/internal/s3path"
)

func (e *Engine) copySingle(j *job) error {
	bucket, key := s3path.FindBucketKey(j.fs.Dest)
	in := &s3.CopyObjectInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(key),
		CopySource: aws.String(copySourceValue(j.fs.Src)),
	}
	e.cfg.Params.applyCopy(in)
	_, srcKey := s3path.FindBucketKey(j.fs.Src)
	in.ContentType = e.contentType(srcKey)
	// A server-side copy of a big object can take a while to answer.
	_, err := e.cfg.Client.CopyObject(e.ctx, in, e.cfg.Client.SlowOptions()...)
	if err == nil {
		e.addBytes(j, j.fs.Size)
	}
	return err
}

// copyMultipart copies a large object with parallel UploadPartCopy requests.
//
// Unlike a single CopyObject, a multipart copy starts from a blank object, so
// the source's Content-Type and metadata would be lost (they are in AWS CLI
// 1.x). gs3 reads them with one HeadObject and carries them over unless
// --metadata-directive REPLACE asks for a clean slate.
func (e *Engine) copyMultipart(j *job) error {
	bucket, key := s3path.FindBucketKey(j.fs.Dest)
	srcBucket, srcKey := s3path.FindBucketKey(j.fs.Src)

	create := &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	e.cfg.Params.applyCreateMultipart(create)
	create.ContentType = e.contentType(srcKey)

	if e.cfg.Params.MetadataDirective != "REPLACE" && len(e.cfg.Params.Metadata) == 0 {
		head := &s3.HeadObjectInput{Bucket: aws.String(srcBucket), Key: aws.String(srcKey)}
		e.cfg.Params.ApplyHead(head, true)
		if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
			return err
		}
		src, err := e.cfg.SourceClient.HeadObject(e.ctx, head)
		e.cfg.Limiter.Release()
		if err != nil {
			return err
		}
		inherit := func(dst **string, value *string) {
			if *dst == nil && value != nil {
				*dst = value
			}
		}
		if e.cfg.Params.ContentType == "" {
			// The source's real type beats a guess from its name.
			create.ContentType = src.ContentType
		}
		inherit(&create.CacheControl, src.CacheControl)
		inherit(&create.ContentDisposition, src.ContentDisposition)
		inherit(&create.ContentEncoding, src.ContentEncoding)
		inherit(&create.ContentLanguage, src.ContentLanguage)
		inherit(&create.WebsiteRedirectLocation, src.WebsiteRedirectLocation)
		if create.Expires == nil && src.ExpiresString != nil {
			if t, err := http.ParseTime(*src.ExpiresString); err == nil {
				create.Expires = &t
			}
		}
		if len(src.Metadata) > 0 {
			create.Metadata = src.Metadata
		}
	}

	size := j.fs.Size
	partSize := e.partSize(size, e.cfg.CopyChunkSize)
	count := int((size + partSize - 1) / partSize)
	source := copySourceValue(j.fs.Src)

	err := e.multipart(bucket, key, create, count, func(ctx context.Context, n int) (string, error) {
		off := int64(n-1) * partSize
		length := min(partSize, size-off)
		in := &s3.UploadPartCopyInput{
			Bucket:          aws.String(bucket),
			Key:             aws.String(key),
			UploadId:        uploadID(ctx),
			PartNumber:      aws.Int32(int32(n)),
			CopySource:      aws.String(source),
			CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", off, off+length-1)),
		}
		e.cfg.Params.applyUploadPartCopy(in)
		out, err := e.cfg.Client.UploadPartCopy(ctx, in, e.cfg.Client.SlowOptions()...)
		if err != nil {
			return "", err
		}
		e.addBytes(j, length)
		if out.CopyPartResult == nil {
			return "", errors.New("UploadPartCopy returned no result")
		}
		return aws.ToString(out.CopyPartResult.ETag), nil
	})
	if err != nil && isUnsupported(err) {
		// The store has no UploadPartCopy (Naver Cloud's older Object Storage
		// does not list it). Remember that, and copy another way.
		e.noPartCopy.Store(true)
		e.addBytes(j, -j.done.Load())
		if j.fs.Size > maxSingleCopySize {
			return e.copyRelay(j)
		}
		if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
			return err
		}
		defer e.cfg.Limiter.Release()
		return e.copySingle(j)
	}
	return err
}

// isUnsupported reports whether err says the store does not implement the
// operation at all, as opposed to the request having failed.
func isUnsupported(err error) bool {
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.HTTPStatusCode() {
		case http.StatusNotImplemented, http.StatusMethodNotAllowed:
			return true
		}
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotImplemented", "MethodNotAllowed", "UnsupportedOperation":
			return true
		}
	}
	return false
}

// copyRelay copies an object too large for CopyObject (over 5 GiB) on a store
// without UploadPartCopy by downloading each part and uploading it again. It
// is slow compared with a server-side copy and exists only so such copies
// still work.
func (e *Engine) copyRelay(j *job) error {
	bucket, key := s3path.FindBucketKey(j.fs.Dest)
	srcBucket, srcKey := s3path.FindBucketKey(j.fs.Src)

	create := &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	e.cfg.Params.applyCreateMultipart(create)
	create.ContentType = e.contentType(srcKey)

	size := j.fs.Size
	partSize := e.partSize(size, e.cfg.ChunkSize)
	count := int((size + partSize - 1) / partSize)

	// Each part is held in memory between download and upload, so only a few
	// may be in flight.
	window := make(chan struct{}, e.cfg.StreamWindow)

	return e.multipart(bucket, key, create, count, func(ctx context.Context, n int) (string, error) {
		select {
		case window <- struct{}{}:
			defer func() { <-window }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
		off := int64(n-1) * partSize
		length := min(partSize, size-off)

		get := &s3.GetObjectInput{
			Bucket: aws.String(srcBucket), Key: aws.String(srcKey),
			Range:        aws.String(fmt.Sprintf("bytes=%d-%d", off, off+length-1)),
			RequestPayer: e.cfg.Params.payer(),
		}
		if alg := e.cfg.Params.CopySourceSSECAlgorithm; alg != "" {
			get.SSECustomerAlgorithm = aws.String(alg)
			get.SSECustomerKey, get.SSECustomerKeyMD5 = sseKey(e.cfg.Params.CopySourceSSECKey)
		}
		out, err := e.cfg.SourceClient.GetObject(ctx, get)
		if err != nil {
			return "", err
		}
		data := make([]byte, length)
		_, err = io.ReadFull(out.Body, data)
		out.Body.Close()
		if err != nil {
			return "", err
		}

		in := &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			UploadId: uploadID(ctx), PartNumber: aws.Int32(int32(n)),
			Body: bytes.NewReader(data),
		}
		e.cfg.Params.applyUploadPart(in)
		put, err := e.cfg.Client.UploadPart(ctx, in, e.cfg.Client.UploadOptions()...)
		if err != nil {
			return "", err
		}
		e.addBytes(j, length)
		return aws.ToString(put.ETag), nil
	})
}
