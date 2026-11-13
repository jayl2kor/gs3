package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"gs3/internal/s3path"
)

// downloadAttempts is how many times a response body is re-requested after it
// breaks off mid-stream (s3transfer's num_download_attempts). The SDK's own
// retries stop once response headers arrive, so this is separate.
const downloadAttempts = 5

// tempFile is a download in progress. The AWS CLI writes to
// "<name>.<8 hex digits>" next to the destination and renames it into place
// on success, so a reader never sees a partial file under the final name.
type tempFile struct {
	*os.File
	final string
}

func (e *Engine) createTemp(dest string) (*tempFile, error) {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o777); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("Could not create directory %s: %s", dir, errText(err))
	}
	f, err := os.OpenFile(dest+tempSuffix(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return nil, err
	}
	return &tempFile{File: f, final: dest}, nil
}

// discard removes the temporary file after a failure.
func (t *tempFile) discard() {
	t.Close()
	os.Remove(t.Name())
}

// commit moves the finished download into place and gives it the object's
// modification time. sync decides what to transfer by comparing that time, so
// it is part of the contract, not a nicety.
func (e *Engine) commit(j *job, t *tempFile) error {
	if err := t.Close(); err != nil {
		os.Remove(t.Name())
		return err
	}
	if err := os.Rename(t.Name(), t.final); err != nil {
		os.Remove(t.Name())
		return err
	}
	if !j.fs.ModTime.IsZero() {
		// Whole seconds, as the CLI sets it.
		mtime := time.Unix(j.fs.ModTime.Unix(), 0)
		if err := os.Chtimes(t.final, mtime, mtime); err != nil {
			reason := errText(err)
			if errors.Is(err, syscall.EPERM) {
				reason = "The file was downloaded, but attempting to modify the utime of the file failed. Is the file owned by another user?"
			}
			e.cfg.Reporter.Warn(t.final, "Successfully Downloaded "+t.final+
				" but was unable to update the last modified time. "+reason)
		}
	}
	return nil
}

func errText(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// offsetWriter writes sequentially from a starting offset of a file and
// reports progress. Several may write to one file at once (pwrite).
type offsetWriter struct {
	f   *os.File
	off int64
	e   *Engine
	j   *job
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	w.e.throttle(len(p))
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	if n > 0 {
		w.e.addBytes(w.j, int64(n))
	}
	return n, err
}

// fetch downloads bytes [start, end] of an object (the whole object when
// end < 0) into f at offset start. If the body breaks off part-way it resumes
// from where it stopped rather than starting over.
func (e *Engine) fetch(ctx context.Context, j *job, f *os.File, bucket, key string, start, end int64) error {
	w := &offsetWriter{f: f, off: start, e: e, j: j}
	buf := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(buf)

	var lastErr error
	for attempt := 0; attempt < downloadAttempts; attempt++ {
		in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
		e.cfg.Params.applyGet(in)
		switch {
		case end >= 0:
			in.Range = aws.String(fmt.Sprintf("bytes=%d-%d", w.off, end))
		case w.off > start:
			in.Range = aws.String(fmt.Sprintf("bytes=%d-", w.off))
		}
		out, err := e.cfg.Client.GetObject(ctx, in)
		if err != nil {
			return err
		}
		_, err = io.CopyBuffer(w, out.Body, *buf)
		out.Body.Close()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Only a failed read from the network is worth retrying; a failed
		// write to disk is not going to get better.
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

func (e *Engine) downloadSingle(j *job) error {
	bucket, key := s3path.FindBucketKey(j.fs.Src)
	t, err := e.createTemp(j.fs.Dest)
	if err != nil {
		return err
	}
	if err := e.fetch(e.ctx, j, t.File, bucket, key, 0, -1); err != nil {
		t.discard()
		return err
	}
	return e.commit(j, t)
}

// downloadRanged downloads a large object as parallel ranged GETs written
// straight into the temporary file at their offsets. No part is buffered in
// memory, and ranges are started in ascending order so the file grows roughly
// sequentially.
func (e *Engine) downloadRanged(j *job) error {
	bucket, key := s3path.FindBucketKey(j.fs.Src)
	t, err := e.createTemp(j.fs.Dest)
	if err != nil {
		return err
	}

	size := j.fs.Size
	chunk := e.cfg.ChunkSize
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()

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
	for start := int64(0); start < size; start += chunk {
		if err := e.cfg.Limiter.Acquire(ctx); err != nil {
			fail(err)
			break
		}
		wg.Add(1)
		go func(start int64) {
			defer wg.Done()
			defer e.cfg.Limiter.Release()
			end := min(start+chunk, size) - 1
			if err := e.fetch(ctx, j, t.File, bucket, key, start, end); err != nil {
				fail(err)
			}
		}(start)
	}
	wg.Wait()

	if firstErr != nil {
		t.discard()
		if errors.Is(firstErr, context.Canceled) && e.ctx.Err() != nil {
			return context.Cause(e.ctx)
		}
		return firstErr
	}
	return e.commit(j, t)
}
