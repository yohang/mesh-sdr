package engine

import (
	"context"
	"testing"
	"time"
)

// TestFrontEndBlocks: small connector reads become fixed-size ring blocks,
// so the IQ ring keeps RingDuration of samples whatever the read size.
func TestFrontEndBlocks(t *testing.T) {
	f := newFrontEnd(rate)
	rd := f.ring.NewReader()
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	// 300 ms in 100-sample reads, nobody reading meanwhile.
	chunk := make([]complex64, 100)
	total := uint64(rate * 3 / 10)

	for i := uint64(0); i < total; i += uint64(len(chunk)) {
		f.push(i, t0.Add(time.Duration(i)*time.Second/rate), chunk)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	meta, iq, _, err := rd.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(iq) != f.block || meta.Index%uint64(f.block) != 0 || !meta.Time.Equal(t0.Add(time.Duration(meta.Index)*time.Second/rate)) {
		t.Fatalf("block %+v of %d samples", meta, len(iq))
	}

	// The ring still holds at least RingDuration of the 300 ms.
	if kept := total - meta.Index; kept < uint64(RingDuration.Seconds()*rate) {
		t.Fatalf("ring kept %d samples, want ≥ %v", kept, RingDuration)
	}

	// A gap in the indexes, then another one: the partial block is flushed
	// first, and the readers see the gap.
	f.push(total+50, t0, chunk)
	f.push(total+50+uint64(f.block), t0, make([]complex64, f.block))

	for meta.Index+meta.Span < total {
		if meta, _, _, err = rd.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}

	meta, iq, gap, err := rd.Read(ctx)
	if err != nil || gap == nil || gap.From != total || gap.To != total+50 || meta.Index != total+50 || len(iq) != len(chunk) {
		t.Fatalf("after the jump: %+v %d samples, gap %+v, %v", meta, len(iq), gap, err)
	}
}
