package transfer

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"gs3/internal/mimetype"
	"gs3/internal/s3path"
)

// sectionBody is the request body for an upload: a window onto the file, read
// with pread so that parts of one file can be sent concurrently without
// buffering them in memory. It is seekable, which lets the SDK learn the
// length and rewind for a retry.
type sectionBody struct {
	*io.SectionReader
	e *Engine
	j *job
	// counted is how many bytes of this body have been reported as progress.
	counted int64
}

func (e *Engine) newSectionBody(j *job, f *os.File, off, size int64) *sectionBody {
	return &sectionBody{SectionReader: io.NewSectionReader(f, off, size), e: e, j: j}
}

func (b *sectionBody) Read(p []byte) (int, error) {
	n, err := b.SectionReader.Read(p)
	if n > 0 {
		b.e.throttle(n)
		b.counted += int64(n)
		b.e.addBytes(b.j, int64(n))
	}
	return n, err
}

// Seek rewinds for a retry (or after the SDK hashed the body); the progress
// already reported for this body is taken back so bytes are not counted twice.
func (b *sectionBody) Seek(offset int64, whence int) (int64, error) {
	pos, err := b.SectionReader.Seek(offset, whence)
	if err == nil && whence == io.SeekStart && pos == 0 && b.counted > 0 {
		b.e.addBytes(b.j, -b.counted)
		b.counted = 0
	}
	return pos, err
}

// contentType returns the Content-Type to send for a path: the --content-type
// flag, else a guess from the name unless --no-guess-mime-type.
func (e *Engine) contentType(path string) *string {
	if e.cfg.Params.ContentType != "" {
		return aws.String(e.cfg.Params.ContentType)
	}
	if e.cfg.GuessMIME {
		if t := mimetype.Guess(path); t != "" {
			return aws.String(t)
		}
	}
	return nil
}

func (e *Engine) uploadSingle(j *job) error {
	f, err := os.Open(j.fs.Src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	bucket, key := s3path.FindBucketKey(j.fs.Dest)
	return e.putObject(j, f, st.Size(), bucket, key)
}

func (e *Engine) putObject(j *job, f *os.File, size int64, bucket, key string) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   e.newSectionBody(j, f, 0, size),
	}
	e.cfg.Params.applyPut(in)
	in.ContentType = e.contentType(j.fs.Src)
	if e.cfg.ContentMD5 {
		sum, err := contentMD5(f, 0, size)
		if err != nil {
			return err
		}
		in.ContentMD5 = aws.String(sum)
	}
	_, err := e.cfg.Client.PutObject(e.ctx, in, e.cfg.Client.UploadOptions()...)
	return err
}

func (e *Engine) uploadMultipart(j *job) error {
	f, err := os.Open(j.fs.Src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	bucket, key := s3path.FindBucketKey(j.fs.Dest)
	if size == 0 {
		// The file was emptied after it was listed; a multipart upload needs
		// at least one non-empty part.
		if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
			return err
		}
		defer e.cfg.Limiter.Release()
		return e.putObject(j, f, 0, bucket, key)
	}

	create := &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	e.cfg.Params.applyCreateMultipart(create)
	create.ContentType = e.contentType(j.fs.Src)

	partSize := e.partSize(size, e.cfg.ChunkSize)
	count := int((size + partSize - 1) / partSize)

	return e.multipart(bucket, key, create, count, func(ctx context.Context, n int) (string, error) {
		off := int64(n-1) * partSize
		length := min(partSize, size-off)
		in := &s3.UploadPartInput{
			Bucket:     aws.String(bucket),
			Key:        aws.String(key),
			UploadId:   uploadID(ctx),
			PartNumber: aws.Int32(int32(n)),
			Body:       e.newSectionBody(j, f, off, length),
		}
		e.cfg.Params.applyUploadPart(in)
		if e.cfg.ContentMD5 {
			sum, err := contentMD5(f, off, length)
			if err != nil {
				return "", err
			}
			in.ContentMD5 = aws.String(sum)
		}
		out, err := e.cfg.Client.UploadPart(ctx, in, e.cfg.Client.UploadOptions()...)
		if err != nil {
			return "", err
		}
		return aws.ToString(out.ETag), nil
	})
}

// partFunc transfers part n (1-based) of a multipart upload and returns its
// ETag. The upload ID is carried by the context (see uploadID).
type partFunc func(ctx context.Context, n int) (etag string, err error)

type uploadIDKey struct{}

// uploadID returns the multipart upload ID carried by ctx.
func uploadID(ctx context.Context) *string {
	id, _ := ctx.Value(uploadIDKey{}).(string)
	return aws.String(id)
}

// multipart runs the create / parts / complete sequence shared by multipart
// uploads and multipart copies.
//
// Parts are started in ascending order, each under its own token, so the
// total number of requests in flight never exceeds the engine's budget no
// matter how many large files are open. The first failing part cancels the
// others, and the upload is then aborted so no parts are left behind.
func (e *Engine) multipart(bucket, key string, create *s3.CreateMultipartUploadInput, count int, part partFunc) error {
	if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
		return err
	}
	created, err := e.cfg.Client.CreateMultipartUpload(e.ctx, create)
	e.cfg.Limiter.Release()
	if err != nil {
		return err
	}
	id := aws.ToString(created.UploadId)

	ctx, cancel := context.WithCancel(context.WithValue(e.ctx, uploadIDKey{}, id))
	defer cancel()

	etags := make([]string, count)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	for n := 1; n <= count; n++ {
		if err := e.cfg.Limiter.Acquire(ctx); err != nil {
			fail(err)
			break
		}
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			defer e.cfg.Limiter.Release()
			etag, err := part(ctx, n)
			if err != nil {
				fail(err)
				return
			}
			etags[n-1] = etag
		}(n)
	}
	wg.Wait()

	if firstErr == nil {
		firstErr = e.completeMultipart(bucket, key, id, etags)
	}
	if firstErr != nil {
		e.abortMultipart(bucket, key, id)
		// Prefer the error that caused the cancellation over the
		// cancellation itself.
		if errors.Is(firstErr, context.Canceled) && e.ctx.Err() != nil {
			return context.Cause(e.ctx)
		}
		return firstErr
	}
	return nil
}

func (e *Engine) completeMultipart(bucket, key, id string, etags []string) error {
	parts := make([]types.CompletedPart, len(etags))
	for i, etag := range etags {
		parts[i] = types.CompletedPart{ETag: aws.String(etag), PartNumber: aws.Int32(int32(i + 1))}
	}
	if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
		return err
	}
	defer e.cfg.Limiter.Release()
	_, err := e.cfg.Client.CompleteMultipartUpload(e.ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		RequestPayer:    e.cfg.Params.payer(),
	}, e.cfg.Client.SlowOptions()...)
	if err == nil {
		return nil
	}
	// If the response to a successful Complete was lost, the retry finds the
	// upload gone. The object being there means it did succeed.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchUpload" {
		if _, headErr := e.cfg.Client.HeadObject(e.ctx, &s3.HeadObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
		}); headErr == nil {
			return nil
		}
	}
	return err
}

// abortMultipart removes an unfinished upload. It runs even when the engine
// is shutting down, on its own short-lived context.
func (e *Engine) abortMultipart(bucket, key, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), 10*time.Second)
	defer cancel()
	_, _ = e.cfg.Client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(key),
		UploadId:     aws.String(id),
		RequestPayer: e.cfg.Params.payer(),
	})
}
