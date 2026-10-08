package process

import (
	"context"
	"io"
	"sync"
)

// DropOldest is the bounded buffer between a DSP tap and a streaming decoder's
// stdin (§8.3: 2 s, drop the oldest data and insert a gap). Write never
// blocks the producer; Feed blocks on the pipe, which is the tool's own
// back-pressure.
type DropOldest struct {
	mu       sync.Mutex
	cond     *sync.Cond
	chunks   [][]byte
	size     int
	capBytes int
	dropped  int64 // bytes dropped (input_overruns)
	gaps     int64 // drop events (gap markers)
	closed   bool
}

func NewDropOldest(capBytes int) *DropOldest {
	b := &DropOldest{capBytes: capBytes}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Write appends a copy of p, dropping the oldest chunks when over capacity.
func (b *DropOldest) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	c := append([]byte(nil), p...)
	b.chunks = append(b.chunks, c)
	b.size += len(c)
	dropped := false
	for b.size > b.capBytes && len(b.chunks) > 1 {
		b.size -= len(b.chunks[0])
		b.dropped += int64(len(b.chunks[0]))
		b.chunks = b.chunks[1:]
		dropped = true
	}
	if dropped {
		b.gaps++
	}
	b.cond.Signal()
	return len(p), nil
}

// Close ends Feed after the remaining data has been written.
func (b *DropOldest) Close() {
	b.mu.Lock()
	b.closed = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

// Overruns returns dropped bytes and drop events.
func (b *DropOldest) Overruns() (bytes, gaps int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped, b.gaps
}

// Feed copies buffered chunks to w until ctx is done, the buffer is closed and
// drained, or w fails (tool exited: EPIPE). It is used as Spec.Stdin.
func (b *DropOldest) Feed(ctx context.Context, w io.Writer) error {
	stop := context.AfterFunc(ctx, func() {
		b.mu.Lock()
		b.cond.Broadcast()
		b.mu.Unlock()
	})
	defer stop()
	for {
		b.mu.Lock()
		for len(b.chunks) == 0 && !b.closed && ctx.Err() == nil {
			b.cond.Wait()
		}
		if ctx.Err() != nil || (len(b.chunks) == 0 && b.closed) {
			b.mu.Unlock()
			return ctx.Err()
		}
		c := b.chunks[0]
		b.chunks = b.chunks[1:]
		b.size -= len(c)
		b.mu.Unlock()
		if _, err := w.Write(c); err != nil {
			return err
		}
	}
}
