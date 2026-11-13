package output

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

// Kind is the type of a Result.
type Kind uint8

const (
	// Queued announces a transfer that is about to run.
	Queued Kind = iota
	// Success and Failure end a transfer.
	Success
	Failure
	// Warning reports a skipped or suspicious file.
	Warning
	// Fatal is an error that stops the whole command ("fatal error: ...").
	Fatal
	// Cancelled reports an interrupt.
	Cancelled
	// DryRun announces what a transfer would do.
	DryRun
	// FinalTotal says how many transfers were submitted in total, which is
	// when the progress line stops saying "(calculating...)".
	FinalTotal
)

// Result is one event from the transfer pipeline. It mirrors the result
// classes of awscli.customizations.s3.results.
type Result struct {
	Kind Kind
	// TransferType is "upload", "download", "copy", "delete" or "move".
	TransferType string
	Src          string
	// Dest is "" when the operation has no destination (delete).
	Dest string
	// Size is the transfer's total size; SizeKnown is false when it is not
	// known up front (streams).
	Size      int64
	SizeKnown bool
	// BytesDone, on a Failure, is how much of Size had been transferred.
	BytesDone int64
	// Message is the error text (Failure, Fatal) or the full warning line.
	Message string
	// Total is the number of submissions, for FinalTotal.
	Total int
}

// Mode selects what the printer shows.
type Mode uint8

const (
	// ModeFull prints progress, results, warnings and errors.
	ModeFull Mode = iota
	// ModeNoProgress omits the progress line (--no-progress).
	ModeNoProgress
	// ModeOnlyErrors prints failures, warnings, fatal errors and dry-run
	// lines only (--only-show-errors, and streaming).
	ModeOnlyErrors
	// ModeQuiet prints nothing at all, not even errors (--quiet).
	ModeQuiet
)

// Summary is the outcome of a command.
type Summary struct {
	Failed int
	Warned int
}

// ExitCode is the AWS CLI's rule: 1 if anything failed, else 2 if anything
// was skipped with a warning, else 0.
func (s Summary) ExitCode() int {
	switch {
	case s.Failed > 0:
		return 1
	case s.Warned > 0:
		return 2
	}
	return 0
}

// Reporter collects results from the pipeline, keeps the transfer statistics
// and prints lines in the AWS CLI's formats. It is the only writer to stdout
// and stderr while a transfer command runs.
//
// Byte progress is reported through AddBytes, an atomic counter, rather than
// as one event per chunk: at high throughput per-chunk events would cost more
// than the transfers they describe.
type Reporter struct {
	results chan Result
	done    chan struct{}
	mode    Mode
	out     *bufio.Writer
	errOut  io.Writer

	bytesTransferred atomic.Int64

	// The fields below belong to the printing goroutine.
	bytesFailed    int64
	expectedBytes  int64
	expectedFiles  int
	finalFiles     int
	finalKnown     bool
	filesDone      int
	filesFailed    int
	filesWarned    int
	errors         int
	startTime      time.Time
	progressLength int
}

// progressInterval is how often the progress line is redrawn. The AWS CLI
// redraws on every chunk; five times a second looks the same and costs
// nothing.
const progressInterval = 200 * time.Millisecond

// NewReporter starts a reporter writing to stdout and stderr.
func NewReporter(stdout, stderr io.Writer, mode Mode) *Reporter {
	r := &Reporter{
		results: make(chan Result, 256),
		done:    make(chan struct{}),
		mode:    mode,
		out:     bufio.NewWriterSize(stdout, 64<<10),
		errOut:  stderr,
	}
	go r.run()
	return r
}

// Send delivers a result. It blocks when the printer is behind, which is the
// back-pressure that keeps a slow terminal from being flooded.
func (r *Reporter) Send(res Result) { r.results <- res }

// AddBytes records n transferred bytes.
func (r *Reporter) AddBytes(n int64) { r.bytesTransferred.Add(n) }

// Warn sends a "warning: Skipping file <path>. <message>" result.
func (r *Reporter) Warn(path, message string) {
	r.Send(Result{Kind: Warning, Message: "warning: Skipping file " + path + ". " + message})
}

// WarnNoSkip sends a "warning: <message>" result for a file that is still
// transferred.
func (r *Reporter) WarnNoSkip(message string) {
	r.Send(Result{Kind: Warning, Message: "warning: " + message})
}

// Close waits for every sent result to be printed and returns the summary.
func (r *Reporter) Close() Summary {
	close(r.results)
	<-r.done
	return Summary{Failed: r.filesFailed + r.errors, Warned: r.filesWarned}
}

func (r *Reporter) run() {
	defer close(r.done)
	var tick <-chan time.Time
	if r.mode == ModeFull {
		t := time.NewTicker(progressInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case res, ok := <-r.results:
			if !ok {
				r.finish()
				return
			}
			r.handle(res)
			// Flush when idle so lines appear promptly, but batch writes
			// while results keep arriving.
			if len(r.results) == 0 {
				r.out.Flush()
			}
		case <-tick:
			if r.hasRemainingProgress() && !r.startTime.IsZero() {
				r.printProgress()
				r.out.Flush()
			}
		}
	}
}

func (r *Reporter) finish() {
	if r.progressLength > 0 && r.mode == ModeFull {
		// Blank out a progress line left on screen.
		r.out.WriteString(strings.Repeat(" ", r.progressLength) + "\n")
	}
	r.out.Flush()
}

func (r *Reporter) handle(res Result) {
	switch res.Kind {
	case Queued:
		if r.startTime.IsZero() {
			r.startTime = time.Now()
		}
		if res.SizeKnown {
			r.expectedBytes += res.Size
		}
		r.expectedFiles++
	case Success:
		r.filesDone++
		if r.mode == ModeFull || r.mode == ModeNoProgress {
			r.printLine(res.TransferType + ": " + location(res))
		}
	case Failure:
		if res.SizeKnown {
			r.bytesFailed += res.Size - res.BytesDone
		}
		r.filesFailed++
		r.filesDone++
		r.printError(res.TransferType + " failed: " + location(res) + " " + res.Message)
	case Warning:
		r.filesWarned++
		r.printError(res.Message)
	case Fatal:
		r.errors++
		r.printError("fatal error: " + res.Message)
	case Cancelled:
		r.errors++
		r.printError("cancelled: ctrl-c received")
	case DryRun:
		if r.mode != ModeQuiet {
			r.printLine("(dryrun) " + res.TransferType + ": " + location(res))
		}
	case FinalTotal:
		r.finalFiles, r.finalKnown = res.Total, true
	}
}

func location(res Result) string {
	if res.Dest == "" {
		return res.Src
	}
	return res.Src + " to " + res.Dest
}

// pad extends a line to cover the progress line it overwrites.
func (r *Reporter) pad(line string) string {
	if n := r.progressLength - len([]rune(line)); n > 0 {
		line += strings.Repeat(" ", n)
	}
	return line
}

func (r *Reporter) printLine(line string) {
	r.out.WriteString(r.pad(line))
	r.out.WriteByte('\n')
	r.progressLength = 0
}

func (r *Reporter) printError(line string) {
	if r.mode == ModeQuiet {
		return
	}
	// Keep stdout and stderr in order relative to each other.
	r.out.Flush()
	fmt.Fprintln(r.errOut, r.pad(line))
	r.progressLength = 0
}

func (r *Reporter) totalsAreFinal() bool {
	return r.finalKnown && r.finalFiles == r.expectedFiles
}

func (r *Reporter) hasRemainingProgress() bool {
	return !r.totalsAreFinal() || r.filesDone != r.expectedFiles
}

func (r *Reporter) estimate(s string) string {
	if r.totalsAreFinal() {
		return s
	}
	return "~" + s
}

func (r *Reporter) printProgress() {
	remaining := r.estimate(fmt.Sprint(r.expectedFiles - r.filesDone))
	var line string
	if r.expectedBytes > 0 {
		transferred := r.bytesTransferred.Load()
		speed := 0.0
		if elapsed := time.Since(r.startTime).Seconds(); elapsed > 0 {
			speed = float64(transferred) / elapsed
		}
		line = fmt.Sprintf("Completed %s/%s (%s/s) with %s file(s) remaining",
			HumanReadableSize(transferred+r.bytesFailed),
			r.estimate(HumanReadableSize(r.expectedBytes)),
			HumanReadableSize(int64(speed)), remaining)
	} else {
		line = fmt.Sprintf("Completed %d file(s) with %s file(s) remaining", r.filesDone, remaining)
	}
	if !r.totalsAreFinal() {
		line += " (calculating...)"
	}
	line = r.pad(line)
	r.progressLength = len([]rune(line))
	r.out.WriteString(line)
	r.out.WriteByte('\r')
}
