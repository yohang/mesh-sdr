package sendq

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newQueue(t *testing.T) (*Queue, *clock, *[]string) {
	t.Helper()

	c := &clock{t: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
	updates := &[]string{}
	q := New(DefaultConfig(), c.now, func(id uint16, fps int) []byte {
		s := fmt.Sprintf("update %d %d", id, fps)
		*updates = append(*updates, s)

		return []byte(s)
	})

	return q, c, updates
}

func audio(id uint16, n int) Frame {
	return Frame{Type: rxv1.FrameAudio, Codec: rxv1.CodecPCMS16LE, StreamID: id, Payload: make([]byte, n), Duration: 20 * time.Millisecond}
}

func fft(id uint16) Frame {
	return Frame{Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB, StreamID: id, Payload: make([]byte, 100)}
}

func TestPriorityAndNumbering(t *testing.T) {
	q, _, _ := newQueue(t)
	q.ConfigureFFT(1, 30, false)

	if err := q.PushFFT(fft(1)); err != nil {
		t.Fatal(err)
	}

	q.PushMeter("d1", []byte("meter"))

	if err := q.PushAudio(audio(2, 10)); err != nil {
		t.Fatal(err)
	}

	if err := q.PushJSON([]byte("json")); err != nil {
		t.Fatal(err)
	}

	want := []string{"json", "audio", "meter", "fft"}
	for i, w := range want {
		it, ok := q.Pop()
		if !ok {
			t.Fatalf("pop %d: empty", i)
		}

		got := string(it.Text)
		if it.Binary() {
			h, payload, err := rxv1.ParseFrame(append(append([]byte{}, it.Header...), it.Payload...))
			if err != nil || h.Seq != 0 {
				t.Fatalf("frame %d: %+v %v", i, h, err)
			}

			got = map[rxv1.FrameType]string{rxv1.FrameAudio: "audio", rxv1.FrameFFT: "fft"}[h.Type]
			_ = payload
		}

		if got != w {
			t.Fatalf("pop %d = %q, want %q", i, got, w)
		}
	}

	if _, ok := q.Pop(); ok || q.Pending() != 0 {
		t.Fatalf("queue not empty: %d bytes", q.Pending())
	}

	if err := q.PushAudio(Frame{Type: rxv1.FrameAudio, Codec: rxv1.CodecFFTU8DB}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatal(err)
	}
}

func TestMeterCoalesced(t *testing.T) {
	q, _, _ := newQueue(t)
	q.PushMeter("d1", []byte("a"))
	q.PushMeter("d1", []byte("bb"))
	q.PushMeter("d2", []byte("c"))

	it, _ := q.Pop()
	it2, _ := q.Pop()

	if string(it.Text) != "bb" || string(it2.Text) != "c" || q.Pending() != 0 {
		t.Fatalf("got %q %q", it.Text, it2.Text)
	}
}

func TestAudioDropsOldestAndFlagsDiscontinuity(t *testing.T) {
	q, c, _ := newQueue(t)

	// 30 frames of 20 ms: 600 ms > 500 ms cap → 5 oldest dropped.
	for range 30 {
		if err := q.PushAudio(audio(3, 10)); err != nil {
			t.Fatal(err)
		}

		c.advance(time.Millisecond)
	}

	n := 0

	for {
		it, ok := q.Pop()
		if !ok {
			break
		}

		h, _, _ := rxv1.ParseFrame(append(append([]byte{}, it.Header...), it.Payload...))
		if n == 0 && !h.Flags.Has(rxv1.FlagDiscontinuity) {
			t.Fatal("first frame after drops lacks discontinuity")
		}

		if n > 0 && h.Flags.Has(rxv1.FlagDiscontinuity) {
			t.Fatal("discontinuity repeated")
		}

		n++
	}

	if n != 25 {
		t.Fatalf("kept %d frames", n)
	}
}

func TestAudioSustainedOverflowCloses(t *testing.T) {
	q, c, _ := newQueue(t)

	var err error

	for i := 0; i < 2000 && err == nil; i++ {
		err = q.PushAudio(audio(3, 10))
		c.advance(20 * time.Millisecond)
	}

	if !errors.Is(err, ErrSlowConsumer) || !errors.Is(q.Err(), ErrSlowConsumer) {
		t.Fatalf("err = %v", err)
	}

	// Drops with quiet pauses of ≥ 1 s never close.
	q, c, _ = newQueue(t)

	for range 30 {
		for range 30 {
			if err := q.PushAudio(audio(3, 10)); err != nil {
				t.Fatal(err)
			}
		}

		for {
			if _, ok := q.Pop(); !ok {
				break
			}
		}

		c.advance(1500 * time.Millisecond)
	}
}

func TestJSONCap(t *testing.T) {
	q, _, _ := newQueue(t)
	big := make([]byte, 600<<10)

	if err := q.PushJSON(big); err != nil {
		t.Fatal(err)
	}

	if err := q.PushJSON(big); !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("err = %v", err)
	}
}

func TestTotalCap(t *testing.T) {
	q, _, _ := newQueue(t)
	q.cfg.AudioCap = time.Hour

	var err error
	for i := 0; i < 200 && err == nil; i++ {
		err = q.PushAudio(audio(3, 60_000))
	}

	if !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("err = %v", err)
	}
}

func TestFFTLatestWinsAndRateDecimation(t *testing.T) {
	q, c, _ := newQueue(t)
	q.ConfigureFFT(1, 10, false)

	// 30 frames offered at 30 fps over 1 s, nothing popped: at 10 fps, 10
	// accepted, 2 kept.
	for range 30 {
		if err := q.PushFFT(fft(1)); err != nil {
			t.Fatal(err)
		}

		c.advance(time.Second / 30)
	}

	n := 0

	for {
		if _, ok := q.Pop(); !ok {
			break
		}

		n++
	}

	if n != 2 {
		t.Fatalf("kept %d FFT frames", n)
	}
}

func TestFFTHalvesUnderDropsAndRecovers(t *testing.T) {
	q, c, updates := newQueue(t)
	q.ConfigureFFT(1, 8, false)

	// A consumer that never reads: ≥ 25 % drops over 5 s → 4 fps.
	for range 6 * 8 {
		_ = q.PushFFT(fft(1))
		c.advance(time.Second / 8)
	}

	if q.FPS(1) != 4 || len(*updates) == 0 || (*updates)[0] != "update 1 4" {
		t.Fatalf("fps %d, updates %v", q.FPS(1), *updates)
	}

	// stream.update is queued as JSON, before the frames.
	if it, _ := q.Pop(); string(it.Text) != "update 1 4" {
		t.Fatalf("first item %q", it.Text)
	}

	// A fast consumer for 40 s: back to 8 fps.
	for range 40 * 4 {
		_ = q.PushFFT(fft(1))

		for {
			if _, ok := q.Pop(); !ok {
				break
			}
		}

		c.advance(time.Second / 4)
	}

	if q.FPS(1) != 8 {
		t.Fatalf("fps %d after recovery, updates %v", q.FPS(1), *updates)
	}
}

func TestPausedAndRemovedStreams(t *testing.T) {
	q, c, _ := newQueue(t)
	q.ConfigureFFT(1, 10, false)
	_ = q.PushFFT(fft(1))
	q.ConfigureFFT(1, 10, true)
	c.advance(time.Second)
	_ = q.PushFFT(fft(1))

	if _, ok := q.Pop(); ok || q.Pending() != 0 {
		t.Fatal("paused stream still queued")
	}

	_ = q.PushAudio(audio(5, 10))
	q.RemoveStream(5)

	if _, ok := q.Pop(); ok || q.Pending() != 0 {
		t.Fatal("removed stream still queued")
	}
}
