package engine

import (
	"encoding/binary"
	"errors"
	"log/slog"
	"math"
	"math/cmplx"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
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
		{"usbd", 48000, 0, 24_000},
		{"lsbd", 48000, -24_000, 0},
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

	if got := Modes(); len(got) != 9 || got[0] != "am" {
		t.Fatalf("modes %v", got)
	}
}

// The DATA modes have their own capabilities (DEM-013).
func TestCapabilities(t *testing.T) {
	got := Capabilities()
	if len(got) != 3 || got[0].Cap != AnalogCap || strings.Join(got[0].Modes, ",") != "am,sam,nfm,usb,lsb,cw,wfm" ||
		got[1].Cap != "cap:usbd" || strings.Join(got[1].Modes, ",") != "usbd" || got[2].Cap != "cap:lsbd" {
		t.Errorf("capabilities = %+v", got)
	}
}

// A tap receives the demodulated audio at the output rate until cancelled
// (decoder sessions, DEC-002).
func TestTap(t *testing.T) {
	e := New(slog.New(slog.DiscardHandler))
	defer e.Close()

	e.Start(tuning())

	d, err := e.NewDemod(params(), func(app.AudioOut) {}, func(app.Meter) {})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var (
		samples atomic.Int64
		tapRate atomic.Int64
	)

	cancel := d.Tap(func(b app.AudioBlock) {
		samples.Add(int64(len(b.Samples)))
		tapRate.Store(int64(b.Rate))
	})

	next := feed(e, 0, 300*time.Millisecond, 30_000)
	eventually(t, func() bool { return samples.Load() > 1000 })

	if tapRate.Load() != int64(params().OutputRate) {
		t.Errorf("tap rate %d", tapRate.Load())
	}

	cancel()

	n := samples.Load()
	feed(e, next, 100*time.Millisecond, 30_000)

	time.Sleep(50 * time.Millisecond)

	if samples.Load() != n {
		t.Error("audio after cancel")
	}
}

// feedFM pushes d of a broadcast FM carrier at offset Hz modulated by a
// toneHz tone (deviation dev), paced at about real time.
func feedFM(e *Engine, from uint64, d time.Duration, offset, toneHz, dev float64) uint64 {
	n := uint64(d.Seconds() * rate)
	block := make([]complex64, 5000)
	t0 := time.Now()

	for i := uint64(0); i < n; i += uint64(len(block)) {
		for k := range block {
			t := float64(from+i+uint64(k)) / rate
			block[k] = complex64(cmplx.Rect(0.3, 2*math.Pi*offset*t+dev/toneHz*math.Sin(2*math.Pi*toneHz*t)))
		}

		e.Samples(from+i, t0.Add(time.Duration(i)*time.Second/rate), block)
		time.Sleep(time.Duration(len(block)) * time.Second / rate)
	}

	return from + n
}

// TestDeemphasisLive changes the hub's WFM de-emphasis under a running
// WFM demodulator: a 10 kHz tone comes out quieter at 75 µs than at 50 µs,
// without a new chain.
func TestDeemphasisLive(t *testing.T) {
	var us atomic.Int64
	us.Store(50)

	e := Factory{Logger: slog.New(slog.DiscardHandler), Deemphasis: func() int { return int(us.Load()) }}.New(shared.MustDeviceID("vhf")).(*Engine)
	defer e.Close()

	var (
		mu     sync.Mutex
		levels []float64
		skip   int
	)

	e.Start(tuning())

	p := app.DemodParams{Mode: "wfm", OffsetHz: 30_000, OutputRate: 48000, Codec: app.CodecPCM}

	d, err := e.NewDemod(p, func(a app.AudioOut) {
		var s float64

		for i := 0; i+1 < len(a.Payload); i += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(a.Payload[i:])))
			s += v * v
		}

		// A slow machine may make the engine drop samples (a real overrun):
		// the frames after a discontinuity carry the click, not the tone.
		mu.Lock()
		if a.Discontinuity {
			skip = 2
		}

		if skip > 0 {
			skip--
		} else {
			levels = append(levels, math.Sqrt(s/float64(max(len(a.Payload)/2, 1))))
		}
		mu.Unlock()
	}, func(app.Meter) {})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// level feeds 400 ms and returns the median level of the last clean
	// frames.
	next := uint64(0)
	level := func() float64 {
		mu.Lock()
		before := len(levels)
		mu.Unlock()

		next = feedFM(e, next, 400*time.Millisecond, 30_000, 10_000, 50_000)
		eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(levels) >= before+9 })

		mu.Lock()
		defer mu.Unlock()

		last := slices.Clone(levels[len(levels)-5:])
		slices.Sort(last)

		return last[2]
	}

	at50 := level()
	us.Store(75)
	level() // let the change and the filter settle
	at75 := level()

	// Analog response at 10 kHz: 0.30 at 50 µs, 0.21 at 75 µs.
	if at50 == 0 || at75/at50 > 0.85 || at75/at50 < 0.55 {
		t.Fatalf("10 kHz level %.0f at 50 µs, %.0f at 75 µs", at50, at75)
	}
}

// TestSetDuringRateChange: between Retuned with a new sample rate and the
// restart (Start), a demodulator is checked against the new rate, not the
// running one, and the restart binds it in the new run.
func TestSetDuringRateChange(t *testing.T) {
	e := Factory{Logger: slog.New(slog.DiscardHandler)}.New(shared.MustDeviceID("vhf")).(*Engine)
	defer e.Close()

	e.Start(tuning())

	d, err := e.NewDemod(app.DemodParams{Mode: "nfm", OutputRate: 48000, Codec: app.CodecPCM}, func(app.AudioOut) {}, func(app.Meter) {})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	wide := domain.NewTuning(domain.MustFrequency(145_000_000), domain.MustSampleRate(1_024_000))
	e.Retuned(wide)

	// 400 kHz is outside the running 250 kS/s band, inside the new one.
	p := d.Params()
	p.OffsetHz = 400_000

	if err := d.Set(p); err != nil {
		t.Fatalf("set at the new rate: %v", err)
	}

	p.OffsetHz = 600_000
	if err := d.Set(p); err == nil {
		t.Fatal("an offset outside the new band was accepted")
	}

	e.Start(wide)

	dm := d.(*demod)
	dm.mu.Lock()
	b := dm.b
	dm.mu.Unlock()

	e.mu.Lock()
	ep := e.ep
	e.mu.Unlock()

	if b == nil || b.ep != ep || d.Params().OffsetHz != 400_000 {
		t.Fatalf("not bound in the new run: binding %v, params %+v", b, d.Params())
	}
}
