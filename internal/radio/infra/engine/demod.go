package engine

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
)

// ModeNFM is the only demodulator of this node (DEM-001).
const ModeNFM = "nfm"

func validate(p app.DemodParams) error {
	switch {
	case p.Mode != ModeNFM:
		return domain.ErrUnsupportedMode.WithDetail("mode " + strconv.Quote(p.Mode) + " is not supported by this node")
	case !slices.Contains(dsp.OutputRates, p.OutputRate):
		return domain.ErrOutOfRange.WithDetail("output rate " + strconv.Itoa(p.OutputRate) + " is not supported")
	case p.Codec != app.CodecPCM && p.Codec != app.CodecADPCM:
		return domain.ErrOutOfRange.WithDetail("audio codec " + strconv.Quote(string(p.Codec)) + " is not supported")
	case p.LowHz >= p.HighHz:
		return domain.ErrOutOfRange.WithDetail("bandpass: want low < high")
	case p.SquelchDB != nil && (*p.SquelchDB < -150 || *p.SquelchDB > 0):
		return domain.ErrOutOfRange.WithDetail("squelch: want -150..0 dBFS")
	}

	return nil
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
// pass band against the channel plan.
func newBinding(ep *epoch, p app.DemodParams) (*binding, error) {
	if ep.plan.N == 0 {
		return nil, domain.ErrDeviceUnavailable.WithDetail("no demodulator at this sample rate")
	}

	ch, err := ep.plan.NewChannel(float64(p.OffsetHz), p.LowHz, p.HighHz)
	if err != nil {
		return nil, domain.ErrOutOfRange.WithDetail(err.Error())
	}

	ring := dsp.NewRing[complex64](ringSlots(ep.rate, ep.plan.L), ep.plan.L/ep.plan.D)

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
	if err := validate(p); err != nil {
		return err
	}

	e := d.e

	e.mu.Lock()
	ep, tuning := e.ep, e.tuning
	e.mu.Unlock()

	old := d.Params()
	channel := p.OffsetHz != old.OffsetHz || p.LowHz != old.LowHz || p.HighHz != old.HighHz

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

	d.mu.Lock()
	d.params = p
	d.gen++
	d.mu.Unlock()

	if !channel {
		d.poke()

		return nil
	}

	e.mu.Lock()
	current := e.ep
	_, live := e.demods[d]

	if live && b != nil && b.ep == current {
		d.setBinding(b)
	}
	e.mu.Unlock()

	if live && current != nil && (b == nil || b.ep != current) {
		e.bind(d, current)
	}

	return nil
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

// chainKey are the parameters that need a new chain.
type chainKey struct {
	mode  string
	rate  int
	codec app.AudioCodec
	ep    *epoch
}

// run is the listener goroutine: channel IQ → NFM chain → framing.
func (d *demod) run() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-d.done
		cancel()
	}()

	var (
		chain     *dsp.NFM
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

		res, err := chain.Process(iq)
		if err != nil {
			d.e.log.Error("demodulator failed", slog.Any("error", err))

			continue
		}

		framer.Push(res.Audio, meta.Time, !res.Open, func(f dsp.AudioFrame) {
			d.audio(app.AudioOut{
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
// otherwise a live update of the squelch.
func (d *demod) rebuild(chain *dsp.NFM, framer *dsp.Framer, key chainKey, b *binding, p app.DemodParams) (*dsp.NFM, *dsp.Framer, chainKey) {
	if b == nil {
		return chain, framer, key
	}

	next := chainKey{mode: p.Mode, rate: p.OutputRate, codec: p.Codec, ep: b.ep}

	// Same chain: a new offset only moves the residual shift (no AGC or
	// filter reset while tuning).
	if chain != nil && next == key {
		if err := chain.SetResidual(b.ch.Residual()); err != nil {
			d.e.log.Warn("offset not applied", slog.Any("error", err))
		}

		if err := chain.SetSquelch(p.SquelchDB); err != nil {
			d.e.log.Warn("squelch not applied", slog.Any("error", err))
		}

		return chain, framer, key
	}

	nc, err := dsp.NewNFM(dsp.NFMConfig{
		ChannelRate: b.ep.plan.ChannelRate(), OutputRate: p.OutputRate, ResidualHz: b.ch.Residual(), Squelch: p.SquelchDB, AGC: dsp.AGCSlow,
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
