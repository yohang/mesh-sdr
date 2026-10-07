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
	return app.DemodParams{Mode: ModeNFM, OffsetHz: 30_000, LowHz: -5000, HighHz: 5000, OutputRate: 12000, Codec: app.CodecADPCM}
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

	p.Mode = "wfm"
	if err := d.Set(p); !errors.Is(err, domain.ErrUnsupportedMode) {
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
		func(p *app.DemodParams) { p.Mode = "am" },
		func(p *app.DemodParams) { p.OutputRate = 9000 },
		func(p *app.DemodParams) { p.Codec = "opus" },
		func(p *app.DemodParams) { p.LowHz, p.HighHz = 5000, -5000 },
		func(p *app.DemodParams) { v := 10.0; p.SquelchDB = &v },
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

	wide := params()
	wide.LowHz, wide.HighHz = -20_000, 20_000

	if _, err := e.NewDemod(wide, nop, nom); !errors.Is(err, domain.ErrOutOfRange) {
		t.Fatalf("band wider than the channel before start: %v", err)
	}

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
