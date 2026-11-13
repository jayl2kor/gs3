package transfer

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"gs3/internal/core"
	"gs3/internal/output"
	"gs3/internal/s3client"
	"gs3/internal/s3path"
)

// maxUploadSize is S3's object size limit (5 TiB).
const maxUploadSize = 5 << 40

// maxSingleCopySize is the largest object CopyObject accepts (5 GiB).
const maxSingleCopySize = 5 << 30

// Config configures an Engine.
type Config struct {
	// Client is the destination (and default) client; SourceClient reads the
	// source of S3-to-S3 copies and deletes the source of a move.
	Client       *s3client.Client
	SourceClient *s3client.Client
	Reporter     *output.Reporter
	// Limiter is the shared budget of concurrent requests.
	Limiter *Limiter

	QueueSize          int
	MultipartThreshold int64
	ChunkSize          int64
	CopyChunkSize      int64
	MaxParts           int
	// StreamChunkSize and StreamWindow bound a stdin/stdout stream: it holds
	// at most StreamWindow buffers of StreamChunkSize bytes.
	StreamChunkSize int64
	StreamWindow    int
	// Stdin and Stdout are the streams used by `cp - s3://...` and
	// `cp s3://... -`.
	Stdin  io.Reader
	Stdout io.Writer

	// ContentMD5 adds a Content-MD5 header to uploads, as the AWS CLI does.
	ContentMD5 bool
	// BatchDelete groups S3 deletes into DeleteObjects requests.
	BatchDelete bool
	// MaxBandwidth, when positive, caps transfer speed in bytes per second.
	MaxBandwidth int64

	Params Params

	IsMove                bool
	DryRun                bool
	GuessMIME             bool
	ForceGlacierTransfer  bool
	IgnoreGlacierWarnings bool

	// Cwd is the directory local paths are displayed relative to.
	Cwd string
}

// Engine runs transfers. Submit blocks when the pipeline is full, which is
// what bounds memory regardless of how many files a command touches.
type Engine struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelCauseFunc

	tasks          chan core.FileStat
	dispatcherDone chan struct{}
	wg             sync.WaitGroup
	admission      *weighted
	bandwidth      *rate.Limiter

	// submissions counts transfers actually started; only the dispatcher
	// touches it.
	submissions int
	deletes     deleteBatcher

	// noBatchDelete and noPartCopy record that the store rejected an
	// optimisation, so it is not attempted again in this run.
	noBatchDelete atomic.Bool
	noPartCopy    atomic.Bool
}

// New starts an engine.
func New(ctx context.Context, cfg Config) *Engine {
	if cfg.SourceClient == nil {
		cfg.SourceClient = cfg.Client
	}
	if cfg.MaxParts <= 0 {
		cfg.MaxParts = 10000
	}
	if cfg.CopyChunkSize <= 0 {
		cfg.CopyChunkSize = cfg.ChunkSize
	}
	if cfg.StreamWindow <= 0 {
		cfg.StreamWindow = 4
	}
	if cfg.StreamChunkSize <= 0 {
		cfg.StreamChunkSize = 8 << 20
	}
	ctx, cancel := context.WithCancelCause(ctx)
	e := &Engine{
		cfg:            cfg,
		ctx:            ctx,
		cancel:         cancel,
		tasks:          make(chan core.FileStat, max(cfg.QueueSize, 1)),
		dispatcherDone: make(chan struct{}),
		// A large file may hold up to a full budget of parts; allowing twice
		// the budget in admitted weight keeps the pool busy across the tail
		// of one file and the start of the next.
		admission: newWeighted(2 * cfg.Limiter.Limit()),
	}
	if cfg.MaxBandwidth > 0 {
		burst := int(min(cfg.MaxBandwidth, 4<<20))
		e.bandwidth = rate.NewLimiter(rate.Limit(cfg.MaxBandwidth), max(burst, 64<<10))
	}
	go e.dispatch()
	return e
}

// Submit queues one file. It returns false once the engine has been aborted.
func (e *Engine) Submit(fs core.FileStat) bool {
	select {
	case e.tasks <- fs:
		return true
	case <-e.ctx.Done():
		return false
	}
}

// Abort stops the engine after a fatal error: queued files are dropped and
// requests in flight are cancelled.
func (e *Engine) Abort(err error) { e.cancel(err) }

// Close waits for all submitted transfers to finish.
func (e *Engine) Close() {
	close(e.tasks)
	<-e.dispatcherDone
	e.cfg.Reporter.Send(output.Result{Kind: output.FinalTotal, Total: e.submissions})
	e.wg.Wait()
}

// job is one transfer in flight.
type job struct {
	fs           core.FileStat
	transferType string
	src, dest    string // as displayed
	// done counts bytes transferred so far, for failure accounting.
	done atomic.Int64
}

func (e *Engine) newJob(fs core.FileStat) *job {
	j := &job{fs: fs, transferType: fs.Op.String()}
	if e.cfg.IsMove && fs.Op != core.OpDelete {
		j.transferType = "move"
	}
	display := func(path string, t core.PathType) string {
		if t == core.S3 {
			return s3path.DisplayS3(path)
		}
		return s3path.RelativePath(path, e.cfg.Cwd)
	}
	j.src = display(fs.Src, fs.SrcType)
	if fs.Op != core.OpDelete {
		j.dest = display(fs.Dest, fs.DestType)
	}
	return j
}

func (e *Engine) result(kind output.Kind, j *job) output.Result {
	return output.Result{
		Kind: kind, TransferType: j.transferType, Src: j.src, Dest: j.dest,
		Size: j.fs.Size, SizeKnown: j.fs.SizeKnown,
	}
}

// finish reports the outcome of a job.
func (e *Engine) finish(j *job, err error) {
	if err == nil {
		e.cfg.Reporter.Send(e.result(output.Success, j))
		return
	}
	if e.ctx.Err() != nil && errors.Is(err, context.Canceled) {
		// The command is being torn down; the fatal error or interrupt that
		// caused it is reported once, not once per in-flight file.
		return
	}
	res := e.result(output.Failure, j)
	res.Message = output.ErrorMessage(err)
	res.BytesDone = j.done.Load()
	e.cfg.Reporter.Send(res)
}

// addBytes records transfer progress for a job.
func (e *Engine) addBytes(j *job, n int64) {
	j.done.Add(n)
	e.cfg.Reporter.AddBytes(n)
}

// throttle blocks until n bytes may be transferred under --max_bandwidth.
func (e *Engine) throttle(n int) {
	if e.bandwidth == nil {
		return
	}
	for n > 0 {
		step := min(n, e.bandwidth.Burst())
		if err := e.bandwidth.WaitN(e.ctx, step); err != nil {
			return
		}
		n -= step
	}
}

// dispatch is the single goroutine that starts transfers, in the order files
// arrive. Warnings and dry-run lines are produced here so they keep listing
// order, as in the AWS CLI.
func (e *Engine) dispatch() {
	defer close(e.dispatcherDone)
	idle := time.NewTimer(time.Hour)
	defer idle.Stop()
	for {
		// A partial delete batch is flushed when no more work is arriving,
		// so a slow listing does not hold deletions back.
		var flush <-chan time.Time
		if e.deletes.pending() {
			idle.Reset(50 * time.Millisecond)
			flush = idle.C
		}
		select {
		case fs, ok := <-e.tasks:
			if !ok {
				e.flushDeletes()
				return
			}
			if e.ctx.Err() == nil {
				e.start(fs)
			}
		case <-flush:
			e.flushDeletes()
		}
	}
}

func (e *Engine) start(fs core.FileStat) {
	j := e.newJob(fs)
	switch fs.Op {
	case core.OpUpload:
		if fs.Size > maxUploadSize {
			rel := j.src
			e.cfg.Reporter.WarnNoSkip("File " + rel + " exceeds s3 upload limit of " +
				output.HumanReadableSize(maxUploadSize) + ".")
		}
	case core.OpDownload:
		if e.warnGlacier(j) || e.warnParentReference(j) {
			return
		}
	case core.OpCopy:
		if e.warnGlacier(j) {
			return
		}
	}

	if e.cfg.DryRun {
		e.cfg.Reporter.Send(e.result(output.DryRun, j))
		return
	}

	e.submissions++
	switch fs.Op {
	case core.OpDelete:
		if fs.SrcType == core.Local {
			e.deleteLocal(j)
			return
		}
		e.cfg.Reporter.Send(e.result(output.Queued, j))
		if e.cfg.BatchDelete && !e.noBatchDelete.Load() {
			e.deletes.add(j)
			if e.deletes.full() {
				e.flushDeletes()
			}
			return
		}
		e.run(j, func() error { return e.deleteObject(j) })
	case core.OpUpload:
		e.cfg.Reporter.Send(e.result(output.Queued, j))
		if fs.Size >= e.cfg.MultipartThreshold {
			e.runLarge(j, fs.Size, e.cfg.ChunkSize, func() error { return e.uploadMultipart(j) })
		} else {
			e.run(j, func() error { return e.uploadSingle(j) })
		}
	case core.OpDownload:
		e.cfg.Reporter.Send(e.result(output.Queued, j))
		if fs.Size >= e.cfg.MultipartThreshold {
			e.runLarge(j, fs.Size, e.cfg.ChunkSize, func() error { return e.downloadRanged(j) })
		} else {
			e.run(j, func() error { return e.downloadSingle(j) })
		}
	case core.OpCopy:
		e.cfg.Reporter.Send(e.result(output.Queued, j))
		switch {
		case fs.Size >= e.cfg.MultipartThreshold && !e.noPartCopy.Load():
			e.runLarge(j, fs.Size, e.cfg.CopyChunkSize, func() error { return e.copyMultipart(j) })
		case fs.Size > maxSingleCopySize:
			e.runLarge(j, fs.Size, e.cfg.ChunkSize, func() error { return e.copyRelay(j) })
		default:
			e.run(j, func() error { return e.copySingle(j) })
		}
	}
}

// run executes a single-request transfer under one token.
func (e *Engine) run(j *job, fn func() error) {
	if err := e.cfg.Limiter.Acquire(e.ctx); err != nil {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		err := fn()
		if err == nil {
			err = e.afterSuccess(j)
		}
		// Report before releasing the token: if the printer is behind, the
		// engine slows down with it.
		e.finish(j, err)
		e.cfg.Limiter.Release()
	}()
}

// runLarge executes a multi-request transfer. The coordinator goroutine holds
// no token itself; each part acquires one. Admission is weighted by part
// count so that one huge file cannot take the whole budget twice over, while
// many few-part files can be open together.
func (e *Engine) runLarge(j *job, size, chunk int64, fn func() error) {
	parts := int((size + chunk - 1) / max(chunk, 1))
	weight := min(max(parts, 1), e.cfg.Limiter.Limit())
	if err := e.admission.acquire(e.ctx, weight); err != nil {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		err := fn()
		if err == nil {
			err = e.afterSuccess(j)
		}
		e.finish(j, err)
		e.admission.release(weight)
	}()
}

// afterSuccess deletes the source of a move.
func (e *Engine) afterSuccess(j *job) error {
	if !e.cfg.IsMove {
		return nil
	}
	switch j.fs.Op {
	case core.OpUpload:
		return os.Remove(j.fs.Src)
	case core.OpDownload, core.OpCopy:
		return e.deleteSource(j)
	}
	return nil
}

func (e *Engine) warnGlacier(j *job) bool {
	if e.cfg.ForceGlacierTransfer || !j.fs.IsGlacier() {
		return false
	}
	if !e.cfg.IgnoreGlacierWarnings {
		op := j.fs.Op.String()
		e.cfg.Reporter.Warn(s3path.DisplayS3(j.fs.Src),
			"Object is of storage class GLACIER. Unable to perform "+op+
				" operations on GLACIER objects. You must restore the object to be able to perform the operation. See aws s3 "+
				op+" help for additional parameter options to ignore or force these transfers.")
	}
	return true
}

// warnParentReference refuses to download a key that would land outside the
// destination directory ("../x").
func (e *Engine) warnParentReference(j *job) bool {
	clean := filepath.Clean(strings.ReplaceAll(j.fs.CompareKey, "/", string(os.PathSeparator)))
	if strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		e.cfg.Reporter.Warn(j.fs.CompareKey, "File references a parent directory.")
		return true
	}
	return false
}

// partSize picks the size of multipart parts: the configured chunk size,
// raised when the object would otherwise need more than MaxParts parts, and
// kept within S3's 5 MiB - 5 GiB range.
func (e *Engine) partSize(size, chunk int64) int64 {
	const minPart, maxPart = 5 << 20, 5 << 30
	if needed := (size + int64(e.cfg.MaxParts) - 1) / int64(e.cfg.MaxParts); needed > chunk {
		// Round up to a whole MiB to keep part boundaries tidy.
		chunk = (needed + (1<<20 - 1)) &^ (1<<20 - 1)
	}
	return min(max(chunk, minPart), maxPart)
}

// weighted is a semaphore with weights, used only for admitting large files.
type weighted struct {
	mu   sync.Mutex
	cond *sync.Cond
	cap  int
	used int
}

func newWeighted(capacity int) *weighted {
	w := &weighted{cap: max(capacity, 1)}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *weighted) acquire(ctx context.Context, n int) error {
	n = min(n, w.cap)
	stop := context.AfterFunc(ctx, func() {
		w.mu.Lock()
		w.cond.Broadcast()
		w.mu.Unlock()
	})
	defer stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.used+n > w.cap {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.used += n
	return nil
}

func (w *weighted) release(n int) {
	n = min(n, w.cap)
	w.mu.Lock()
	w.used -= n
	w.cond.Broadcast()
	w.mu.Unlock()
}

// copyBuffers recycles the buffers used to move bytes between sockets and
// files.
var copyBuffers = sync.Pool{New: func() any {
	b := make([]byte, 256<<10)
	return &b
}}

// contentMD5 returns the base64 MD5 of size bytes of r starting at off.
func contentMD5(r io.ReaderAt, off, size int64) (string, error) {
	h := md5.New()
	buf := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(buf)
	if _, err := io.CopyBuffer(h, io.NewSectionReader(r, off, size), *buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// tempSuffix is the ".XXXXXXXX" appended to a download's temporary file.
func tempSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "." + hex.EncodeToString(b[:])
}
