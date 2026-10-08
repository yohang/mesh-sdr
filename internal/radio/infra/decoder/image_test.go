package decoder

import (
	"bytes"
	"encoding/json"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp/dsptest"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// imageEvents records the output of an image session.
type imageEvents struct {
	mu     sync.Mutex
	recs   []app.DecodeRecord
	files  []app.ProducedFile
	status []string
	ended  chan struct{}
}

func newImageEvents() *imageEvents { return &imageEvents{ended: make(chan struct{}, 8)} }

func (e *imageEvents) events() app.DecoderEvents {
	return app.DecoderEvents{
		Decode: func(r app.DecodeRecord) {
			e.mu.Lock()
			e.recs = append(e.recs, r)
			e.mu.Unlock()

			var p ImageRecord
			if json.Unmarshal(r.Payload, &p) == nil && p.Event == "end" {
				e.ended <- struct{}{}
			}
		},
		Status: func(s app.DecoderStatus) {
			e.mu.Lock()
			e.status = append(e.status, s.State)
			e.mu.Unlock()
		},
		File: func(f app.ProducedFile) error {
			e.mu.Lock()
			e.files = append(e.files, f)
			e.mu.Unlock()

			return nil
		},
	}
}

// payloads returns the image.v1 payloads, by event.
func (e *imageEvents) payloads(t *testing.T) map[string][]ImageRecord {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()

	out := map[string][]ImageRecord{}

	for _, r := range e.recs {
		var p ImageRecord
		if err := json.Unmarshal(r.Payload, &p); err != nil || r.Schema != ImageSchema {
			t.Fatalf("record %+v: %v", r, err)
		}

		if live := p.Event != "start"; r.Live != live {
			t.Errorf("%s record live = %v", p.Event, r.Live)
		}

		out[p.Event] = append(out[p.Event], p)
	}

	return out
}

func (e *imageEvents) waitEnd(t *testing.T) {
	t.Helper()

	select {
	case <-e.ended:
	case <-time.After(30 * time.Second):
		t.Fatal("no image end")
	}
}

// countRows counts the rows of rows messages.
func countRows(msgs []ImageRecord) int {
	n := 0
	for _, m := range msgs {
		n += m.Count
	}

	return n
}

// startImage starts a session of mode and feeds it audio at rate in 20 ms
// blocks.
func startImage(t *testing.T, r *Runner, mode string, ev *imageEvents, audio []float32, rate int) app.DecoderRun {
	t.Helper()

	m, err := domain.DigitalModeOf(mode)
	if err != nil {
		t.Fatal(err)
	}

	run, err := r.Start(app.DecoderSpec{Session: shared.MustParseUUID("01890a5d-ac96-774b-bcce-b302099a8057"), Mode: m}, ev.events())
	if err != nil {
		t.Fatal(err)
	}

	at := time.Unix(1_800_000_000, 0)
	block := rate / 50

	for off := 0; off < len(audio); off += block {
		b := audio[off:min(off+block, len(audio))]
		run.Audio(app.AudioBlock{Samples: b, Rate: rate, Time: at.Add(time.Duration(off) * time.Second / time.Duration(rate))})
		// Faster than real time: wait for the queue to drain, a full one
		// drops blocks.
		for len(run.(*imageSession).in) > imageQueue/2 {
			time.Sleep(time.Millisecond)
		}
	}

	return run
}

// An SSTV picture is streamed row by row and saved as PNG with its
// metadata (DEC-037, FIL-008).
func TestImageSSTV(t *testing.T) {
	r := NewRunner(Options{})
	ev := newImageEvents()

	audio := dsptest.SSTV(dsptest.Robot36, 12000, 240, func(x, _ int) (uint8, uint8, uint8) { return uint8(x / 2), 128, 64 })
	run := startImage(t, r, "sstv", ev, audio, 12000)
	ev.waitEnd(t)
	run.Close()

	p := ev.payloads(t)
	if len(p["start"]) != 1 || p["start"][0].SSTVMode != "Robot 36" || *p["start"][0].VISCode != 8 || countRows(p["rows"]) != 240 {
		t.Fatalf("%d starts (%+v), %d rows", len(p["start"]), p["start"], countRows(p["rows"]))
	}

	if rows := p["rows"][1]; rows.Width != 320 || rows.Channels != 3 || len(rows.Pixels) != rows.Count*960 || *rows.Row != *p["rows"][0].Row+p["rows"][0].Count {
		t.Errorf("rows %+v", rows)
	}

	if end := p["end"]; len(end) != 1 || end[0].Lines != 240 || !end[0].Complete || !end[0].Sent || end[0].Short {
		t.Errorf("end %+v", end)
	}

	ev.mu.Lock()
	defer ev.mu.Unlock()

	if ev.status[0] != app.DecoderRunning || len(ev.files) != 1 || ev.recs[0].Text != "Robot 36 (VIS 8), 320×240" {
		t.Fatalf("status %v, %d files, start %q", ev.status, len(ev.files), ev.recs[0].Text)
	}

	f := ev.files[0]
	if f.Kind != "sstv" || f.Start.IsZero() || !f.End.After(f.Start) || f.Metadata["vis_code"] != 8 || f.Metadata["sstv_mode"] != "Robot 36" ||
		f.Metadata["lines_received"] != 240 || f.Metadata["lines_total"] != 240 || f.Metadata["complete"] != true {
		t.Errorf("file %+v", f)
	}

	img, err := png.Decode(bytes.NewReader(f.Data))
	if err != nil {
		t.Fatal(err)
	}

	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 240 {
		t.Errorf("PNG bounds %v", b)
	}
}

// An SSTV picture cut short under half its height is discarded when the
// session ends.
func TestImageSSTVShort(t *testing.T) {
	r := NewRunner(Options{})
	ev := newImageEvents()

	audio := dsptest.SSTV(dsptest.Robot36, 12000, 60, func(int, int) (uint8, uint8, uint8) { return 200, 200, 200 })
	startImage(t, r, "sstv", ev, audio, 12000).Close()
	ev.waitEnd(t)

	p := ev.payloads(t)
	if end := p["end"]; len(end) != 1 || end[0].Complete || end[0].Sent || !end[0].Short || end[0].Lines < 55 || end[0].Lines >= 120 {
		t.Errorf("end %+v", end)
	}

	ev.mu.Lock()
	defer ev.mu.Unlock()

	if len(ev.files) != 0 {
		t.Errorf("%d files", len(ev.files))
	}
}

// A FAX page with the settings of the hub: shorter than fax_min_length,
// it is discarded; otherwise it is saved in grey (DEC-038).
func TestImageFAX(t *testing.T) {
	for _, minLength := range []int{30, 50} {
		r := NewRunner(Options{FAX: func() FAXSettings { return FAXSettings{LPM: 120, MinLength: minLength, MaxLength: 500} }})
		ev := newImageEvents()

		audio := dsptest.FAX(12000, 120, 40, func(x, _ int) uint8 { return uint8(x % 256) })
		run := startImage(t, r, "fax", ev, audio, 12000)
		ev.waitEnd(t)
		run.Close()

		p := ev.payloads(t)
		if s := p["start"]; len(s) != 1 || s[0].IOC != 576 || s[0].LPM != 120 || s[0].Width != 1812 || s[0].Channels != 1 {
			t.Fatalf("start %+v", s)
		}

		saved := minLength < 40

		if end := p["end"]; len(end) != 1 || end[0].Lines != 40 || !end[0].Complete || end[0].Sent != saved || end[0].Short == saved {
			t.Errorf("min %d: end %+v", minLength, end)
		}

		ev.mu.Lock()

		if saved && (len(ev.files) != 1 || ev.files[0].Kind != "fax" || ev.files[0].Metadata["lpm"] != 120 || ev.files[0].Metadata["ioc"] != 576) || !saved && len(ev.files) != 0 {
			t.Errorf("min %d: files %+v", minLength, ev.files)
		}

		ev.mu.Unlock()
	}
}
