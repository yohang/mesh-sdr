package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/domain"
)

// Lifecycle defaults (§8.2).
const (
	DefaultLinger      = 10 * time.Second
	DefaultAutoRecover = 15 * time.Minute
	ProbeTimeout       = 5 * time.Second
)

// Options configure a Manager.
type Options struct {
	Devices  []*domain.Device
	Sources  Sources
	Engines  Engines
	Reporter Reporter
	Logger   *slog.Logger
	// Linger keeps a device running after its last listener (default 10 s).
	Linger time.Duration
	// AutoRecover restarts a failed device (default 15 min) when the
	// device enables it.
	AutoRecover time.Duration
	// MaxDemods caps the demodulators of the node (default 32).
	MaxDemods int
}

// DefaultMaxDemods is the interim node-wide demodulator cap (ADR 0019).
const DefaultMaxDemods = 32

// Manager runs the devices of the node, one goroutine each.
type Manager struct {
	o       Options
	runners map[string]*runner
	order   []string

	demodMu sync.Mutex
	demods  int
}

// NewManager builds the sources and engines of the devices.
func NewManager(o Options) (*Manager, error) {
	if o.Linger <= 0 {
		o.Linger = DefaultLinger
	}

	if o.AutoRecover <= 0 {
		o.AutoRecover = DefaultAutoRecover
	}

	if o.MaxDemods <= 0 {
		o.MaxDemods = DefaultMaxDemods
	}

	m := &Manager{o: o, runners: map[string]*runner{}}

	for _, d := range o.Devices {
		id := d.ID().String()
		if _, dup := m.runners[id]; dup {
			return nil, domain.ErrInvalidDevice.WithDetail("duplicate device " + id)
		}

		r := &runner{
			m: m, dev: d, engine: o.Engines.New(d.ID()), watchers: map[int]func(domain.Snapshot){},
			wake: make(chan struct{}, 1), log: o.Logger.With(slog.String("device_id", id)),
		}

		r.engine.SetTuning(d.Tuning())

		if d.Usable() {
			src, err := o.Sources.New(d.Params())
			if err != nil {
				return nil, fmt.Errorf("device %s: %w", id, err)
			}

			r.source = src
		}

		m.runners[id] = r
		m.order = append(m.order, id)
	}

	slices.Sort(m.order)

	return m, nil
}

// Run probes the device tools, reports the initial states and runs every
// device until ctx is done; devices are stopped before it returns.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for _, id := range m.order {
		r := m.runners[id]
		r.probe(ctx)
		r.publish()

		wg.Go(func() { r.loop(ctx) })
	}

	wg.Wait()
}

// Devices returns the status of every device, ordered by id.
func (m *Manager) Devices() []domain.Snapshot {
	out := make([]domain.Snapshot, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.runners[id].snapshot())
	}

	return out
}

func (m *Manager) runner(id string) (*runner, error) {
	r, ok := m.runners[id]
	if !ok {
		return nil, domain.ErrDeviceNotFound.WithDetail("device " + id + " not found on this node")
	}

	return r, nil
}

// Lease is a media session attached to a device: USER demand until
// Release.
type Lease struct {
	r    *runner
	once sync.Once
}

// Attach adds USER demand on a device and returns its lease.
func (m *Manager) Attach(id string) (*Lease, error) {
	r, err := m.runner(id)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	err = r.dev.AddListener()
	r.mu.Unlock()

	if err != nil {
		return nil, err
	}

	r.log.Debug("listener attached")
	r.poke()
	r.publish()

	return &Lease{r: r}, nil
}

// Engine returns the device DSP.
func (l *Lease) Engine() Engine { return l.r.engine }

// Snapshot returns the device status.
func (l *Lease) Snapshot() domain.Snapshot { return l.r.snapshot() }

// NewDemod starts a demodulator on the device within the node-wide and
// per-device caps (capacity_exceeded beyond them).
func (l *Lease) NewDemod(p DemodParams, audio func(AudioOut), meter func(Meter)) (Demod, error) {
	m, r := l.r.m, l.r

	m.demodMu.Lock()
	r.mu.Lock()
	perDevice := r.dev.Params().MaxDemods
	full := m.demods >= m.o.MaxDemods || r.demods >= perDevice

	if !full {
		m.demods++
		r.demods++
	}
	r.mu.Unlock()
	m.demodMu.Unlock()

	if full {
		return nil, domain.ErrCapacityExceeded.WithDetail("the node or the device runs its maximum number of demodulators")
	}

	d, err := r.engine.NewDemod(p, audio, meter)
	if err != nil {
		m.releaseDemod(r)

		return nil, err
	}

	return &countedDemod{Demod: d, release: func() { m.releaseDemod(r) }}, nil
}

func (m *Manager) releaseDemod(r *runner) {
	m.demodMu.Lock()
	r.mu.Lock()
	m.demods--
	r.demods--
	r.mu.Unlock()
	m.demodMu.Unlock()
}

// countedDemod releases its capacity once on Close.
type countedDemod struct {
	Demod
	once    sync.Once
	release func()
}

func (d *countedDemod) Close() {
	d.once.Do(func() {
		d.Demod.Close()
		d.release()
	})
}

// Release removes the demand. It is safe to call twice.
func (l *Lease) Release() {
	l.once.Do(func() {
		l.r.mu.Lock()
		l.r.dev.RemoveListener()
		l.r.mu.Unlock()

		l.r.log.Debug("listener detached")
		l.r.poke()
		l.r.publish()
	})
}

// Retune moves the centre frequency of a device and, when rate is not
// zero, its sample rate. A centre change is live on a running source; a
// new sample rate restarts it (the connectors take the rate at start
// only). Both apply at the next start of a stopped device.
func (m *Manager) Retune(id string, hz, rate int64) (domain.Snapshot, error) {
	r, err := m.runner(id)
	if err != nil {
		return domain.Snapshot{}, err
	}

	f, err := domain.NewFrequency(hz)
	if err != nil {
		return domain.Snapshot{}, err
	}

	var sr domain.SampleRate

	if rate != 0 {
		if sr, err = domain.NewSampleRate(rate); err != nil {
			return domain.Snapshot{}, err
		}
	}

	r.mu.Lock()
	old := r.dev.Tuning()

	if !r.dev.Usable() {
		r.mu.Unlock()

		return domain.Snapshot{}, domain.ErrDeviceUnavailable.WithDetail("device " + id + " is not available")
	}

	if err := r.dev.Retune(f); err != nil {
		r.mu.Unlock()

		return domain.Snapshot{}, err
	}

	if rate != 0 {
		if err := r.dev.SetRate(sr); err != nil {
			_ = r.dev.Retune(old.Center())
			r.mu.Unlock()

			return domain.Snapshot{}, err
		}
	}

	tuning, active := r.dev.Tuning(), r.active
	restart := active && tuning.Rate() != old.Rate()
	r.restart = r.restart || restart
	r.mu.Unlock()

	if active && !restart {
		if err := r.source.SetCenter(hz); err != nil {
			r.mu.Lock()
			_ = r.dev.Retune(old.Center())
			r.mu.Unlock()

			return domain.Snapshot{}, fmt.Errorf("retune device %s: %w", id, err)
		}
	}

	r.engine.Retuned(tuning)
	r.log.Info("device retuned", slog.Int64("center_hz", hz), slog.Int("sample_rate", tuning.Rate().PerSecond()),
		slog.Bool("restart", restart))
	r.publish()

	if restart {
		r.poke()
	}

	return r.snapshot(), nil
}

// Watch calls fn with every status of a device until cancel. fn must not
// block.
func (m *Manager) Watch(id string, fn func(domain.Snapshot)) (cancel func(), err error) {
	r, err := m.runner(id)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	k := r.nextWatch
	r.nextWatch++
	r.watchers[k] = fn
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		delete(r.watchers, k)
		r.mu.Unlock()
	}, nil
}

// runner owns one device.
type runner struct {
	m      *Manager
	source Source
	engine Engine
	log    *slog.Logger
	wake   chan struct{}

	mu       sync.Mutex
	dev      *domain.Device
	active   bool
	stopping bool
	// restart: the running source must restart at the current tuning (a
	// new sample rate).
	restart   bool
	demods    int
	watchers  map[int]func(domain.Snapshot)
	nextWatch int
}

func (r *runner) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *runner) snapshot() domain.Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.dev.Snapshot()
}

// publish reports the current status to the hub and the watchers.
func (r *runner) publish() {
	r.mu.Lock()
	s := r.dev.Snapshot()
	watchers := slices.Collect(maps.Values(r.watchers))
	r.mu.Unlock()

	if r.m.o.Reporter != nil {
		r.m.o.Reporter.DeviceState(s)
	}

	for _, w := range watchers {
		w(s)
	}
}

// transition moves the device and publishes the change.
func (r *runner) transition(to domain.State, reason string, attempt int) {
	r.mu.Lock()
	from, fromReason := r.dev.State()
	err := r.dev.Transition(to, reason, attempt)
	r.mu.Unlock()

	if err != nil {
		r.log.Debug("ignored device transition", slog.Any("error", err))

		return
	}

	if from != to || fromReason != reason {
		r.publish()
	}
}

// onEvent applies a source event.
func (r *runner) onEvent(e SourceEvent) {
	r.mu.Lock()
	stopping := r.stopping
	r.mu.Unlock()

	if e.State == domain.StateStopping && !stopping {
		return
	}

	r.transition(e.State, e.Reason, e.Attempt)
}

func (r *runner) probe(ctx context.Context) {
	if r.source == nil {
		return
	}

	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	r.mu.Lock()
	p := r.dev.Params()
	r.mu.Unlock()

	if err := r.m.o.Sources.Probe(pctx, p); err != nil {
		r.log.Warn("device unavailable: its tool cannot run", slog.Any("error", err))
		r.mu.Lock()
		_ = r.dev.Transition(domain.StateUnavailable, "tool_missing", 0)
		r.mu.Unlock()
	}
}

// loop starts the source when the device is wanted and stops it after the
// linger once it is not (§8.2 "Lifecycle").
func (r *runner) loop(ctx context.Context) {
	var (
		runCancel context.CancelFunc
		runDone   chan error
		linger    *time.Timer
		lingerC   <-chan time.Time
		recoverC  <-chan time.Time
	)

	stopLinger := func() {
		if linger != nil {
			linger.Stop()
			linger, lingerC = nil, nil
		}
	}

	start := func(tuning domain.Tuning) {
		r.mu.Lock()
		r.active, r.stopping = true, false
		r.mu.Unlock()

		r.transition(domain.StateStarting, "", 0)
		r.engine.Start(tuning)

		rctx, cancel := context.WithCancel(ctx)
		runCancel, runDone = cancel, make(chan error, 1)

		go func() { runDone <- r.source.Run(rctx, tuning, r.engine, r.onEvent) }()
	}

	defer r.engine.Close()

	for {
		r.mu.Lock()
		wanted := r.dev.Wanted()
		state, _ := r.dev.State()
		tuning := r.dev.Tuning()
		stopping := r.stopping
		restart := r.restart && runDone != nil && !stopping

		if restart {
			r.stopping = true
		}
		r.mu.Unlock()

		if restart {
			r.log.Info("restarting device at a new sample rate")
			runCancel()
		}

		switch {
		case runDone != nil && wanted:
			stopLinger()
		case runDone != nil && !stopping && lingerC == nil:
			linger = time.NewTimer(r.m.o.Linger)
			lingerC = linger.C
		case runDone == nil && wanted && state == domain.StateStopped:
			start(tuning)
		}

		select {
		case <-ctx.Done():
			stopLinger()

			if runDone != nil {
				r.mu.Lock()
				r.stopping = true
				r.mu.Unlock()
				runCancel()
				<-runDone
				r.engine.Stop()
			}

			return
		case <-r.wake:
		case <-lingerC:
			linger, lingerC = nil, nil

			r.mu.Lock()
			idle := !r.dev.Wanted()
			r.stopping = idle
			r.mu.Unlock()

			if idle && runCancel != nil {
				r.log.Info("stopping device: no demand")
				runCancel()
			}
		case err := <-runDone:
			runDone, runCancel = nil, nil
			stopLinger()
			r.engine.Stop()

			r.mu.Lock()
			r.active, r.stopping, r.restart = false, false, false
			autoRecover := r.dev.Params().AutoRecover
			r.mu.Unlock()

			switch {
			case err == nil:
				r.transition(domain.StateStopped, "", 0)
			case errors.Is(err, ErrSourceUnavailable):
				r.transition(domain.StateUnavailable, "tool_unavailable", 0)
			default:
				r.transition(domain.StateFailed, "start_attempts_exhausted", 0)

				if autoRecover {
					recoverC = time.After(r.m.o.AutoRecover)
				}
			}
		case <-recoverC:
			recoverC = nil

			r.log.Info("auto-recovering failed device")
			r.transition(domain.StateStopped, "auto_recover", 0)
		}
	}
}
