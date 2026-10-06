package sendq_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/spikes/spk-07-ws/sendq"
)

func frame(t *testing.T, typ rxv1.FrameType, codec rxv1.Codec, seq uint32, n int) []byte {
	t.Helper()
	b, err := rxv1.AppendFrame(nil, rxv1.FrameHeader{Type: typ, Codec: codec, Seq: seq}, make([]byte, n))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func drain(t *testing.T, q *sendq.Queue) []sendq.Item {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var out []sendq.Item
	for {
		it, err := q.Next(ctx)
		if err != nil {
			return out
		}
		out = append(out, it)
	}
}

func TestPriorityJSONAudioMeterFFT(t *testing.T) {
	q := sendq.New(sendq.DefaultPolicy(), nil)
	q.OpenAudio(2, 20*time.Millisecond)
	q.OpenFFT(1)
	_ = q.PushFFT(1, frame(t, rxv1.FrameFFT, rxv1.CodecFFTU8DB, 1, 10))
	_ = q.PushMeter("d1", []byte(`m`))
	_ = q.PushAudio(2, frame(t, rxv1.FrameAudio, rxv1.CodecOpus, 1, 10))
	_ = q.PushJSON([]byte(`j`))

	items := drain(t, q)
	if len(items) != 4 {
		t.Fatalf("got %d items", len(items))
	}
	if string(items[0].Data) != "j" || items[0].Kind != sendq.Text {
		t.Errorf("first must be JSON, got %q", items[0].Data)
	}
	if h, _, _ := rxv1.ParseFrame(items[1].Data); h.Type != rxv1.FrameAudio {
		t.Errorf("second must be audio, got %v", h.Type)
	}
	if string(items[2].Data) != "m" {
		t.Errorf("third must be meter, got %q", items[2].Data)
	}
	if h, _, _ := rxv1.ParseFrame(items[3].Data); h.Type != rxv1.FrameFFT {
		t.Errorf("fourth must be FFT, got %v", h.Type)
	}
	if q.Pending() != 0 {
		t.Errorf("pending = %d", q.Pending())
	}
}

func TestFFTLatestWins(t *testing.T) {
	q := sendq.New(sendq.DefaultPolicy(), nil)
	q.OpenFFT(1)
	for i := uint32(1); i <= 5; i++ {
		_ = q.PushFFT(1, frame(t, rxv1.FrameFFT, rxv1.CodecFFTU8DB, i, 10))
	}
	items := drain(t, q)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	for i, want := range []uint32{4, 5} {
		h, _, _ := rxv1.ParseFrame(items[i].Data)
		if h.Seq != want {
			t.Errorf("item %d seq = %d, want %d", i, h.Seq, want)
		}
	}
	enq, drop := q.TakeFFTWindow(1)
	if enq != 5 || drop != 3 {
		t.Errorf("window = %d/%d", enq, drop)
	}
	if enq, drop := q.TakeFFTWindow(1); enq != 0 || drop != 0 {
		t.Errorf("window not reset: %d/%d", enq, drop)
	}
}

func TestAudioDropOldestAndFlagDiscontinuity(t *testing.T) {
	q := sendq.New(sendq.DefaultPolicy(), nil)
	q.OpenAudio(2, 20*time.Millisecond) // 500 ms → 25 frames
	shared := make([][]byte, 30)
	for i := range shared {
		shared[i] = frame(t, rxv1.FrameAudio, rxv1.CodecOpus, uint32(i+1), 10)
		if err := q.PushAudio(2, shared[i]); err != nil {
			t.Fatal(err)
		}
	}
	items := drain(t, q)
	if len(items) != 25 {
		t.Fatalf("got %d frames, want 25", len(items))
	}
	h, _, _ := rxv1.ParseFrame(items[0].Data)
	if h.Seq != 6 || !h.Flags.Has(rxv1.FlagDiscontinuity) {
		t.Errorf("first frame seq=%d flags=%v, want seq 6 with discontinuity", h.Seq, h.Flags)
	}
	h, _, _ = rxv1.ParseFrame(items[1].Data)
	if h.Flags.Has(rxv1.FlagDiscontinuity) {
		t.Error("only the first frame after the drop is flagged")
	}
	// The shared buffer must not have been modified (copy on mark).
	if sh, _, _ := rxv1.ParseFrame(shared[5]); sh.Flags != 0 {
		t.Error("shared frame buffer was mutated")
	}
	st := q.Stats()
	if st.AudioDropped != 5 || st.AudioDropRuns != 1 || st.CopiesOnMark != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestAudioStuckAtCapIsSlowConsumer(t *testing.T) {
	now := time.Unix(0, 0)
	q := sendq.New(sendq.DefaultPolicy(), func() time.Time { return now })
	q.OpenAudio(2, 20*time.Millisecond)
	var err error
	for i := 0; i < 25+600 && err == nil; i++ { // 25 to fill, then 12 s of overflow
		now = now.Add(20 * time.Millisecond)
		err = q.PushAudio(2, frame(t, rxv1.FrameAudio, rxv1.CodecOpus, uint32(i), 10))
	}
	if !errors.Is(err, sendq.ErrSlowConsumer) {
		t.Fatalf("err = %v, want slow consumer", err)
	}
	if _, err := q.Next(context.Background()); !errors.Is(err, sendq.ErrSlowConsumer) {
		t.Fatalf("Next err = %v", err)
	}
}

func TestAudioBacklogDrainResetsStuckTimer(t *testing.T) {
	now := time.Unix(0, 0)
	q := sendq.New(sendq.DefaultPolicy(), func() time.Time { return now })
	q.OpenAudio(2, 20*time.Millisecond)
	// Fill to the cap, then 30 s where the consumer drains a little faster
	// than real time: overflow happens but never lasts 10 s.
	for i := 0; i < 1500+25; i++ {
		now = now.Add(20 * time.Millisecond)
		if err := q.PushAudio(2, frame(t, rxv1.FrameAudio, rxv1.CodecOpus, uint32(i), 10)); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		if i > 25 && i%5 == 0 {
			for range 6 {
				drain1(t, q)
			}
		}
	}
}

func TestAudioPartialDrainStillSlowConsumer(t *testing.T) {
	// The consumer drains 3 frames per 5 produced: the queue is never
	// "above" its cap, yet it overflows continuously → 4413 after 10 s.
	now := time.Unix(0, 0)
	q := sendq.New(sendq.DefaultPolicy(), func() time.Time { return now })
	q.OpenAudio(2, 20*time.Millisecond)
	var err error
	for i := 0; i < 1500 && err == nil; i++ {
		now = now.Add(20 * time.Millisecond)
		err = q.PushAudio(2, frame(t, rxv1.FrameAudio, rxv1.CodecOpus, uint32(i), 10))
		if i%5 == 0 {
			for range 3 {
				drain1(t, q)
			}
		}
	}
	if !errors.Is(err, sendq.ErrSlowConsumer) {
		t.Fatalf("err = %v, want slow consumer", err)
	}
	if el := now.Sub(time.Unix(0, 0)); el < 10*time.Second || el > 12*time.Second {
		t.Fatalf("closed after %v, want ≈ 10.5 s", el)
	}
}

func drain1(t *testing.T, q *sendq.Queue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, _ = q.Next(ctx)
}

func TestJSONCap(t *testing.T) {
	q := sendq.New(sendq.DefaultPolicy(), nil)
	chunk := make([]byte, 64<<10)
	var err error
	for i := 0; i < 17 && err == nil; i++ {
		err = q.PushJSON(chunk)
	}
	if !errors.Is(err, sendq.ErrSlowConsumer) {
		t.Fatalf("err = %v", err)
	}
}

func TestTotalCap(t *testing.T) {
	p := sendq.DefaultPolicy()
	p.MaxTotalBytes = 1000
	q := sendq.New(p, nil)
	q.OpenAudio(2, 20*time.Millisecond)
	var err error
	for i := 0; i < 20 && err == nil; i++ {
		err = q.PushAudio(2, frame(t, rxv1.FrameAudio, rxv1.CodecOpus, uint32(i), 100))
	}
	if !errors.Is(err, sendq.ErrSlowConsumer) {
		t.Fatalf("err = %v", err)
	}
}

func TestMeterCoalescing(t *testing.T) {
	q := sendq.New(sendq.DefaultPolicy(), nil)
	_ = q.PushMeter("a", []byte("a1"))
	_ = q.PushMeter("b", []byte("b1"))
	_ = q.PushMeter("a", []byte("a2"))
	items := drain(t, q)
	if len(items) != 2 || string(items[0].Data) != "a2" || string(items[1].Data) != "b1" {
		t.Fatalf("items = %q", items)
	}
	if q.Stats().MeterCoalesced != 1 {
		t.Errorf("coalesced = %d", q.Stats().MeterCoalesced)
	}
}
