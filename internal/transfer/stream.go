package transfer

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"gs3/internal/core"
	"gs3/internal/output"
	"gs3/internal/s3path"
)

// Stream runs `cp - s3://bucket/key` (upload from stdin) or
// `cp s3://bucket/key -` (download to stdout).
//
// Streams are the one place gs3 buffers object data: stdin cannot be read at
// an offset and stdout must be written in order. Memory is bounded by
// StreamWindow buffers of StreamChunkSize bytes regardless of object size.
func (e *Engine) Stream(files s3path.Files, op core.Op, expectedSize string) {
	dest, key := files.DestAndCompareKey(files.Src.Path)
	j := &job{
		fs: core.FileStat{
			Src: files.Src.Path, Dest: dest, CompareKey: key,
			SrcType: files.Src.Type, DestType: files.Dest.Type, Op: op,
		},
		transferType: op.String(),
	}
	if op == core.OpUpload {
		j.src, j.dest = "-", s3path.DisplayS3(dest)
	} else {
		j.src, j.dest = s3path.DisplayS3(files.Src.Path), "-"
	}

	expected := int64(-1)
	if op == core.OpUpload && expectedSize != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(expectedSize), 10, 64)
		if err != nil {
			e.cfg.Reporter.Send(output.Result{
				Kind:    output.Fatal,
				Message: "invalid literal for int() with base 10: '" + expectedSize + "'",
			})
			return
		}
		expected = n
		j.fs.Size, j.fs.SizeKnown = n, true
	}

	if e.cfg.DryRun {
		e.cfg.Reporter.Send(e.result(output.DryRun, j))
		return
	}
	e.submissions++
	e.cfg.Reporter.Send(e.result(output.Queued, j))

	var err error
	if op == core.OpUpload {
		err = e.uploadStream(j, expected)
	} else {
		err = e.downloadStream(j)
	}
	e.finish(j, err)
}

// bufferPool hands out a fixed number of equally sized buffers; taking one
// blocks while all are in use, which is what throttles the stream.
type bufferPool struct {
	free chan []byte
}

func newBufferPool(count int, size int64) *bufferPool {
	p := &bufferPool{free: make(chan []byte, count)}
	for i := 0; i < count; i++ {
		// Allocated on first use, so a small stream never pays for the
		// whole window.
		p.free <- nil
	}
	_ = size
	return p
}

func (p *bufferPool) get(ctx context.Context, size int64) ([]byte, error) {
	select {
	case b := <-p.free:
		if int64(cap(b)) < size {
			b = make([]byte, size)
		}
		return b[:size], nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *bufferPool) put(b []byte) { p.free <- b }

func md5Base64(data []byte) *string {
	sum := md5.Sum(data)
	return aws.String(base64.StdEncoding.EncodeToString(sum[:]))
}

// streamContentType is the Content-Type of a stream upload: the flag only,
// since there is no file name to guess from.
func (e *Engine) streamContentType() *string {
	if e.cfg.Params.ContentType != "" {
		return aws.String(e.cfg.Params.ContentType)
	}
	return nil
}

func (e *Engine) uploadStream(j *job, expected int64) error {
	bucket, key := s3path.FindBucketKey(j.fs.Dest)
	partSize := e.cfg.StreamChunkSize
	if expected > 0 {
		// Knowing the size up front lets the part size be raised so that the
		// stream fits in MaxParts parts.
		partSize = e.partSize(expected, partSize)
	}
	pool := newBufferPool(e.cfg.StreamWindow, partSize)

	buf, err := pool.get(e.ctx, partSize)
	if err != nil {
		return err
	}
	n, err := io.ReadFull(e.cfg.Stdin, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if int64(n) < partSize {
		// The whole stream fits in one part: a plain PutObject.
		in := &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			Body: bytes.NewReader(buf[:n]),
		}
		e.cfg.Params.applyPut(in)
		in.ContentType = e.streamContentType()
		if e.cfg.ContentMD5 {
			in.ContentMD5 = md5Base64(buf[:n])
		}
		if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
			return err
		}
		defer e.cfg.Limiter.Release()
		_, err := e.cfg.Client.PutObject(e.ctx, in, e.cfg.Client.UploadOptions()...)
		if err == nil {
			e.addBytes(j, int64(n))
		}
		return err
	}

	create := &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	e.cfg.Params.applyCreateMultipart(create)
	create.ContentType = e.streamContentType()
	if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
		return err
	}
	created, err := e.cfg.Client.CreateMultipartUpload(e.ctx, create)
	e.cfg.Limiter.Release()
	if err != nil {
		return err
	}
	id := aws.ToString(created.UploadId)

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		etags    []string
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}

	for part := 1; ; part++ {
		if part > e.cfg.MaxParts {
			fail(fmt.Errorf("stream needs more than %d parts of %s; pass --expected-size so a larger part size can be chosen",
				e.cfg.MaxParts, output.HumanReadableSize(partSize)))
			pool.put(buf)
			break
		}
		if err := e.cfg.Limiter.Acquire(ctx); err != nil {
			fail(err)
			pool.put(buf)
			break
		}
		mu.Lock()
		etags = append(etags, "")
		mu.Unlock()

		wg.Add(1)
		go func(part int, data []byte) {
			defer wg.Done()
			defer e.cfg.Limiter.Release()
			defer pool.put(data[:cap(data)])
			in := &s3.UploadPartInput{
				Bucket: aws.String(bucket), Key: aws.String(key),
				UploadId: aws.String(id), PartNumber: aws.Int32(int32(part)),
				Body: bytes.NewReader(data),
			}
			e.cfg.Params.applyUploadPart(in)
			if e.cfg.ContentMD5 {
				in.ContentMD5 = md5Base64(data)
			}
			out, err := e.cfg.Client.UploadPart(ctx, in, e.cfg.Client.UploadOptions()...)
			if err != nil {
				fail(err)
				return
			}
			e.addBytes(j, int64(len(data)))
			mu.Lock()
			etags[part-1] = aws.ToString(out.ETag)
			mu.Unlock()
		}(part, buf[:n])

		// Read the next part. Waiting for a free buffer here is what stops
		// stdin from being read faster than parts are uploaded.
		if buf, err = pool.get(ctx, partSize); err != nil {
			fail(err)
			break
		}
		n, err = io.ReadFull(e.cfg.Stdin, buf)
		if n == 0 {
			pool.put(buf)
			if err != nil && !errors.Is(err, io.EOF) {
				fail(err)
			}
			break
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			pool.put(buf)
			fail(err)
			break
		}
	}
	wg.Wait()

	if firstErr == nil {
		firstErr = e.completeMultipart(bucket, key, id, etags)
	}
	if firstErr != nil {
		e.abortMultipart(bucket, key, id)
		if errors.Is(firstErr, context.Canceled) && e.ctx.Err() != nil {
			return context.Cause(e.ctx)
		}
	}
	return firstErr
}

func (e *Engine) downloadStream(j *job) error {
	bucket, key := s3path.FindBucketKey(j.fs.Src)

	head := &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	e.cfg.Params.ApplyHead(head, false)
	if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
		return err
	}
	info, err := e.cfg.Client.HeadObject(e.ctx, head)
	e.cfg.Limiter.Release()
	if err != nil {
		return err
	}
	size := aws.ToInt64(info.ContentLength)
	out := e.cfg.Stdout

	if size < e.cfg.MultipartThreshold {
		in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
		e.cfg.Params.applyGet(in)
		if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
			return err
		}
		defer e.cfg.Limiter.Release()
		resp, err := e.cfg.Client.GetObject(e.ctx, in)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		buf := copyBuffers.Get().(*[]byte)
		defer copyBuffers.Put(buf)
		n, err := io.CopyBuffer(out, resp.Body, *buf)
		e.addBytes(j, n)
		return err
	}

	// Large object: fetch ranges concurrently, a bounded window ahead of the
	// writer, and write them out strictly in order.
	chunk := e.cfg.StreamChunkSize
	count := int((size + chunk - 1) / chunk)
	pool := newBufferPool(e.cfg.StreamWindow, chunk)

	type part struct {
		data []byte
		err  error
	}
	results := make([]chan part, count)
	for i := range results {
		results[i] = make(chan part, 1)
	}

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < count; i++ {
			start := int64(i) * chunk
			length := min(chunk, size-start)
			// Buffers are handed out in part order, so the part the writer
			// is waiting for always has one.
			buf, err := pool.get(ctx, length)
			if err != nil {
				results[i] <- part{err: err}
				return
			}
			if err := e.cfg.Limiter.Acquire(ctx); err != nil {
				results[i] <- part{err: err}
				return
			}
			wg.Add(1)
			go func(i int, buf []byte) {
				defer wg.Done()
				defer e.cfg.Limiter.Release()
				err := e.fetchInto(ctx, bucket, key, start, buf)
				results[i] <- part{data: buf, err: err}
			}(i, buf)
		}
	}()

	var writeErr error
	for i := 0; i < count; i++ {
		p := <-results[i]
		if p.err != nil {
			writeErr = p.err
			break
		}
		if _, err := out.Write(p.data); err != nil {
			writeErr = err
			break
		}
		e.addBytes(j, int64(len(p.data)))
		pool.put(p.data[:cap(p.data)])
	}
	cancel()
	wg.Wait()
	if writeErr != nil && errors.Is(writeErr, context.Canceled) && e.ctx.Err() != nil {
		return context.Cause(e.ctx)
	}
	return writeErr
}

// fetchInto reads len(buf) bytes of an object starting at offset into buf,
// re-requesting the rest if the body breaks off.
func (e *Engine) fetchInto(ctx context.Context, bucket, key string, offset int64, buf []byte) error {
	read := 0
	var lastErr error
	for attempt := 0; attempt < downloadAttempts && read < len(buf); attempt++ {
		in := &s3.GetObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset+int64(read), offset+int64(len(buf))-1)),
		}
		e.cfg.Params.applyGet(in)
		out, err := e.cfg.Client.GetObject(ctx, in)
		if err != nil {
			return err
		}
		n, err := io.ReadFull(out.Body, buf[read:])
		out.Body.Close()
		read += n
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
	}
	if read < len(buf) {
		return lastErr
	}
	return nil
}
