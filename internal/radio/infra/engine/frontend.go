package engine

import (
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
)

// FrontEndBlock is the duration of an IQ ring block.
const FrontEndBlock = 10 * time.Millisecond

// frontEnd cuts the connector samples into fixed-size blocks before the IQ
// ring. The ring is bounded in blocks, so its depth in time (RingDuration)
// only holds if every block has the size it was sized for: connectors
// deliver whatever a socket read returned, often much less than a full
// block, which would shrink the ring and make slow readers lose samples.
type frontEnd struct {
	ring  *dsp.Ring[complex64]
	rate  int
	block int

	mu sync.Mutex
	// buf holds the samples of the block being filled, from sample index
	// index and time at.
	buf   []complex64
	index uint64
	at    time.Time
}

// frontEndBlockLen returns the block length at rate: FrontEndBlock worth of
// samples, at most MaxBlock.
func frontEndBlockLen(rate int) int {
	return max(1, min(MaxBlock, int(FrontEndBlock.Seconds()*float64(rate))))
}

func newFrontEnd(rate int) *frontEnd {
	block := frontEndBlockLen(rate)

	return &frontEnd{
		ring: dsp.NewRing[complex64](ringSlots(rate, block), block),
		rate: rate, block: block, buf: make([]complex64, 0, block),
	}
}

// push appends samples starting at index, whose first sample is at t. A
// discontinuity in the indexes flushes the partial block first, so the
// ring readers see the gap.
func (f *frontEnd) push(index uint64, t time.Time, iq []complex64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.buf) > 0 && index != f.index+uint64(len(f.buf)) {
		f.flush()
	}

	for i := 0; i < len(iq); {
		if len(f.buf) == 0 {
			f.index = index + uint64(i)
			f.at = t.Add(time.Duration(i) * time.Second / time.Duration(f.rate))
		}

		n := min(f.block-len(f.buf), len(iq)-i)
		f.buf = append(f.buf, iq[i:i+n]...)
		i += n

		if len(f.buf) == f.block {
			f.flush()
		}
	}
}

func (f *frontEnd) flush() {
	f.ring.Push(f.index, len(f.buf), f.at, f.buf)
	f.buf = f.buf[:0]
}
