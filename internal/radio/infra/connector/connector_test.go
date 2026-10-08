package connector_test

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	"github.com/yohang/mesh-sdr/internal/radio/infra/connector"
	"github.com/yohang/mesh-sdr/internal/radio/infra/engine"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

var toolDir string

func TestMain(m *testing.M) {
	process.MaybeRunExecHelper()

	dir, err := os.MkdirTemp("", "connector-tools-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for _, name := range []string{"rtl_connector", "rtl_tcp_connector"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name), "./fakeconnector")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr

		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "build fake connector:", err)
			os.Exit(1)
		}
	}

	toolDir = dir
	code := m.Run()

	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func logger() *slog.Logger {
	if os.Getenv("RADIO_TEST_LOG") == "1" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	return slog.New(slog.DiscardHandler)
}

const testRate = 250_000

func device(t *testing.T, id, driverDevice string, alwaysOn bool) *domain.Device {
	t.Helper()

	typ, _ := domain.NewDeviceType(domain.TypeRTLSDR)

	drv, err := domain.NewDriver(typ, domain.DriverSettings{Device: driverDevice, Gain: domain.AutoGain()})
	if err != nil {
		t.Fatal(err)
	}

	r, _ := domain.NewFreqRange(domain.MustFrequency(24_000_000), domain.MustFrequency(1_766_000_000))

	d, err := domain.NewDevice(domain.DeviceParams{
		ID: shared.MustDeviceID(id), Name: id, Type: typ, Enabled: true, Range: r,
		Rates: []domain.SampleRate{domain.MustSampleRate(testRate)}, AlwaysOn: alwaysOn, AutoRecover: true, Driver: drv,
	})
	if err != nil {
		t.Fatal(err)
	}

	return d
}

type states struct {
	mu   sync.Mutex
	all  []domain.Snapshot
	seen map[string]int
	ch   chan domain.Snapshot
}

func (s *states) DeviceState(sn domain.Snapshot) {
	s.mu.Lock()
	s.all = append(s.all, sn)
	s.mu.Unlock()

	select {
	case s.ch <- sn:
	default:
	}
}

// waitFor waits for a state matching pred among the states reported since
// the last match of the same device.
func (s *states) waitFor(t *testing.T, d time.Duration, pred func(domain.Snapshot) bool) domain.Snapshot {
	t.Helper()

	deadline := time.Now().Add(d)

	for {
		s.mu.Lock()
		for i := range s.all {
			if i >= s.seen[s.all[i].ID] && pred(s.all[i]) {
				s.seen[s.all[i].ID] = i + 1
				sn := s.all[i]
				s.mu.Unlock()

				return sn
			}
		}

		if time.Now().After(deadline) {
			defer s.mu.Unlock()
			t.Fatalf("state not reached in %s; seen %+v", d, s.all)

			return domain.Snapshot{}
		}
		s.mu.Unlock()

		select {
		case <-s.ch:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type harness struct {
	m      *app.Manager
	states *states
	lines  *lineLog
	cancel context.CancelFunc
	done   chan struct{}
}

// lineLog records the connector lines sent to the device log.
type lineLog struct {
	mu    sync.Mutex
	lines map[string][]string
}

func (l *lineLog) add(device string, line process.Line) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lines[device] = append(l.lines[device], line.Text)
}

func (l *lineLog) of(device string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.lines[device])
}

func newHarness(t *testing.T, policy *process.RestartPolicy, devices ...*domain.Device) *harness {
	t.Helper()

	rt := filepath.Join(t.TempDir(), "run")
	self, _ := filepath.Abs(os.Args[0])

	sup, err := process.New(process.Options{RuntimeDir: rt, Logger: logger(), HelperPath: self})
	if err != nil {
		t.Fatal(err)
	}

	ports, err := connector.NewPorts(42000, 42999)
	if err != nil {
		t.Fatal(err)
	}

	lines := &lineLog{lines: map[string][]string{}}
	src := connector.NewSources(connector.Options{
		Supervisor: sup, Tools: connector.Tools{Dirs: []string{toolDir}}, Ports: ports, Logger: logger(),
		Policy: policy, StartTimeout: 2 * time.Second, StallTimeout: 500 * time.Millisecond, DeviceLog: lines.add,
	})

	st := &states{ch: make(chan domain.Snapshot, 256), seen: map[string]int{}}

	m, err := app.NewManager(app.Options{
		Devices: devices, Sources: src, Engines: engine.Factory{Logger: logger()}, Reporter: st, Logger: logger(),
		Linger: 300 * time.Millisecond, AutoRecover: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{m: m, states: st, lines: lines, cancel: cancel, done: make(chan struct{})}

	go func() {
		m.Run(ctx)
		close(h.done)
	}()

	t.Cleanup(func() {
		cancel()
		<-h.done
	})

	return h
}

func TestDemandStartsAndLingerStops(t *testing.T) {
	h := newHarness(t, nil, device(t, "rtl", "0", false))

	if s := h.m.Devices()[0]; s.State != domain.StateStopped {
		t.Fatalf("initial state %s", s.State)
	}

	lease, err := h.m.Attach("rtl")
	if err != nil {
		t.Fatal(err)
	}

	h.states.waitFor(t, 5*time.Second, func(s domain.Snapshot) bool { return s.State == domain.StateRunning && s.Listeners == 1 })

	// The connector's stderr goes to the device log (SRC-005).
	if lines := h.lines.of("rtl"); !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "fakeconnector streaming") }) {
		t.Errorf("device log lines = %q", lines)
	}

	// Spectrum lines flow from the shared spectrum.
	frames := make(chan app.SpectrumFrame, 16)
	cancel := lease.Engine().SubscribeSpectrum(func(f app.SpectrumFrame) {
		select {
		case frames <- f:
		default:
		}
	})

	info := lease.Engine().Spectrum()
	if info.Size != 4096 || info.SpanHz != testRate || info.StartHz != 24_000_000 {
		t.Fatalf("spectrum %+v", info)
	}

	var f app.SpectrumFrame
	select {
	case f = <-frames:
	case <-time.After(3 * time.Second):
		t.Fatal("no spectrum frame")
	}

	if len(f.Payload) != 8+4096 || f.TimestampUS == 0 {
		t.Fatalf("frame %d bytes ts %d", len(f.Payload), f.TimestampUS)
	}

	// The NFM carrier sits at +rate/8 with a 1 kHz tone.
	audio := make(chan app.AudioOut, 256)
	meters := make(chan app.Meter, 16)

	d, err := lease.Engine().NewDemod(app.DemodParams{
		Mode: "nfm", OffsetHz: testRate / 8, LowHz: -5000, HighHz: 5000, OutputRate: dsp.DefaultOutputRate, Codec: app.CodecPCM,
	}, func(a app.AudioOut) {
		select {
		case audio <- a:
		default:
		}
	}, func(m app.Meter) {
		select {
		case meters <- m:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	var pcm []float32

	for len(pcm) < 6000 {
		select {
		case a := <-audio:
			if a.Samples != 240 || a.Duration != 20*time.Millisecond {
				t.Fatalf("audio frame %+v", a)
			}

			for i := 0; i+1 < len(a.Payload); i += 2 {
				pcm = append(pcm, float32(int16(uint16(a.Payload[i])|uint16(a.Payload[i+1])<<8))/32768)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("audio stopped after %d samples", len(pcm))
		}
	}

	if e := goertzel(pcm[3000:], 12000, 1000); e < 0.3 {
		t.Fatalf("1 kHz tone holds %.2f of the audio energy", e)
	}

	select {
	case m := <-meters:
		if !m.Open || m.LevelDB < -25 {
			t.Fatalf("meter %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("no meter")
	}

	// Live retune: the carrier stays at its absolute frequency.
	if _, err := h.m.Retune("rtl", 24_000_000+testRate/2+1000, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := h.m.Retune("rtl", 10, 0); err == nil {
		t.Fatal("retune out of range accepted")
	}

	h.states.waitFor(t, 2*time.Second, func(s domain.Snapshot) bool { return s.CenterHz == 24_000_000+testRate/2+1000 })

	d.Close()
	cancel()
	lease.Release()

	h.states.waitFor(t, 5*time.Second, func(s domain.Snapshot) bool { return s.State == domain.StateStopped && s.Listeners == 0 })
}

func goertzel(audio []float32, rate, freq float64) float64 {
	k := 2 * math.Cos(2*math.Pi*freq/rate)

	var s1, s2, total float64
	for _, v := range audio {
		s := float64(v) + k*s1 - s2
		s2, s1 = s1, s
		total += float64(v) * float64(v)
	}

	if total == 0 {
		return 0
	}

	return (s1*s1 + s2*s2 - k*s1*s2) / (total * float64(len(audio)) / 2)
}

func TestAlwaysOnStallAndCrashRetries(t *testing.T) {
	quick := &process.RestartPolicy{Steps: []time.Duration{50 * time.Millisecond}, MaxAttempts: 3}
	h := newHarness(t, quick, device(t, "stall", "stall", true), device(t, "crash", "crash", true))

	// The stalled device is restarted with reason sample_stall.
	h.states.waitFor(t, 10*time.Second, func(s domain.Snapshot) bool {
		return s.ID == "stall" && s.State == domain.StateRetryWait && s.Reason == "sample_stall"
	})

	// The crashing one fails after its attempts.
	h.states.waitFor(t, 10*time.Second, func(s domain.Snapshot) bool { return s.ID == "crash" && s.State == domain.StateFailed })
}

func TestStartTimeoutAndMissingTool(t *testing.T) {
	quick := &process.RestartPolicy{Steps: []time.Duration{50 * time.Millisecond}, MaxAttempts: 2}
	dev := device(t, "noiq", "noiq", true)

	typ, _ := domain.NewDeviceType(domain.TypeRTLTCP)
	drv, _ := domain.NewDriver(typ, domain.DriverSettings{Device: "127.0.0.1:1234", Gain: domain.AutoGain()})
	r, _ := domain.NewFreqRange(domain.MustFrequency(24_000_000), domain.MustFrequency(1_766_000_000))
	tcp, _ := domain.NewDevice(domain.DeviceParams{
		ID: shared.MustDeviceID("tcp"), Name: "tcp", Type: typ, Enabled: true, Range: r,
		Rates: []domain.SampleRate{domain.MustSampleRate(testRate)}, AlwaysOn: true, Driver: drv,
	})

	h := newHarness(t, quick, dev, tcp)

	h.states.waitFor(t, 10*time.Second, func(s domain.Snapshot) bool {
		return s.ID == "noiq" && s.State == domain.StateRetryWait && s.Reason == "start_timeout"
	})

	// rtl_tcp runs the same fake.
	h.states.waitFor(t, 5*time.Second, func(s domain.Snapshot) bool { return s.ID == "tcp" && s.State == domain.StateRunning })

	// The longest device id (63 characters) runs too.
	long := strings.Repeat("l", 63)
	hl := newHarness(t, quick, device(t, long, "0", true))
	hl.states.waitFor(t, 5*time.Second, func(s domain.Snapshot) bool { return s.ID == long && s.State == domain.StateRunning })

	// A device whose tool is missing is unavailable.
	sup, _ := process.New(process.Options{RuntimeDir: filepath.Join(t.TempDir(), "run"), Logger: logger()})
	ports, _ := connector.NewPorts(43000, 43010)
	src := connector.NewSources(connector.Options{Supervisor: sup, Tools: connector.Tools{Dirs: []string{t.TempDir()}}, Ports: ports, Logger: logger()})

	if err := src.Probe(context.Background(), dev.Params()); err == nil {
		t.Fatal("probe of a missing tool succeeded")
	}

	srcOK := connector.NewSources(connector.Options{Supervisor: sup, Tools: connector.Tools{Dirs: []string{toolDir}}, Ports: ports, Logger: logger()})
	if err := srcOK.Probe(context.Background(), dev.Params()); err != nil {
		t.Fatal(err)
	}

	// The capability report lists every registered type with the
	// availability of its connector (SRC-001).
	if got := srcOK.Drivers(context.Background()); len(got) != 2 || got[0].Type != domain.TypeRTLSDR || got[1].Type != domain.TypeRTLTCP ||
		!got[0].Available || !got[1].Available {
		t.Errorf("drivers = %+v", got)
	}

	if got := src.Drivers(context.Background()); len(got) != 2 || got[0].Available || got[0].Reason == "" {
		t.Errorf("drivers without tools = %+v", got)
	}
}

func TestToolsAndPorts(t *testing.T) {
	tools := connector.Tools{Paths: map[string]string{"rtl_connector": "/opt/x/rtl_connector"}, Dirs: []string{toolDir}}

	if p, err := tools.Resolve("rtl_connector"); err != nil || p != "/opt/x/rtl_connector" {
		t.Fatal(p, err)
	}

	if p, err := tools.Resolve("rtl_tcp_connector"); err != nil || p != filepath.Join(toolDir, "rtl_tcp_connector") {
		t.Fatal(p, err)
	}

	if _, err := (connector.Tools{Paths: map[string]string{"x": "rel"}}).Resolve("x"); err == nil {
		t.Fatal("relative tool path accepted")
	}

	ports, _ := connector.NewPorts(44000, 44003)

	a, err := ports.Take(2)
	if err != nil {
		t.Fatal(err)
	}

	b, err := ports.Take(2)
	if err != nil || a[0] == b[0] || a[1] == b[1] {
		t.Fatal(a, b, err)
	}

	if _, err := ports.Take(1); err == nil {
		t.Fatal("exhausted pool gave a port")
	}

	ports.Release(a)

	if c, err := ports.Take(2); err != nil || c[0] != a[0] {
		t.Fatal(c, err)
	}

	if _, err := connector.NewPorts(80, 90); err == nil {
		t.Fatal("privileged range accepted")
	}
}

// levels counts the records logged at each level.
type levels struct {
	mu sync.Mutex
	n  map[slog.Level]int
}

func (l *levels) Enabled(context.Context, slog.Level) bool { return true }
func (l *levels) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *levels) WithGroup(string) slog.Handler            { return l }
func (l *levels) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.n[r.Level]++
	l.mu.Unlock()

	return nil
}

func (l *levels) count(lv slog.Level) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.n[lv]
}

// TestVersionProbe: a connector that prints its version and exits non-zero
// (the fake, like owrx_connector) is available and logs no error; one that
// prints no version is unavailable, logged once at Warn.
func TestVersionProbe(t *testing.T) {
	ctx := context.Background()
	rec := &levels{n: map[slog.Level]int{}}
	log := slog.New(rec)

	sup, err := process.New(process.Options{RuntimeDir: filepath.Join(t.TempDir(), "run"), Logger: log})
	if err != nil {
		t.Fatal(err)
	}

	ok := connector.NewSources(connector.Options{Supervisor: sup, Tools: connector.Tools{Dirs: []string{toolDir}}, Logger: log})
	for _, d := range ok.Drivers(ctx) {
		if !d.Available {
			t.Errorf("driver %+v", d)
		}
	}

	if n := rec.count(slog.LevelError) + rec.count(slog.LevelWarn); n != 0 {
		t.Errorf("available drivers logged %d warnings or errors", n)
	}

	broken := t.TempDir()
	for _, name := range []string{"rtl_connector", "rtl_tcp_connector"} {
		if err := os.WriteFile(filepath.Join(broken, name), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	bad := connector.NewSources(connector.Options{Supervisor: sup, Tools: connector.Tools{Dirs: []string{broken}}, Logger: log})
	for range 2 {
		for _, d := range bad.Drivers(ctx) {
			if d.Available || d.Reason == "" {
				t.Errorf("broken driver %+v", d)
			}
		}
	}

	if e, w := rec.count(slog.LevelError), rec.count(slog.LevelWarn); e != 0 || w != 2 {
		t.Errorf("broken drivers logged %d errors, %d warnings; want 0, 2 (once per type)", e, w)
	}
}
