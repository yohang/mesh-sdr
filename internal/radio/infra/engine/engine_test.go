package engine

import (
	"errors"
	"log/slog"
	"math"
	"math/cmplx"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
)

const rate = 250_000

func tuning() domain.Tuning {
	return domain.NewTuning(domain.MustFrequency(145_000_000), domain.MustSampleRate(rate))
}

// feed pushes d of a carrier at offset Hz, paced at about real time (the
// rings hold 250 ms).
func feed(e *Engine, from uint64, d time.Duration, offset float64) uint64 {
	n := uint64(d.Seconds() * rate)
	block := make([]complex64, 5000)
	t0 := time.Now()

	for i := uint64(0); i < n; i += uint64(len(block)) {
		for k := range block {
			block[k] = complex64(cmplx.Rect(0.3, 2*math.Pi*offset*float64(from+i+uint64(k))/rate))
		}

		e.Samples(from+i, t0.Add(time.Duration(i)*time.Second/rate), block)
		time.Sleep(time.Duration(len(block)) * time.Second / rate)
	}

	return from + n
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5 s")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func params() app.DemodParams {
	return app.DemodParams{Mode: "nfm", OffsetHz: 30_000, LowHz: -5000, HighHz: 5000, OutputRate: 12000, Codec: app.CodecADPCM}
}

func TestSpectrumAndDemodLifecycle(t *testing.T) {
	e := New(slog.New(slog.DiscardHandler))
	defer e.Close()

	var frames, audio, meters atomic.Int64

	cancel := e.SubscribeSpectrum(func(f app.SpectrumFrame) {
		if len(f.Payload) == 8+4096 {
			frames.Add(1)
		}
	})

	// Demodulators may be created before the source runs.
	d, err := e.NewDemod(params(), func(a app.AudioOut) {
		if len(a.Payload) == 4+120 && a.Codec == app.CodecADPCM {
			audio.Add(1)
		}
	}, func(app.Meter) { meters.Add(1) })
	if err != nil {
		t.Fatal(err)
	}

	e.Start(tuning())

	if info := e.Spectrum(); info.StartHz != 145_000_000-rate/2 || info.SpanHz != rate {
		t.Fatalf("spectrum %+v", info)
	}

	next := feed(e, 0, 500*time.Millisecond, 30_000)

	eventually(t, func() bool { return frames.Load() > 0 && audio.Load() >= 10 && meters.Load() > 0 })

	// Retune within the band, then out of it.
	p := params()
	p.OffsetHz = -20_000

	if err := d.Set(p); err != nil || d.Params().OffsetHz != -20_000 {
		t.Fatal(err)
	}

	p.OffsetHz = 200_000
	if err := d.Set(p); !errors.Is(err, domain.ErrOutOfRange) {
		t.Fatal(err)
	}

	p.Mode = "dmr"
	if err := d.Set(p); !errors.Is(err, domain.ErrUnsupportedMode) {
		t.Fatal(err)
	}

	// Broadcast FM needs the HD audio path.
	p.Mode, p.OffsetHz = "wfm", -20_000
	if err := d.Set(p); !errors.Is(err, domain.ErrOutOfRange) {
		t.Fatal(err)
	}

	// A new run rebinds the demodulator.
	e.Stop()
	e.Start(tuning())

	before := audio.Load()
	feed(e, next, 300*time.Millisecond, -20_000)

	eventually(t, func() bool { return audio.Load() > before+5 })

	cancel()
	d.Close()
	d.Close()

	e.mu.Lock()
	ep := e.ep
	e.mu.Unlock()

	if ep.spectrumCancel != nil || ep.channelizerCancel != nil {
		t.Fatal("goroutines still wanted without subscribers")
	}
}

func TestDemodValidation(t *testing.T) {
	e := New(slog.New(slog.DiscardHandler))
	defer e.Close()

	nop := func(app.AudioOut) {}
	nom := func(app.Meter) {}

	bad := []func(*app.DemodParams){
		func(p *app.DemodParams) { p.Mode = "dmr" },
		func(p *app.DemodParams) { p.OutputRate = 9000 },
		func(p *app.DemodParams) { p.Codec = "opus" },
		func(p *app.DemodParams) { p.LowHz, p.HighHz = 5000, -5000 },
		func(p *app.DemodParams) { v := 10.0; p.SquelchDB = &v },
		func(p *app.DemodParams) { p.NR = app.NR{Enabled: true, ThresholdDB: 25} },
		func(p *app.DemodParams) { p.LowHz, p.HighHz = 9950, 10_050 },
		func(p *app.DemodParams) { p.HighHz = math.NaN() },
		func(p *app.DemodParams) { p.Mode = "wfm" },
	}

	for i, mutate := range bad {
		p := params()
		mutate(&p)

		if _, err := e.NewDemod(p, nop, nom); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}

	// Before the device runs, offsets and bands are checked against its
	// tuning, and the spectrum geometry is known.
	e.SetTuning(tuning())

	if info := e.Spectrum(); info.SpanHz != rate {
		t.Fatalf("spectrum before start %+v", info)
	}

	// Pass band edges are clamped to the mode limits.
	wide := params()
	wide.LowHz, wide.HighHz = -20_000, 20_000

	dw, err := e.NewDemod(wide, nop, nom)
	if err != nil {
		t.Fatal(err)
	}

	if got := dw.Params(); got.LowHz != -10_000 || got.HighHz != 10_000 {
		t.Fatalf("clamped band %g..%g", got.LowHz, got.HighHz)
	}

	dw.Close()

	d, err := e.NewDemod(params(), nop, nom)
	if err != nil {
		t.Fatal(err)
	}

	far := params()
	far.OffsetHz = 400_000

	if err := d.Set(far); !errors.Is(err, domain.ErrOutOfRange) {
		t.Fatalf("offset outside the band before start: %v", err)
	}

	d.Close()

	e.Start(tuning())

	p := params()
	p.OffsetHz = 500_000

	if _, err := e.NewDemod(p, nop, nom); !errors.Is(err, domain.ErrOutOfRange) {
		t.Fatal(err)
	}
}

// TestModeSwitch changes the mode of a running demodulator: default pass
// bands, the wide channel of broadcast FM on the HD path, NR.
func TestModeSwitch(t *testing.T) {
	e := New(slog.New(slog.DiscardHandler))
	defer e.Close()

	var audio atomic.Int64

	e.Start(tuning())

	p := params()
	p.LowHz, p.HighHz = 0, 0

	d, err := e.NewDemod(p, func(app.AudioOut) { audio.Add(1) }, func(app.Meter) {})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if got := d.Params(); got.LowHz != -4000 || got.HighHz != 4000 {
		t.Fatalf("nfm default band %g..%g", got.LowHz, got.HighHz)
	}

	next := feed(e, 0, 200*time.Millisecond, 30_000)
	eventually(t, func() bool { return audio.Load() > 3 })

	for _, mode := range []struct {
		name      string
		rate      int
		low, high float64
	}{
		{"usb", 12000, 300, 2700},
		{"cw", 12000, 650, 950},
		{"wfm", 48000, -75_000, 75_000},
		{"sam", 12000, -4000, 4000},
	} {
		p := d.Params()
		p.Mode, p.LowHz, p.HighHz, p.OutputRate = mode.name, 0, 0, mode.rate
		p.NR = app.NR{Enabled: true, ThresholdDB: 6}

		if err := d.Set(p); err != nil {
			t.Fatalf("%s: %v", mode.name, err)
		}

		if got := d.Params(); got.LowHz != mode.low || got.HighHz != mode.high || !got.NR.Enabled {
			t.Fatalf("%s: applied %+v", mode.name, got)
		}

		before := audio.Load()
		next = feed(e, next, 200*time.Millisecond, 30_000)

		eventually(t, func() bool { return audio.Load() > before+3 })
	}

	if got := Modes(); len(got) != 7 || got[0] != "am" {
		t.Fatalf("modes %v", got)
	}
}
