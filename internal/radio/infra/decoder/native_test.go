package decoder

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/dsp/dsptest"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// textEvents collects the output of a text session.
type textEvents struct {
	mu      sync.Mutex
	lines   []app.DecodeRecord
	partial int
	frames  []app.SpectrumFrame
	states  []app.DecoderStatus
}

func (e *textEvents) events() app.DecoderEvents {
	return app.DecoderEvents{
		Decode: func(r app.DecodeRecord) {
			e.mu.Lock()
			defer e.mu.Unlock()

			if r.Partial {
				e.partial++

				return
			}

			e.lines = append(e.lines, r)
		},
		Status: func(s app.DecoderStatus) {
			e.mu.Lock()
			e.states = append(e.states, s)
			e.mu.Unlock()
		},
		Spectrum: func(f app.SpectrumFrame) {
			e.mu.Lock()
			e.frames = append(e.frames, app.SpectrumFrame{Payload: append([]byte(nil), f.Payload...), TimestampUS: f.TimestampUS})
			e.mu.Unlock()
		},
	}
}

// wait polls until ok holds (the session runs in its own goroutine).
func (e *textEvents) wait(t *testing.T, what string, ok func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for {
		e.mu.Lock()
		done := ok()
		e.mu.Unlock()

		if done {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func textMode(t *testing.T, name string) domain.DigitalMode {
	t.Helper()

	m, err := domain.DigitalModeOf(name)
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// TestTextModesCatalogue: every native text mode of the catalogue (IQ
// input) has a chain, reads the selector IQ at the decoder rate and has a
// selector band.
func TestTextModesCatalogue(t *testing.T) {
	native := 0

	for _, m := range domain.DigitalModes() {
		if m.Cap != domain.CapNativeDSP || m.Input != domain.InputNarrowIQ {
			continue
		}

		native++

		cfg, ok := textModes[m.Name]
		if !ok || m.Input != domain.InputNarrowIQ || m.InputRate != dsp.TextRate || m.BandwidthHz <= 0 || !m.SecondaryFFT {
			t.Errorf("%s: %+v", m.Name, m)

			continue
		}

		cfg.BandwidthHz = m.BandwidthHz
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: %v", m.Name, err)
		}
	}

	if native != len(textModes) {
		t.Errorf("%d native modes in the catalogue, %d chains", native, len(textModes))
	}
}

// TestTextSession decodes RTTY from the selector IQ of a demodulator at a
// channel rate (the signal 600 Hz above the offset given at start, moved
// onto it), with the secondary FFT on: lines to the listener and the hub,
// partial lines while printing, the secondary FFT lines, running status.
func TestTextSession(t *testing.T) {
	const rate = 24094.117647

	r := NewRunner(Options{Text: func() TextSettings { return TextSettings{FFTSize: 1024} }})
	ev := &textEvents{}

	run, err := r.Start(app.DecoderSpec{Session: shared.MustParseUUID("01890a5d-ac96-7a3b-8000-000000000001"), Mode: textMode(t, "rtty170"), OffsetHz: 1500}, ev.events())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	if run.SpectrumSize() != 1024 {
		t.Fatalf("spectrum size %d", run.SpectrumSize())
	}

	run.Retune(2100)
	run.Spectrum(9)

	iq := dsptest.RTTY("RYRYRY CQ CQ DE F4TEST F4TEST K\r\nTEST LINE TWO\r\n", 45.45, 170, false, rate, 2100)
	at := time.Unix(1_800_000_000, 0)

	for i := 0; i < len(iq); i += 2400 {
		run.IQ(app.IQBlock{Samples: iq[i:min(i+2400, len(iq))], Rate: rate, Time: at.Add(time.Duration(float64(i) / rate * float64(time.Second)))})
		// The queue holds two seconds: wait for the session to take it.
		s := run.(*textSession)
		ev.wait(t, "the input taken", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()

			return s.queued == 0
		})
	}

	ev.wait(t, "two lines", func() bool { return len(ev.lines) >= 2 })

	ev.mu.Lock()
	defer ev.mu.Unlock()

	if !strings.Contains(ev.lines[0].Text, "CQ DE F4TEST F4TEST K") || ev.lines[1].Text != "TEST LINE TWO" {
		t.Fatalf("lines %+v", ev.lines)
	}

	var p TextRecord
	if err := json.Unmarshal(ev.lines[1].Payload, &p); err != nil || p.Text != "TEST LINE TWO" || ev.lines[1].Schema != TextSchema || ev.lines[1].Time.Before(at) {
		t.Fatalf("record %+v %v", ev.lines[1], err)
	}

	if ev.partial == 0 {
		t.Error("no partial line")
	}

	if len(ev.frames) == 0 || len(ev.frames[0].Payload) != 8+1024 {
		t.Fatalf("%d secondary fft lines", len(ev.frames))
	}

	if len(ev.states) != 1 || ev.states[0].State != app.DecoderRunning {
		t.Fatalf("states %+v", ev.states)
	}
}

// TestTextSessionQueue: a session that falls behind drops the oldest
// input; a closed session reports nothing.
func TestTextSessionQueue(t *testing.T) {
	q := &textSession{wake: make(chan struct{}, 1), done: make(chan struct{})}
	block := make([]complex64, 24000)

	for range 5 {
		q.IQ(app.IQBlock{Samples: block, Rate: 24000})
	}

	if q.queued != 2*24000 || len(q.queue) != 2 || !q.queue[0].gap {
		t.Fatalf("queued %d samples in %d blocks", q.queued, len(q.queue))
	}

	// A failed session drops its input.
	q.fail()
	q.IQ(app.IQBlock{Samples: block, Rate: 24000})

	if q.queued != 0 {
		t.Fatalf("failed session queued %d samples", q.queued)
	}

	r := NewRunner(Options{})
	ev := &textEvents{}

	run, err := r.Start(app.DecoderSpec{Session: shared.MustParseUUID("01890a5d-ac96-7a3b-8000-000000000002"), Mode: textMode(t, "bpsk31"), OffsetHz: 1000}, ev.events())
	if err != nil {
		t.Fatal(err)
	}

	s := run.(*textSession)

	run.Close()
	run.IQ(app.IQBlock{Samples: block, Rate: 24000})
	s.emit(app.DecodeRecord{Text: "late"})
	s.status(app.DecoderStatus{State: app.DecoderError})

	ev.mu.Lock()
	defer ev.mu.Unlock()

	if len(ev.lines) != 0 || len(ev.states) != 0 {
		t.Fatalf("closed session reported %+v %+v", ev.lines, ev.states)
	}
}

func TestLineAssembler(t *testing.T) {
	var (
		got   []app.DecodeRecord
		clock = time.Unix(1_800_000_000, 0)
	)

	l := &lineAssembler{emit: func(r app.DecodeRecord) { got = append(got, r) }, now: func() time.Time { return clock }}
	at := time.Unix(1_700_000_000, 0)

	// Leading spaces and control characters are dropped; a line break
	// ends the line; the next one shows as a partial line.
	l.feed([]byte("  CQ\x00 CQ\a\r\n\r\nDE"), at)

	if len(got) != 2 || got[0].Partial || got[0].Text != "CQ CQ" || !got[0].Time.Equal(at) || !got[1].Partial || got[1].Text != "DE" {
		t.Fatalf("records %+v", got)
	}

	// The partial line is throttled; idle ends it.
	l.feed([]byte(" F4TEST"), at)

	if len(got) != 2 {
		t.Fatalf("partial not throttled: %+v", got)
	}

	clock = clock.Add(LineIdle)
	l.idle()

	if len(got) != 3 || got[2].Text != "DE F4TEST" || got[2].Partial {
		t.Fatalf("idle %+v", got)
	}

	// A long line is cut at its last space.
	got = nil
	clock = clock.Add(time.Second)

	l.feed([]byte(strings.Repeat("ABCD ", 17)), at)

	if len(got) != 2 || got[0].Text != strings.TrimSpace(strings.Repeat("ABCD ", 16)) || string(l.line) != "ABCD " {
		t.Fatalf("wrap %+v, rest %q", got, l.line)
	}
}

// TestTextSessionGap: lost input ends the line being printed.
func TestTextSessionGap(t *testing.T) {
	const rate = 24000.0

	r := NewRunner(Options{})
	ev := &textEvents{}

	run, err := r.Start(app.DecoderSpec{Session: shared.MustParseUUID("01890a5d-ac96-7a3b-8000-000000000003"), Mode: textMode(t, "rtty170"), OffsetHz: 1500}, ev.events())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	s := run.(*textSession)
	iq := dsptest.RTTY("RYRYRY CQ CQ DE F4TEST F4TEST", 45.45, 170, false, rate, 1500)

	feed := func(b []complex64, gap bool) {
		run.IQ(app.IQBlock{Samples: b, Rate: rate, Discontinuity: gap})
		ev.wait(t, "the input taken", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()

			return s.queued == 0
		})
	}

	for i := 0; i < len(iq); i += 2400 {
		feed(iq[i:min(i+2400, len(iq))], false)
	}

	ev.mu.Lock()
	lines := len(ev.lines)
	ev.mu.Unlock()

	if lines != 0 {
		t.Fatalf("%d lines before the gap", lines)
	}

	feed(make([]complex64, 2400), true)

	ev.wait(t, "the line ended by the gap", func() bool {
		return len(ev.lines) == 1 && strings.Contains(ev.lines[0].Text, "CQ DE F4TEST")
	})
}
