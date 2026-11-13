// Package syncer decides what `aws s3 sync` transfers and deletes. It is a
// port of awscli's Comparator and sync strategies.
package syncer

import (
	"context"

	"gs3/internal/core"
	"gs3/internal/listing"
)

// Options select the sync strategies.
type Options struct {
	// SizeOnly transfers only when sizes differ (--size-only).
	SizeOnly bool
	// ExactTimestamps makes a download happen whenever the times differ at
	// all (--exact-timestamps). When both flags are given this one wins, as
	// in the AWS CLI.
	ExactTimestamps bool
	// Delete removes destination files that have no source (--delete).
	Delete bool
}

// ShouldSync decides whether a file present on both sides is transferred.
//
// The default rule compares size and time. The time rule depends on the
// direction:
//   - upload and copy: transfer when the source is newer than the destination;
//   - download: transfer when the local file is newer than the S3 object.
//
// The download rule looks backwards, but it follows from how downloads work:
// a downloaded file is given the object's timestamp, so an untouched local
// copy is never newer; one that is newer was modified locally after the
// download, and sync restores it from S3.
func (o Options) ShouldSync(src, dst *core.FileStat) bool {
	sameSize := src.Size == dst.Size
	if o.SizeOnly && !o.ExactTimestamps {
		return !sameSize
	}
	return !sameSize || !o.sameTime(src, dst)
}

func (o Options) sameTime(src, dst *core.FileStat) bool {
	delta := dst.ModTime.Sub(src.ModTime)
	switch src.Op {
	case core.OpUpload, core.OpCopy:
		// Destination same age or newer: nothing to do.
		return delta >= 0
	case core.OpDownload:
		if o.ExactTimestamps {
			return delta == 0
		}
		return delta <= 0
	}
	return false
}

// Run merges the source and destination listings and calls emit for every
// file to transfer and (with Delete) every destination file to remove.
//
// Both listings are in ascending compare-key order, so a single pass decides
// everything in constant memory:
//
//	key on both sides -> transfer if ShouldSync
//	key only at source -> transfer
//	key only at destination -> delete, if requested
//
// Run returns the first listing error. On an error it stops at once and emits
// nothing further: in particular a source listing that failed part-way never
// causes the destination files it had not reached yet to be deleted.
func Run(ctx context.Context, src, dst *listing.Stream, opts Options, emit func(core.FileStat) bool) error {
	s := &cursor{stream: src}
	d := &cursor{stream: dst}

	for {
		sf, sok, err := s.peek(ctx)
		if err != nil {
			return err
		}
		df, dok, err := d.peek(ctx)
		if err != nil {
			return err
		}

		switch {
		case !sok && !dok:
			return nil
		case sok && dok && sf.CompareKey == df.CompareKey:
			if opts.ShouldSync(sf, df) {
				if !emit(*sf) {
					return ctx.Err()
				}
			}
			s.advance()
			d.advance()
		case sok && (!dok || sf.CompareKey < df.CompareKey):
			// Missing at the destination.
			if !emit(*sf) {
				return ctx.Err()
			}
			s.advance()
		default:
			// Missing at the source.
			if opts.Delete {
				del := *df
				del.Op = core.OpDelete
				if !emit(del) {
					return ctx.Err()
				}
			}
			d.advance()
		}
	}
}

// cursor reads a Stream one entry at a time.
type cursor struct {
	stream *listing.Stream
	batch  []core.FileStat
	pos    int
	done   bool
}

// peek returns the current entry. ok is false at the end of a listing that
// completed successfully; a listing that failed returns its error instead.
func (c *cursor) peek(ctx context.Context) (fs *core.FileStat, ok bool, err error) {
	for !c.done && c.pos >= len(c.batch) {
		batch, open := <-c.stream.C
		if !open {
			c.done = true
			if err := c.stream.Err(); err != nil {
				return nil, false, err
			}
			break
		}
		c.batch, c.pos = batch, 0
	}
	if c.done {
		return nil, false, c.stream.Err()
	}
	return &c.batch[c.pos], true, nil
}

func (c *cursor) advance() { c.pos++ }
