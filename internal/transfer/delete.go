package transfer

import (
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"gs3/internal/output"
	"gs3/internal/s3path"
)

// maxDeleteBatch is the most keys one DeleteObjects request may carry.
const maxDeleteBatch = 1000

// deleteLocal removes a local file (the destination side of
// `sync --delete` when downloading). Like the AWS CLI it runs inline: it is
// fast, and it keeps the result lines in listing order.
func (e *Engine) deleteLocal(j *job) {
	res := e.result(output.Queued, j)
	res.Size, res.SizeKnown = 0, true
	e.cfg.Reporter.Send(res)
	e.finish(j, os.Remove(j.fs.Src))
}

func (e *Engine) deleteObject(j *job) error {
	bucket, key := s3path.FindBucketKey(j.fs.Src)
	_, err := e.cfg.Client.DeleteObject(e.ctx, &s3.DeleteObjectInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(key),
		RequestPayer: e.cfg.Params.payer(),
	})
	return err
}

// deleteSource removes the source object of a completed move.
func (e *Engine) deleteSource(j *job) error {
	bucket, key := s3path.FindBucketKey(j.fs.Src)
	_, err := e.cfg.SourceClient.DeleteObject(e.ctx, &s3.DeleteObjectInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(key),
		RequestPayer: e.cfg.Params.payer(),
	})
	return err
}

// deleteBatcher collects S3 deletions so they can be sent as DeleteObjects
// requests of up to 1,000 keys instead of one request per key, which is what
// makes `rm --recursive` and `sync --delete` fast on large prefixes. It is
// used only by the dispatcher goroutine. All keys of a run share one bucket.
type deleteBatcher struct {
	jobs []*job
}

func (b *deleteBatcher) add(j *job)    { b.jobs = append(b.jobs, j) }
func (b *deleteBatcher) pending() bool { return len(b.jobs) > 0 }
func (b *deleteBatcher) full() bool    { return len(b.jobs) >= maxDeleteBatch }

// flushDeletes sends the pending batch under one token.
func (e *Engine) flushDeletes() {
	jobs := e.deletes.jobs
	if len(jobs) == 0 {
		return
	}
	e.deletes.jobs = nil
	if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.deleteBatch(jobs)
		e.cfg.Limiter.Release()
	}()
}

func (e *Engine) deleteBatch(jobs []*job) {
	bucket, _ := s3path.FindBucketKey(jobs[0].fs.Src)
	byKey := make(map[string]*job, len(jobs))
	objects := make([]types.ObjectIdentifier, len(jobs))
	for i, j := range jobs {
		_, key := s3path.FindBucketKey(j.fs.Src)
		objects[i] = types.ObjectIdentifier{Key: aws.String(key)}
		byKey[key] = j
	}

	out, err := e.cfg.Client.DeleteObjects(e.ctx, &s3.DeleteObjectsInput{
		Bucket:       aws.String(bucket),
		Delete:       &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		RequestPayer: e.cfg.Params.payer(),
	})
	if err != nil {
		if e.ctx.Err() != nil {
			return
		}
		// Whatever went wrong with the batch request, deleting the keys one
		// by one gives each its own, accurate outcome. If the store simply
		// does not support batches, stop using them.
		if isUnsupported(err) {
			e.noBatchDelete.Store(true)
		}
		for _, j := range jobs {
			e.finish(j, e.deleteObject(j))
		}
		return
	}

	// Quiet mode returns only the keys that failed.
	for _, failed := range out.Errors {
		key := aws.ToString(failed.Key)
		j, ok := byKey[key]
		if !ok {
			continue
		}
		delete(byKey, key)
		res := e.result(output.Failure, j)
		res.Message = "An error occurred (" + aws.ToString(failed.Code) +
			") when calling the DeleteObject operation: " + aws.ToString(failed.Message)
		e.cfg.Reporter.Send(res)
	}
	for _, j := range jobs {
		_, key := s3path.FindBucketKey(j.fs.Src)
		if _, ok := byKey[key]; ok {
			e.finish(j, nil)
		}
	}
}
