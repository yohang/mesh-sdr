// Package dsp holds the node signal processing (TECHNICAL_SPEC §8.3, ADR
// 0014, ADR 0019): bounded rings with per-reader cursors and gap markers,
// the shared spectrum (libcsdr++ FFT), the shared FFT channelizer (gonum,
// overlap-save), the NFM chain (libcsdr++ kernels) and the audio framing
// (PCM, IMA ADPCM in the rx.v1 layout).
//
// The package is technical shared code: no I/O besides the rings, no
// logging, no goroutines of its own. Callers run one goroutine per chain.
package dsp

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrRingClosed is returned by a reader once its ring is closed and drained.
var ErrRingClosed = errors.New("dsp: ring closed")

// Gap marks samples a reader lost because it was too slow (§8.3 "Buffers
// and overflow policies"): sample indexes [From, To).
type Gap struct {
	From, To uint64
}

// Meta describes a block read from a ring.
type Meta struct {
	// Seq is the block number since the ring was created.
	Seq uint64
	// Index is the sample index of the first item (monotonic since the
	// device started).
	Index uint64
	// Span is the number of samples the block covers (its length for IQ).
	Span uint64
	// Time is the UTC time of the first sample.
	Time time.Time
}

type slot[T any] struct {
	meta Meta
	data []T
}

// Ring is a bounded ring of blocks written by one producer and read by any
// number of readers, each with its own cursor. A reader that falls behind
// loses the oldest blocks (only that reader) and gets a gap marker. The
// writer never blocks.
type Ring[T any] struct {
	mu     sync.Mutex
	slots  []slot[T]
	next   uint64
	wake   chan struct{}
	closed bool
}

// NewRing returns a ring of n blocks of at most blockLen items each.
func NewRing[T any](n, blockLen int) *Ring[T] {
	r := &Ring[T]{slots: make([]slot[T], max(n, 2)), wake: make(chan struct{})}
	for i := range r.slots {
		r.slots[i].data = make([]T, 0, blockLen)
	}

	return r
}

// Push copies data into the next slot, overwriting the oldest block. index
// is the sample index of the block and span the number of samples it
// covers.
func (r *Ring[T]) Push(index uint64, span int, t time.Time, data []T) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()

		return
	}

	s := &r.slots[r.next%uint64(len(r.slots))]
	s.meta = Meta{Seq: r.next, Index: index, Span: uint64(span), Time: t}
	s.data = append(s.data[:0], data...)
	r.next++
	wake := r.wake
	r.wake = make(chan struct{})
	r.mu.Unlock()

	close(wake)
}

// Close wakes every reader; reads return ErrRingClosed once drained.
func (r *Ring[T]) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.closed {
		r.closed = true
		close(r.wake)
	}
}

// NewReader returns a reader positioned at the next block written.
func (r *Ring[T]) NewReader() *Reader[T] {
	r.mu.Lock()
	defer r.mu.Unlock()

	return &Reader[T]{r: r, seq: r.next}
}

// Reader reads the blocks of a ring in order.
type Reader[T any] struct {
	r   *Ring[T]
	seq uint64
	buf []T
	// Gaps and Lost count the gaps met and the items lost.
	Gaps, Lost uint64
	lastEnd    uint64
	started    bool
}

// Read waits for the next block and returns its metadata and a copy of its
// items, valid until the next Read. gap is non-nil when blocks were lost
// before this one.
func (rd *Reader[T]) Read(ctx context.Context) (Meta, []T, *Gap, error) {
	r := rd.r

	for {
		r.mu.Lock()
		if rd.seq < r.next {
			oldest := uint64(0)
			if r.next > uint64(len(r.slots)) {
				oldest = r.next - uint64(len(r.slots))
			}

			if rd.seq < oldest {
				rd.seq = oldest
			}

			s := &r.slots[rd.seq%uint64(len(r.slots))]
			meta := s.meta
			rd.buf = append(rd.buf[:0], s.data...)
			rd.seq++
			r.mu.Unlock()

			var gap *Gap
			if rd.started && meta.Index > rd.lastEnd {
				gap = &Gap{From: rd.lastEnd, To: meta.Index}
				rd.Gaps++
				rd.Lost += meta.Index - rd.lastEnd
			}

			rd.started = true
			rd.lastEnd = meta.Index + meta.Span

			return meta, rd.buf, gap, nil
		}

		if r.closed {
			r.mu.Unlock()

			return Meta{}, nil, nil, ErrRingClosed
		}

		wake := r.wake
		r.mu.Unlock()

		select {
		case <-ctx.Done():
			return Meta{}, nil, nil, ctx.Err()
		case <-wake:
		}
	}
}
