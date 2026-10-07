// Package engine is the DSP runtime of a device (TECHNICAL_SPEC §8.3, ADR
// 0014, ADR 0019), built on internal/dsp:
//
//   - the device front-end writes the connector samples into an IQ ring
//     (250 ms) with their sample index and time;
//   - the shared spectrum, one goroutine per device while it has
//     subscribers, encodes each line once (FFT u8 dB) for every subscriber;
//   - the shared FFT channelizer, one goroutine per device while it has
//     demodulators, runs the forward FFT once per block and extracts every
//     listener channel;
//   - each demodulator runs its chain (analog mode catalogue in demod.go)
//     and audio framing in its own
//     goroutine, reading its channel ring.
//
// Rings are bounded on every edge; a slow reader loses its oldest blocks
// and sees a gap, never the producer.
package engine

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Defaults.
const (
	// RingDuration is the depth of the IQ and channel rings (§8.3).
	RingDuration = 250 * time.Millisecond
	// MaxBlock is the largest IQ block the front-end receives.
	MaxBlock = 16384
	// MeterInterval paces demod.meter (≤ 10 Hz, §6.5).
	MeterInterval = 100 * time.Millisecond
)

// Factory builds engines.
type Factory struct {
	Logger *slog.Logger
}

// New implements app.Engines.
func (f Factory) New(id shared.DeviceID) app.Engine {
	return New(f.Logger.With(slog.String("device_id", id.String())))
}

// Engine is the DSP of one device.
type Engine struct {
	log *slog.Logger

	mu     sync.Mutex
	ep     *epoch
	tuning domain.Tuning
	subs   map[int]func(app.SpectrumFrame)
	next   int
	demods map[*demod]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New returns an idle engine.
func New(log *slog.Logger) *Engine {
	return &Engine{log: log, subs: map[int]func(app.SpectrumFrame){}, demods: map[*demod]struct{}{}}
}

// epoch is one run of the device source.
type epoch struct {
	ctx    context.Context
	cancel context.CancelFunc
	rate   int
	iq     *dsp.Ring[complex64]
	plan   dsp.ChannelPlan

	spectrumCancel    context.CancelFunc
	channelizerCancel context.CancelFunc
}

func ringSlots(rate, block int) int {
	return int(RingDuration.Seconds()*float64(rate))/block + 2
}

// Start implements app.Engine. The demodulator channels are rebuilt for
// the new run outside the engine lock, so the front-end is never held up
// by filter design.
func (e *Engine) Start(t domain.Tuning) {
	e.mu.Lock()
	e.stopLocked()

	if e.closed {
		e.mu.Unlock()

		return
	}

	rate := t.Rate().PerSecond()
	ctx, cancel := context.WithCancel(context.Background())
	ep := &epoch{ctx: ctx, cancel: cancel, rate: rate, iq: dsp.NewRing[complex64](ringSlots(rate, MaxBlock), MaxBlock)}

	plan, err := dsp.NewChannelPlan(rate, dsp.NarrowChannelRate)
	if err != nil {
		e.log.Warn("no channelizer at this sample rate: demodulators unavailable", slog.Int("sample_rate", rate), slog.Any("error", err))
	}

	ep.plan = plan
	e.ep, e.tuning = ep, t
	demods := slices.Collect(maps.Keys(e.demods))
	e.reconcileLocked()
	e.mu.Unlock()

	for _, d := range demods {
		e.bind(d, ep)
	}
}

// bind builds the channel of d for ep outside the lock and installs it if
// ep is still the current run. An offset that no longer fits leaves the
// demodulator silent until it is changed.
func (e *Engine) bind(d *demod, ep *epoch) {
	b, err := newBinding(ep, d.Params())
	if err != nil {
		e.log.Warn("demodulator does not fit the new sample rate", slog.Any("error", err))
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ep != ep {
		return
	}

	if _, ok := e.demods[d]; ok {
		d.setBinding(b)
	}
}

// SetTuning records the tuning of a device that has not started yet, so
// that the spectrum geometry and demodulator offsets are checked against
// it.
func (e *Engine) SetTuning(t domain.Tuning) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ep == nil {
		e.tuning = t
	}
}

// Retuned implements app.Engine.
func (e *Engine) Retuned(t domain.Tuning) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.tuning = t
}

// Stop implements app.Engine.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.stopLocked()
}

func (e *Engine) stopLocked() {
	if e.ep == nil {
		return
	}

	e.ep.cancel()
	e.ep.iq.Close()

	for d := range e.demods {
		d.setBinding(nil)
	}

	e.ep = nil
}

// Close implements app.Engine.
func (e *Engine) Close() {
	e.mu.Lock()
	e.stopLocked()
	e.closed = true
	demods := slices.Collect(maps.Keys(e.demods))
	e.mu.Unlock()

	for _, d := range demods {
		d.Close()
	}

	e.wg.Wait()
}

// Samples implements app.IQSink: the device front-end.
func (e *Engine) Samples(index uint64, t time.Time, iq []complex64) {
	e.mu.Lock()
	ep := e.ep
	e.mu.Unlock()

	if ep != nil {
		ep.iq.Push(index, len(iq), t, iq)
	}
}

// Spectrum implements app.Engine.
func (e *Engine) Spectrum() app.SpectrumInfo {
	e.mu.Lock()
	defer e.mu.Unlock()

	cfg := dsp.DefaultSpectrum(e.tuning.Rate().PerSecond())

	return app.SpectrumInfo{Size: cfg.Size, FPS: cfg.FPS, StartHz: e.tuning.Start(), SpanHz: e.tuning.Rate().PerSecond()}
}

// SubscribeSpectrum implements app.Engine.
func (e *Engine) SubscribeSpectrum(deliver func(app.SpectrumFrame)) func() {
	e.mu.Lock()
	k := e.next
	e.next++
	e.subs[k] = deliver
	e.reconcileLocked()
	e.mu.Unlock()

	return func() {
		e.mu.Lock()
		delete(e.subs, k)
		e.reconcileLocked()
		e.mu.Unlock()
	}
}

// reconcileLocked runs the shared spectrum while it has subscribers and the
// channelizer while it has demodulators, for the current run.
func (e *Engine) reconcileLocked() {
	ep := e.ep
	if ep == nil {
		return
	}

	switch {
	case len(e.subs) > 0 && ep.spectrumCancel == nil:
		ctx, cancel := context.WithCancel(ep.ctx)
		ep.spectrumCancel = cancel

		e.wg.Go(func() { e.runSpectrum(ctx, ep) })
	case len(e.subs) == 0 && ep.spectrumCancel != nil:
		ep.spectrumCancel()
		ep.spectrumCancel = nil
	}

	switch {
	case len(e.demods) > 0 && ep.channelizerCancel == nil && ep.plan.N > 0:
		ctx, cancel := context.WithCancel(ep.ctx)
		ep.channelizerCancel = cancel

		e.wg.Go(func() { e.runChannelizer(ctx, ep) })
	case len(e.demods) == 0 && ep.channelizerCancel != nil:
		ep.channelizerCancel()
		ep.channelizerCancel = nil
	}
}

func (e *Engine) runSpectrum(ctx context.Context, ep *epoch) {
	s, err := dsp.NewSpectrum(dsp.DefaultSpectrum(ep.rate))
	if err != nil {
		e.log.Error("shared spectrum unavailable", slog.Any("error", err))

		return
	}
	defer s.Close()

	size := s.Config().Size
	scale := rxv1.DefaultFFTU8Scale()
	rd := ep.iq.NewReader()

	emit := func(l dsp.Line) {
		frame := app.SpectrumFrame{
			Payload:     dsp.EncodeFFTU8(make([]byte, 0, rxv1.FFTU8PrefixSize+size), scale, l.DB),
			TimestampUS: uint64(max(l.Time.UnixMicro(), 0)),
		}

		e.mu.Lock()
		subs := slices.Collect(maps.Values(e.subs))
		e.mu.Unlock()

		for _, deliver := range subs {
			deliver(frame)
		}
	}

	for {
		meta, iq, _, err := rd.Read(ctx)
		if err != nil {
			return
		}

		if err := s.Push(meta.Index, meta.Time, iq, emit); err != nil {
			e.log.Error("shared spectrum failed", slog.Any("error", err))

			return
		}
	}
}

func (e *Engine) runChannelizer(ctx context.Context, ep *epoch) {
	c := dsp.NewChannelizer(ep.plan)
	rd := ep.iq.NewReader()
	buf := make([]complex64, 0, ep.plan.L/ep.plan.D)

	for {
		meta, iq, gap, err := rd.Read(ctx)
		if err != nil {
			return
		}

		if gap != nil {
			c.Reset()
		}

		e.mu.Lock()
		demods := slices.Collect(maps.Keys(e.demods))
		e.mu.Unlock()

		c.Push(meta.Index, iq, func(first uint64, bins []complex128) {
			at := meta.Time.Add(time.Duration((float64(first) - float64(meta.Index)) / float64(ep.rate) * float64(time.Second)))

			for _, d := range demods {
				buf = d.extract(ep, bins, first, at, buf[:0])
			}
		})
	}
}

// NewDemod implements app.Engine. Its channel is validated against the
// device tuning and built outside the engine lock.
func (e *Engine) NewDemod(p app.DemodParams, audio func(app.AudioOut), meter func(app.Meter)) (app.Demod, error) {
	p, _, err := normalize(p)
	if err != nil {
		return nil, err
	}

	d := &demod{e: e, audio: audio, meter: meter, params: p, wake: make(chan struct{}, 1), done: make(chan struct{})}

	e.mu.Lock()
	closed, ep, tuning := e.closed, e.ep, e.tuning
	e.mu.Unlock()

	if closed {
		return nil, domain.ErrDeviceUnavailable
	}

	var b *binding

	switch {
	case ep != nil:
		nb, err := newBinding(ep, p)
		if err != nil {
			return nil, err
		}

		b = nb
	case tuning.Rate().PerSecond() > 0:
		if err := checkChannel(tuning.Rate().PerSecond(), p); err != nil {
			return nil, err
		}
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()

		return nil, domain.ErrDeviceUnavailable
	}

	e.demods[d] = struct{}{}
	current := e.ep

	if b != nil && b.ep == current {
		d.setBinding(b)
	}

	e.reconcileLocked()
	e.wg.Go(d.run)
	e.mu.Unlock()

	// A run started meanwhile: bind to it.
	if current != nil && (b == nil || b.ep != current) {
		e.bind(d, current)
	}

	return d, nil
}

// checkChannel validates the channel of p at a device rate before the
// device runs.
func checkChannel(rate int, p app.DemodParams) error {
	plan, err := dsp.NewChannelPlan(rate, dsp.NarrowChannelRate)
	if err != nil {
		return domain.ErrDeviceUnavailable.WithDetail("no demodulator at this sample rate")
	}

	_, err = newChannel(plan, p)

	return err
}

// newChannel builds the channel of a normalized p at the rate of its mode
// (§8.3 rule 5: a mode must fit the device sample rate).
func newChannel(plan dsp.ChannelPlan, p app.DemodParams) (*dsp.Channel, error) {
	m, _ := modeOf(p.Mode)

	if float64(plan.SampleRate) < m.ChannelRate {
		return nil, domain.ErrOutOfRange.WithDetail("mode " + m.Name + " does not fit the device sample rate")
	}

	ch, err := plan.NewChannel(m.ChannelRate, float64(p.OffsetHz), p.LowHz, p.HighHz)
	if err != nil {
		return nil, domain.ErrOutOfRange.WithDetail(err.Error())
	}

	return ch, nil
}

func (e *Engine) removeDemod(d *demod) {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.demods, d)
	e.reconcileLocked()
}
