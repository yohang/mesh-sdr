package engine

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
)

// Mode is one analog mode of the node's catalogue (DEM-001…DEM-008,
// DEM-015): its demodulator, its default pass band and the limits of the
// band edges (Hz, relative to the offset), squelch support, the AGC
// profile of its family and the channel rate it needs.
type Mode struct {
	Name  string
	Demod dsp.Demodulator
	// Low and High are the default pass band.
	Low, High float64
	// MinHz and MaxHz bound the pass band edges.
	MinHz, MaxHz float64
	// Squelch is false for modes whose squelch is forced open (−150 dB).
	Squelch bool
	AGC     dsp.AGCProfile
	// ChannelRate is the minimum channel rate.
	ChannelRate float64
	// HD modes need an HD output rate (dsp.HDOutputRates).
	HD bool
}

// AGC profile of each analog family (DEM-009: one constant per family,
// the settings that would choose them are not wired).
const (
	agcAM  = dsp.AGCSlow
	agcSSB = dsp.AGCFast
	agcNFM = dsp.AGCSlow
)

// modes is the analog mode catalogue. CW is the SSB chain (DEM-005): its
// default pass band sits around an 800 Hz tone, the BFO offset.
var modes = []Mode{
	{Name: "am", Demod: dsp.DemodAM, Low: -4000, High: 4000, MinHz: -10000, MaxHz: 10000, Squelch: true, AGC: agcAM, ChannelRate: dsp.NarrowChannelRate},
	{Name: "sam", Demod: dsp.DemodSAM, Low: -4000, High: 4000, MinHz: -10000, MaxHz: 10000, Squelch: true, AGC: agcAM, ChannelRate: dsp.NarrowChannelRate},
	{Name: "nfm", Demod: dsp.DemodNFM, Low: -4000, High: 4000, MinHz: -10000, MaxHz: 10000, Squelch: true, AGC: agcNFM, ChannelRate: dsp.NarrowChannelRate},
	{Name: "usb", Demod: dsp.DemodSSB, Low: 300, High: 2700, MinHz: 0, MaxHz: 5000, Squelch: true, AGC: agcSSB, ChannelRate: dsp.NarrowChannelRate},
	{Name: "lsb", Demod: dsp.DemodSSB, Low: -2700, High: -300, MinHz: -5000, MaxHz: 0, Squelch: true, AGC: agcSSB, ChannelRate: dsp.NarrowChannelRate},
	{Name: "cw", Demod: dsp.DemodSSB, Low: 650, High: 950, MinHz: -2000, MaxHz: 2000, Squelch: true, AGC: agcSSB, ChannelRate: dsp.NarrowChannelRate},
	{Name: "wfm", Demod: dsp.DemodWFM, Low: -75000, High: 75000, MinHz: -90000, MaxHz: 90000, Squelch: true, AGC: dsp.AGCOff, ChannelRate: dsp.WideChannelRate, HD: true},
}

// MinBandwidth is the narrowest pass band (RX-020).
const MinBandwidth = 100.0

// Modes returns the names of the analog modes, for the capability report.
func Modes() []string {
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = m.Name
	}

	return out
}

func modeOf(name string) (Mode, bool) {
	i := slices.IndexFunc(modes, func(m Mode) bool { return m.Name == name })
	if i < 0 {
		return Mode{}, false
	}

	return modes[i], true
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// normalize validates p and applies the mode rules (§8.3 rule 4, DEM-006,
// DEM-008): the default pass band when none is given (LowHz == HighHz ==
// 0), edges clamped to the mode limits, squelch forced open for modes
// without squelch.
func normalize(p app.DemodParams) (app.DemodParams, Mode, error) {
	m, ok := modeOf(p.Mode)

	switch {
	case !ok:
		return p, m, domain.ErrUnsupportedMode.WithDetail("mode " + strconv.Quote(p.Mode) + " is not supported by this node")
	case !slices.Contains(dsp.OutputRates, p.OutputRate):
		return p, m, domain.ErrOutOfRange.WithDetail("output rate " + strconv.Itoa(p.OutputRate) + " is not supported")
	case m.HD && !slices.Contains(dsp.HDOutputRates, p.OutputRate):
		return p, m, domain.ErrOutOfRange.WithDetail("mode " + m.Name + " needs 44100 or 48000 Hz audio (audio.configure)")
	case p.Codec != app.CodecPCM && p.Codec != app.CodecADPCM:
		return p, m, domain.ErrOutOfRange.WithDetail("audio codec " + strconv.Quote(string(p.Codec)) + " is not supported")
	case !finite(p.LowHz) || !finite(p.HighHz):
		return p, m, domain.ErrOutOfRange.WithDetail("bandpass: want finite edges")
	}

	if p.LowHz == 0 && p.HighHz == 0 {
		p.LowHz, p.HighHz = m.Low, m.High
	}

	if p.LowHz >= p.HighHz {
		return p, m, domain.ErrOutOfRange.WithDetail("bandpass: want low < high")
	}

	p.LowHz = min(max(p.LowHz, m.MinHz), m.MaxHz)
	p.HighHz = min(max(p.HighHz, m.MinHz), m.MaxHz)

	if p.HighHz-p.LowHz < MinBandwidth {
		return p, m, domain.ErrOutOfRange.WithDetail("bandpass: want at least 100 Hz within the mode limits")
	}

	if !m.Squelch {
		open := float64(dsp.SquelchMin)
		p.SquelchDB = &open
	}

	switch {
	case p.SquelchDB != nil && (!finite(*p.SquelchDB) || *p.SquelchDB < dsp.SquelchMin || *p.SquelchDB > dsp.SquelchMax):
		return p, m, domain.ErrOutOfRange.WithDetail("squelch: want -150..0 dBFS")
	case !finite(p.NR.ThresholdDB) || p.NR.ThresholdDB < dsp.NRThresholdMin || p.NR.ThresholdDB > dsp.NRThresholdMax:
		return p, m, domain.ErrOutOfRange.WithDetail("nr threshold: want -20..20 dB")
	}

	return p, m, nil
}

func appCodec(c rxv1.Codec) app.AudioCodec {
	if c == rxv1.CodecADPCMIMA {
		return app.CodecADPCM
	}

	return app.CodecPCM
}

func codecOf(c app.AudioCodec) rxv1.Codec {
	if c == app.CodecADPCM {
		return rxv1.CodecADPCMIMA
	}

	return rxv1.CodecPCMS16LE
}

// binding ties a demodulator to one channel of a run.
type binding struct {
	ep   *epoch
	ch   *dsp.Channel
	ring *dsp.Ring[complex64]
}

type demod struct {
	e     *Engine
	audio func(app.AudioOut)
	meter func(app.Meter)
	wake  chan struct{}
	done  chan struct{}
	once  sync.Once

	mu     sync.Mutex
	params app.DemodParams
	gen    int
	b      *binding
}

func (d *demod) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// newBinding builds the channel of p in ep, validating the offset and the
// pass band against the channel plan (p is normalized).
func newBinding(ep *epoch, p app.DemodParams) (*binding, error) {
	if ep.plan.N == 0 {
		return nil, domain.ErrDeviceUnavailable.WithDetail("no demodulator at this sample rate")
	}

	ch, err := newChannel(ep.plan, p)
	if err != nil {
		return nil, err
	}

	ring := dsp.NewRing[complex64](ringSlots(ep.rate, ep.plan.L), ch.BlockLen())

	return &binding{ep: ep, ch: ch, ring: ring}, nil
}

func (d *demod) setBinding(b *binding) {
	d.mu.Lock()
	old := d.b
	d.b = b
	d.mu.Unlock()

	if old != nil {
		old.ring.Close()
	}

	d.poke()
}

// extract runs in the channelizer goroutine.
func (d *demod) extract(ep *epoch, bins []complex128, first uint64, at time.Time, buf []complex64) []complex64 {
	d.mu.Lock()
	b := d.b
	d.mu.Unlock()

	if b == nil || b.ep != ep {
		return buf
	}

	out := b.ch.Process(bins, buf)
	b.ring.Push(first, ep.plan.L, at, out)

	return out
}

// Set implements app.Demod. A new channel is built outside the engine
// lock and installed if the run did not change meanwhile.
func (d *demod) Set(p app.DemodParams) error {
	p, m, err := normalize(p)
	if err != nil {
		return err
	}

	e := d.e

	e.mu.Lock()
	ep, tuning := e.ep, e.tuning
	e.mu.Unlock()

	// A new sample rate waits for the restart of the source (Retuned before
	// Start): the parameters are checked against the new rate, and Start
	// binds the channel in the new run.
	if ep != nil && ep.rate != tuning.Rate().PerSecond() {
		ep = nil
	}

	old := d.Params()
	oldMode, _ := modeOf(old.Mode)
	channel := p.OffsetHz != old.OffsetHz || p.LowHz != old.LowHz || p.HighHz != old.HighHz || m.ChannelRate != oldMode.ChannelRate

	var b *binding

	switch {
	case channel && ep != nil:
		nb, err := newBinding(ep, p)
		if err != nil {
			return err
		}

		b = nb
	case channel && tuning.Rate().PerSecond() > 0:
		if err := checkChannel(tuning.Rate().PerSecond(), p); err != nil {
			return err
		}
	}

	// The parameters and their channel are published in one step, so the
	// run loop never pairs the new parameters with the old channel. When
	// the channel cannot be installed now (the run changed meanwhile), the
	// old one is dropped and bind builds the new one.
	e.mu.Lock()
	current := e.ep

	if current != nil && current.rate != e.tuning.Rate().PerSecond() {
		current = nil
	}

	_, live := e.demods[d]
	install := channel && live && b != nil && b.ep == current

	d.mu.Lock()
	d.params = p
	d.gen++

	var stale *binding
	if channel {
		stale = d.b
		d.b = nil

		if install {
			d.b = b
		}
	}
	d.mu.Unlock()
	e.mu.Unlock()

	if stale != nil {
		stale.ring.Close()
	}

	d.poke()

	if channel && !install && live && current != nil {
		e.bind(d, current)
	}

	return nil
}

// state returns the parameters and their generation.
func (d *demod) state() (app.DemodParams, int) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.params, d.gen
}

// installFor installs b if the parameters are still those of generation
// gen; it reports whether it did.
func (d *demod) installFor(b *binding, gen int) bool {
	d.mu.Lock()
	if d.gen != gen {
		d.mu.Unlock()

		return false
	}

	old := d.b
	d.b = b
	d.mu.Unlock()

	if old != nil {
		old.ring.Close()
	}

	d.poke()

	return true
}

// Params implements app.Demod.
func (d *demod) Params() app.DemodParams {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.params
}

// Close implements app.Demod.
func (d *demod) Close() {
	d.once.Do(func() {
		close(d.done)
		d.e.removeDemod(d)
		d.setBinding(nil)
	})
}

// chainKey are the parameters that need a new chain. The pass band only
// counts for the chains that filter it again (SSB, CW).
type chainKey struct {
	mode      string
	rate      int
	codec     app.AudioCodec
	ep        *epoch
	low, high float64
}

// run is the listener goroutine: channel IQ → chain → framing.
func (d *demod) run() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-d.done
		cancel()
	}()

	var (
		chain     *dsp.Chain
		framer    *dsp.Framer
		key       chainKey
		reader    *dsp.Reader[complex64]
		readerOf  *binding
		builtGen  = -1
		builtFor  *binding
		lastMeter time.Time
		gap       bool
	)

	defer func() {
		if chain != nil {
			chain.Close()
		}
	}()

	for {
		d.mu.Lock()
		b, p, gen := d.b, d.params, d.gen
		d.mu.Unlock()

		if b != readerOf {
			readerOf, reader = b, nil
			if b != nil {
				reader = b.ring.NewReader()
			}
		}

		if reader == nil {
			select {
			case <-d.wake:
				continue
			case <-d.done:
				return
			}
		}

		if gen != builtGen || builtFor != b {
			chain, framer, key = d.rebuild(chain, framer, key, b, p)
			builtGen, builtFor = gen, b
		}

		meta, iq, g, err := reader.Read(ctx)
		if errors.Is(err, dsp.ErrRingClosed) {
			reader, readerOf = nil, nil

			continue
		}

		if err != nil {
			return
		}

		if chain == nil {
			continue
		}

		gap = gap || g != nil

		// The hub setting applies live (no chain rebuild).
		if us := d.e.deemphasisUS(); us != chain.Config().DeemphasisUS {
			if err := chain.SetDeemphasis(us); err != nil {
				d.e.log.Warn("wfm de-emphasis not applied", slog.Any("error", err))
			}
		}

		res, err := chain.Process(iq)
		if err != nil {
			d.e.log.Error("demodulator failed", slog.Any("error", err))

			continue
		}

		framer.Push(res.Audio, meta.Time, !res.Open, func(f dsp.AudioFrame) {
			d.audio(app.AudioOut{
				Codec:   appCodec(f.Codec),
				Payload: f.Payload, Samples: f.Samples, Duration: f.Duration(framer.Rate()),
				TimestampUS: uint64(max(f.Time.UnixMicro(), 0)), Squelched: f.Squelched, Reset: f.Reset, Discontinuity: gap,
			})
			gap = false
		})

		if now := time.Now(); now.Sub(lastMeter) >= MeterInterval {
			lastMeter = now
			d.meter(app.Meter{LevelDB: res.LevelDB, Open: res.Open})
		}
	}
}

// rebuild applies new parameters: a new chain when the mode, rate, codec or
// channel changed (built before the old one is released, §8.3 rule 3),
// otherwise a live update of the squelch and the NR.
func (d *demod) rebuild(chain *dsp.Chain, framer *dsp.Framer, key chainKey, b *binding, p app.DemodParams) (*dsp.Chain, *dsp.Framer, chainKey) {
	if b == nil {
		return chain, framer, key
	}

	m, _ := modeOf(p.Mode)
	next := chainKey{mode: p.Mode, rate: p.OutputRate, codec: p.Codec, ep: b.ep}

	if m.Demod == dsp.DemodSSB {
		next.low, next.high = p.LowHz, p.HighHz
	}

	nr := dsp.NR{Enabled: p.NR.Enabled, ThresholdDB: p.NR.ThresholdDB}

	// Same chain: a new offset only moves the residual shift (no AGC or
	// filter reset while tuning).
	if chain != nil && next == key {
		if err := chain.SetResidual(b.ch.Residual()); err != nil {
			d.e.log.Warn("offset not applied", slog.Any("error", err))
		}

		if err := chain.SetSquelch(p.SquelchDB); err != nil {
			d.e.log.Warn("squelch not applied", slog.Any("error", err))
		}

		if err := chain.SetNR(nr); err != nil {
			d.e.log.Warn("noise reduction not applied", slog.Any("error", err))
		}

		return chain, framer, key
	}

	nc, err := dsp.NewChain(dsp.ChainConfig{
		Demod: m.Demod, ChannelRate: b.ch.Rate(), OutputRate: p.OutputRate, ResidualHz: b.ch.Residual(),
		LowHz: p.LowHz, HighHz: p.HighHz, Squelch: p.SquelchDB, AGC: m.AGC, NR: nr, DeemphasisUS: d.e.deemphasisUS(),
	})
	if err != nil {
		d.e.log.Error("demodulator chain not rebuilt, keeping the previous one", slog.Any("error", err))

		return chain, framer, key
	}

	if framer == nil || next.rate != key.rate || next.codec != key.codec {
		nf, err := dsp.NewFramer(codecOf(p.Codec), p.OutputRate)
		if err != nil {
			nc.Close()
			d.e.log.Error("audio framing not rebuilt", slog.Any("error", err))

			return chain, framer, key
		}

		framer = nf
	}

	if chain != nil {
		chain.Close()
	}

	return nc, framer, next
}
