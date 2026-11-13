// Package listing enumerates local files and S3 objects for the transfer
// pipeline, in the order sync needs: ascending by key, compared bytewise.
package listing

import (
	"context"

	"gs3/internal/core"
)

// Stream is a listing in progress: batches of entries in ascending order.
//
// The channel closing does NOT by itself mean the listing is complete. A
// consumer must call Err after the channel closes; only a nil error means
// every entry was delivered. This matters for `sync --delete`: a listing cut
// short by an error must never be mistaken for "the rest does not exist".
type Stream struct {
	C   <-chan []core.FileStat
	err error
}

// Err reports why the listing stopped. It may only be called after C closed.
func (s *Stream) Err() error { return s.err }

// Warner receives the warnings a listing produces for skipped files.
type Warner interface {
	// Warn reports "warning: Skipping file <path>. <message>".
	Warn(path, message string)
}

// start runs produce in a goroutine and exposes its output as a Stream.
// produce calls emit for each batch; emit returns false when the consumer is
// gone and production should stop.
func start(ctx context.Context, buffer int, produce func(emit func([]core.FileStat) bool) error) *Stream {
	ch := make(chan []core.FileStat, buffer)
	s := &Stream{C: ch}
	go func() {
		defer close(ch)
		err := produce(func(batch []core.FileStat) bool {
			if len(batch) == 0 {
				return ctx.Err() == nil
			}
			select {
			case ch <- batch:
				return true
			case <-ctx.Done():
				return false
			}
		})
		if err == nil {
			err = ctx.Err()
		}
		s.err = err
	}()
	return s
}

// batchSize is how many entries are handed over at once. It matches the S3
// page size, so one S3 page becomes one batch.
const batchSize = 1000

// Filter returns a stream of the entries of in that keep reports true for.
// The input's error, if any, becomes the output's.
func Filter(ctx context.Context, in *Stream, keep func(*core.FileStat) bool) *Stream {
	return start(ctx, 2, func(emit func([]core.FileStat) bool) error {
		for batch := range in.C {
			// Filter in place: the batch belongs to this stage now.
			kept := batch[:0]
			for i := range batch {
				if keep(&batch[i]) {
					kept = append(kept, batch[i])
				}
			}
			if !emit(kept) {
				// Drain so the producer can finish.
				for range in.C {
				}
				return nil
			}
		}
		return in.Err()
	})
}
