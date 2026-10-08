package engine

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/dsp/csdr"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
)

// The wide IQ tap of a demodulator (DEC-039, DEC-040, DEC-013): while a
// decoder whose input is wide IQ runs, the demodulator gets a second
// channel of the device channelizer at the same offset, resampled to the
// exact rate of the decoder. The listener keeps hearing the narrow
// demodulator. The channelizer goroutine only extracts the channel into a
// bounded ring (drop oldest, gap marker for the reader, like the narrow
// channel); the tap's own goroutine builds the channel (off the
// channelizer, for each run and offset), resamples and delivers it. The
// channel exists only while the tap does.

// wideChannel is the wide channel of one run and offset.
type wideChannel struct {
	ep     *epoch
	offset int64
	ch     *dsp.Channel
	ring   *dsp.Ring[complex64]
}

// wideWant is the run and offset the channelizer last saw.
type wideWant struct {
	ep     *epoch
	offset int64
}

// wideTap is the wide IQ tap of a demodulator.
type wideTap struct {
	rate      int
	low, high float64
	fn        func(app.WideIQBlock)
	fail      func(reason string)
	log       *slog.Logger
	wake      chan struct{}
	cancel    context.CancelFunc
	once      sync.Once

	mu   sync.Mutex
	want wideWant
	cur  *wideChannel

	// buf is the channelizer goroutine's output buffer.
	buf []complex64
}

// band returns the pass band of the tap cut to its rate.
func (w *wideTap) band() (float64, float64) {
	half := float64(w.rate)/2 - dsp.Transition/2

	return max(w.low, -half), min(w.high, half)
}

// fits checks the band at offset within a device of rate devRate.
func (w *wideTap) fits(devRate int, offset int64) error {
	if devRate <= 0 {
		return nil
	}

	if devRate < w.rate {
		return domain.ErrOutOfRange.WithDetail("the device sample rate (" + strconv.Itoa(devRate) + " Hz) is below the " + strconv.Itoa(w.rate) +
			" Hz this decoder needs")
	}

	low, high := w.band()
	if edge := math.Max(math.Abs(float64(offset)+low), math.Abs(float64(offset)+high)); edge > float64(devRate)/2 {
		return domain.ErrOutOfRange.WithDetail("the decoder band leaves the device span: tune within ±" + strconv.Itoa(devRate/2-int(math.Max(-low, high))) +
			" Hz of the centre")
	}

	return nil
}

// TapWideIQ implements app.Demod.
func (d *demod) TapWideIQ(rate int, low, high float64, fn func(app.WideIQBlock), fail func(string)) (func(), error) {
	if rate <= 0 || !(low < high) {
		return nil, domain.ErrOutOfRange.WithDetail("invalid wide IQ tap")
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &wideTap{rate: rate, low: low, high: high, fn: fn, fail: fail, log: d.e.log, wake: make(chan struct{}, 1), cancel: cancel}

	d.e.mu.Lock()
	devRate := d.e.tuning.Rate().PerSecond()
	if d.e.ep != nil {
		devRate = d.e.ep.rate
	}
	d.e.mu.Unlock()

	if err := w.fits(devRate, d.Params().OffsetHz); err != nil {
		cancel()

		return nil, err
	}

	d.mu.Lock()
	old := d.wide
	d.wide = w
	d.mu.Unlock()

	if old != nil {
		old.close()
	}

	d.e.wg.Go(func() { w.run(ctx) })

	return func() {
		d.mu.Lock()
		if d.wide == w {
			d.wide = nil
		}
		d.mu.Unlock()

		w.close()
	}, nil
}

// close stops the tap's goroutine and drops its channel.
func (w *wideTap) close() {
	w.once.Do(func() {
		w.cancel()

		w.mu.Lock()
		if w.cur != nil {
			w.cur.ring.Close()
			w.cur = nil
		}
		w.mu.Unlock()
	})
}

func (w *wideTap) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// push runs in the channelizer goroutine: the channel of the block's run
// and offset into the ring. A new run or offset drops the channel and asks
// the tap's goroutine for a new one.
func (w *wideTap) push(ep *epoch, offset int64, bins []complex128, first uint64, at time.Time) {
	w.mu.Lock()

	if want := (wideWant{ep: ep, offset: offset}); w.want != want {
		w.want = want

		if w.cur != nil {
			w.cur.ring.Close()
			w.cur = nil
		}

		w.mu.Unlock()
		w.poke()

		return
	}

	c := w.cur
	w.mu.Unlock()

	if c == nil {
		return
	}

	w.buf = c.ch.Process(bins, w.buf[:0])
	c.ring.Push(first, ep.plan.L, at, w.buf)
}

// build makes the channel of want, off the channelizer goroutine.
func (w *wideTap) build(want wideWant) (*wideChannel, error) {
	if err := w.fits(want.ep.rate, want.offset); err != nil {
		return nil, err
	}

	low, high := w.band()

	ch, err := want.ep.plan.NewChannel(float64(w.rate), float64(want.offset), low, high)
	if err != nil {
		return nil, err
	}

	return &wideChannel{ep: want.ep, offset: want.offset, ch: ch, ring: dsp.NewRing[complex64](ringSlots(want.ep.rate, want.ep.plan.L), ch.BlockLen())}, nil
}

// resampler resamples the channel IQ to the tap rate (I and Q apart).
type resampler struct {
	in      float64
	i, q    *csdr.Stage[float32, float32]
	re, im  []float32
	oi, oq  []float32
	out     []complex64
	outRate int
}

func newResampler(in float64, out int) (*resampler, error) {
	r := &resampler{in: in, outRate: out}
	if in == float64(out) {
		return r, nil
	}

	i, err := csdr.NewResampler(in, out)
	if err != nil {
		return nil, err
	}

	q, err := csdr.NewResampler(in, out)
	if err != nil {
		i.Close()

		return nil, err
	}

	r.i, r.q = i, q

	return r, nil
}

func (r *resampler) close() {
	if r != nil && r.i != nil {
		r.i.Close()
		r.q.Close()
	}
}

func (r *resampler) process(iq []complex64) ([]complex64, error) {
	if r.i == nil {
		return iq, nil
	}

	r.re, r.im = r.re[:0], r.im[:0]
	for _, v := range iq {
		r.re = append(r.re, real(v))
		r.im = append(r.im, imag(v))
	}

	n := int(float64(len(iq)+r.i.Pending())*float64(r.outRate)/r.in) + 64
	r.oi, r.oq = dsp.Grow(r.oi, n), dsp.Grow(r.oq, n)

	ni, err := r.i.Process(r.re, r.oi)
	if err != nil {
		return nil, err
	}

	nq, err := r.q.Process(r.im, r.oq)
	if err != nil {
		return nil, err
	}

	r.out = r.out[:0]
	for k := range min(ni, nq) {
		r.out = append(r.out, complex(r.oi[k], r.oq[k]))
	}

	return r.out, nil
}

// run is the tap's goroutine: it builds the channel the channelizer asks
// for, then reads, resamples and delivers its blocks.
func (w *wideTap) run(ctx context.Context) {
	var (
		reader *dsp.Reader[complex64]
		readOf *wideChannel
		rs     *resampler
		tried  wideWant
		gap    bool
	)

	defer func() { rs.close() }()

	for {
		w.mu.Lock()
		cur, want := w.cur, w.want
		w.mu.Unlock()

		if cur == nil {
			if want.ep == nil || want == tried {
				select {
				case <-ctx.Done():
					return
				case <-w.wake:
				}

				continue
			}

			tried = want

			c, err := w.build(want)
			if err != nil {
				w.log.Warn("wide decoder channel not built", slog.Int("rate", w.rate), slog.Any("error", err))

				if w.fail != nil {
					w.fail(failReason(err))
				}

				continue
			}

			w.mu.Lock()
			if w.want == want && ctx.Err() == nil {
				w.cur = c
			} else {
				c.ring.Close()
			}
			w.mu.Unlock()

			continue
		}

		if cur != readOf {
			readOf, reader = cur, cur.ring.NewReader()
			gap = true

			if rs == nil || rs.in != cur.ch.Rate() {
				rs.close()

				nr, err := newResampler(cur.ch.Rate(), w.rate)
				if err != nil {
					w.log.Warn("wide decoder resampler not built", slog.Any("error", err))

					if w.fail != nil {
						w.fail("the decoder input could not be resampled")
					}

					rs = nil

					return
				}

				rs = nr
			}
		}

		meta, iq, g, err := reader.Read(ctx)

		switch {
		case errors.Is(err, dsp.ErrRingClosed):
			readOf, reader = nil, nil

			continue
		case err != nil:
			return
		}

		out, err := rs.process(iq)
		if err != nil {
			w.log.Warn("wide decoder resampling failed", slog.Any("error", err))

			continue
		}

		gap = gap || g != nil

		if len(out) > 0 {
			w.fn(app.WideIQBlock{Samples: out, Rate: w.rate, Time: meta.Time, Discontinuity: gap})
			gap = false
		}
	}
}

// failReason is the reason shown for a channel that cannot be built.
func failReason(err error) string {
	var de interface{ Message() string }
	if errors.As(err, &de) {
		return de.Message()
	}

	return "the decoder channel could not be built"
}
