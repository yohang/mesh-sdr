package app_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

type fakeSource struct {
	mu       sync.Mutex
	runs     int
	fail     error
	setErr   error
	centers  []int64
	probeErr error
}

func (s *fakeSource) Run(ctx context.Context, _ domain.Tuning, sink app.IQSink, report func(app.SourceEvent)) error {
	s.mu.Lock()
	s.runs++
	fail := s.fail
	s.mu.Unlock()

	report(app.SourceEvent{State: domain.StateStarting})

	if fail != nil {
		report(app.SourceEvent{State: domain.StateFailed, Reason: "crash"})

		return fail
	}

	report(app.SourceEvent{State: domain.StateRunning})
	sink.Samples(0, time.Now(), make([]complex64, 8))
	<-ctx.Done()
	report(app.SourceEvent{State: domain.StateStopping})
	report(app.SourceEvent{State: domain.StateStopped})

	return nil
}

func (s *fakeSource) SetCenter(hz int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.centers = append(s.centers, hz)

	return s.setErr
}

type fakeSources struct{ src *fakeSource }

func (f fakeSources) Probe(context.Context, domain.DeviceParams) error { return f.src.probeErr }
func (f fakeSources) New(domain.DeviceParams) (app.Source, error)      { return f.src, nil }

type fakeEngine struct {
	mu                   sync.Mutex
	starts, stops, tunes int
	samples              int
	app.Engine
}

func (e *fakeEngine) Samples(uint64, time.Time, []complex64) { e.mu.Lock(); e.samples++; e.mu.Unlock() }
func (e *fakeEngine) Start(domain.Tuning)                    { e.mu.Lock(); e.starts++; e.mu.Unlock() }
func (e *fakeEngine) SetTuning(domain.Tuning)                {}

type fakeDemod struct{ app.Demod }

func (fakeDemod) Close() {}

func (e *fakeEngine) NewDemod(app.DemodParams, func(app.AudioOut), func(app.Meter)) (app.Demod, error) {
	return fakeDemod{}, nil
}
func (e *fakeEngine) Stop()                 { e.mu.Lock(); e.stops++; e.mu.Unlock() }
func (e *fakeEngine) Retuned(domain.Tuning) { e.mu.Lock(); e.tunes++; e.mu.Unlock() }
func (e *fakeEngine) Close()                {}

type fakeEngines struct{ e *fakeEngine }

func (f fakeEngines) New(shared.DeviceID) app.Engine { return f.e }

type reporter struct {
	mu  sync.Mutex
	all []domain.Snapshot
}

func (r *reporter) DeviceState(s domain.Snapshot) {
	r.mu.Lock()
	r.all = append(r.all, s)
	r.mu.Unlock()
}

func (r *reporter) has(pred func(domain.Snapshot) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, s := range r.all {
		if pred(s) {
			return true
		}
	}

	return false
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func newDevice(t *testing.T, alwaysOn bool) *domain.Device {
	t.Helper()

	typ, _ := domain.NewDeviceType(domain.TypeRTLSDR)
	drv, _ := domain.NewDriver(typ, domain.DriverSettings{Device: "0", Gain: domain.AutoGain()})
	r, _ := domain.NewFreqRange(domain.MustFrequency(24_000_000), domain.MustFrequency(1_766_000_000))

	d, err := domain.NewDevice(domain.DeviceParams{
		ID: shared.MustDeviceID("rtl"), Name: "RTL", Type: typ, Enabled: true, Range: r,
		Rates: []domain.SampleRate{domain.MustSampleRate(2_400_000)}, AlwaysOn: alwaysOn, AutoRecover: true, Driver: drv,
	})
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func run(t *testing.T, src *fakeSource, eng *fakeEngine, rep *reporter, dev *domain.Device) *app.Manager {
	t.Helper()

	m, err := app.NewManager(app.Options{
		Devices: []*domain.Device{dev}, Sources: fakeSources{src}, Engines: fakeEngines{eng}, Reporter: rep,
		Logger: slog.New(slog.DiscardHandler), Linger: 50 * time.Millisecond, AutoRecover: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		m.Run(ctx)
		close(done)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return m
}

func TestAttachLingerAndRetune(t *testing.T) {
	src, eng, rep := &fakeSource{}, &fakeEngine{}, &reporter{}
	m := run(t, src, eng, rep, newDevice(t, false))

	if _, err := m.Attach("nope"); !errors.Is(err, domain.ErrDeviceNotFound) {
		t.Fatal(err)
	}

	lease, err := m.Attach("rtl")
	if err != nil {
		t.Fatal(err)
	}

	eventually(t, "running", func() bool {
		return rep.has(func(s domain.Snapshot) bool { return s.State == domain.StateRunning && s.Listeners == 1 })
	})

	// Retune live, then a refused retune is rolled back.
	if s, err := m.Retune("rtl", 145_000_000); err != nil || s.CenterHz != 145_000_000 {
		t.Fatal(s, err)
	}

	src.mu.Lock()
	src.setErr = errors.New("control socket down")
	src.mu.Unlock()

	if _, err := m.Retune("rtl", 146_000_000); err == nil {
		t.Fatal("failed retune reported success")
	}

	if s := lease.Snapshot(); s.CenterHz != 145_000_000 {
		t.Fatalf("centre after failed retune %d", s.CenterHz)
	}

	if lease.Engine() != app.Engine(eng) {
		t.Fatal("lease engine")
	}

	// A quick re-attach within the linger keeps the source running.
	lease.Release()
	lease.Release()

	again, _ := m.Attach("rtl")
	time.Sleep(100 * time.Millisecond)
	again.Release()

	eventually(t, "stopped after linger", func() bool {
		return rep.has(func(s domain.Snapshot) bool {
			return s.State == domain.StateStopped && s.Listeners == 0 && s.CenterHz == 145_000_000
		})
	})

	src.mu.Lock()
	defer src.mu.Unlock()

	eng.mu.Lock()
	defer eng.mu.Unlock()

	if src.runs != 1 || eng.starts != 1 || eng.stops != 1 || eng.tunes != 1 || eng.samples != 1 {
		t.Fatalf("runs %d starts %d stops %d tunes %d samples %d", src.runs, eng.starts, eng.stops, eng.tunes, eng.samples)
	}
}

func TestFailedThenAutoRecover(t *testing.T) {
	src, eng, rep := &fakeSource{fail: app.ErrSourceFailed}, &fakeEngine{}, &reporter{}
	run(t, src, eng, rep, newDevice(t, true))

	eventually(t, "failed", func() bool {
		return rep.has(func(s domain.Snapshot) bool { return s.State == domain.StateFailed })
	})

	src.mu.Lock()
	src.fail = nil
	src.mu.Unlock()

	eventually(t, "recovered", func() bool {
		return rep.has(func(s domain.Snapshot) bool { return s.State == domain.StateRunning })
	})
}

func TestProbeFailureMakesUnavailable(t *testing.T) {
	src, eng, rep := &fakeSource{probeErr: errors.New("missing")}, &fakeEngine{}, &reporter{}
	m := run(t, src, eng, rep, newDevice(t, true))

	eventually(t, "unavailable", func() bool {
		return rep.has(func(s domain.Snapshot) bool { return s.State == domain.StateUnavailable && s.Reason == "tool_missing" })
	})

	if _, err := m.Attach("rtl"); !errors.Is(err, domain.ErrDeviceUnavailable) {
		t.Fatal(err)
	}

	if _, err := m.Retune("rtl", 145_000_000); !errors.Is(err, domain.ErrDeviceUnavailable) {
		t.Fatal(err)
	}

	src.mu.Lock()
	defer src.mu.Unlock()

	if src.runs != 0 {
		t.Fatal("unavailable device started")
	}
}

func TestInvalidDeviceIsReportedFailedAndNeverStarts(t *testing.T) {
	src, eng, rep := &fakeSource{}, &fakeEngine{}, &reporter{}
	m := run(t, src, eng, rep, domain.NewInvalidDevice(shared.MustDeviceID("rtl"), "RTL"))

	eventually(t, "failed", func() bool {
		return rep.has(func(s domain.Snapshot) bool {
			return s.State == domain.StateFailed && s.Reason == domain.ReasonInvalidConfig
		})
	})

	if _, err := m.Attach("rtl"); !errors.Is(err, domain.ErrDeviceUnavailable) {
		t.Fatal(err)
	}

	time.Sleep(150 * time.Millisecond) // longer than the test auto-recover

	src.mu.Lock()
	defer src.mu.Unlock()

	if src.runs != 0 {
		t.Fatal("invalid device started")
	}
}

func TestWatch(t *testing.T) {
	src, eng, rep := &fakeSource{}, &fakeEngine{}, &reporter{}
	m := run(t, src, eng, rep, newDevice(t, false))

	var mu sync.Mutex

	var seen []domain.State

	cancel, err := m.Watch("rtl", func(s domain.Snapshot) { mu.Lock(); seen = append(seen, s.State); mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Watch("nope", nil); err == nil {
		t.Fatal("watch of an unknown device")
	}

	lease, _ := m.Attach("rtl")
	eventually(t, "watched running", func() bool {
		mu.Lock()
		defer mu.Unlock()

		for _, s := range seen {
			if s == domain.StateRunning {
				return true
			}
		}

		return false
	})

	cancel()
	lease.Release()

	if len(m.Devices()) != 1 {
		t.Fatal("devices")
	}
}

func TestDemodCaps(t *testing.T) {
	src, eng, rep := &fakeSource{}, &fakeEngine{}, &reporter{}

	typ, _ := domain.NewDeviceType(domain.TypeRTLSDR)
	drv, _ := domain.NewDriver(typ, domain.DriverSettings{Device: "0", Gain: domain.AutoGain()})
	r, _ := domain.NewFreqRange(domain.MustFrequency(24_000_000), domain.MustFrequency(1_766_000_000))
	mk := func(id string, maxDemods int) *domain.Device {
		d, err := domain.NewDevice(domain.DeviceParams{
			ID: shared.MustDeviceID(id), Name: id, Type: typ, Enabled: true, Range: r,
			Rates: []domain.SampleRate{domain.MustSampleRate(2_400_000)}, Driver: drv, MaxDemods: maxDemods,
		})
		if err != nil {
			t.Fatal(err)
		}

		return d
	}

	m, err := app.NewManager(app.Options{
		Devices: []*domain.Device{mk("a", 1), mk("b", 0)}, Sources: fakeSources{src}, Engines: fakeEngines{eng}, Reporter: rep,
		Logger: slog.New(slog.DiscardHandler), MaxDemods: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	la, _ := m.Attach("a")
	lb, _ := m.Attach("b")
	p := app.DemodParams{}

	d1, err := la.NewDemod(p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Per-device cap of a, then the node cap.
	if _, err := la.NewDemod(p, nil, nil); !errors.Is(err, domain.ErrCapacityExceeded) {
		t.Fatalf("device cap: %v", err)
	}

	if _, err := lb.NewDemod(p, nil, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := lb.NewDemod(p, nil, nil); !errors.Is(err, domain.ErrCapacityExceeded) {
		t.Fatalf("node cap: %v", err)
	}

	d1.Close()
	d1.Close()

	if _, err := lb.NewDemod(p, nil, nil); err != nil {
		t.Fatalf("capacity not released: %v", err)
	}
}
