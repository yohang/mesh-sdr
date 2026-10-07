package dsp

import (
	"errors"
	"fmt"
	"math"
	"math/cmplx"

	"gonum.org/v1/gonum/dsp/fourier"
)

// The shared FFT channelizer of a device (ADR 0014 decision 6, option d4):
// an overlap-save fast-convolution filter bank. The device runs one forward
// FFT of size N per block of L new samples (Channelizer); each listener
// channel takes the M = N/D bins around its frequency, applies its filter
// in the frequency domain and runs an inverse FFT of size M at the channel
// rate fs/D (Channel). The per-listener cost does not grow with the device
// sample rate.

// ErrChannelizer reports parameters the channelizer cannot serve.
var ErrChannelizer = errors.New("dsp: invalid channelizer parameters")

// ChannelPlan is the geometry of a device channelizer.
type ChannelPlan struct {
	// SampleRate is the device rate fs.
	SampleRate int
	// D is the decimation: channels run at fs/D.
	D int
	// N is the forward FFT size, M = N/D the inverse one.
	N, M int
	// V is the overlap (samples kept from the previous block), L = N − V
	// the new samples per block. Both are multiples of D.
	V, L int
}

// ChannelRate returns fs/D.
func (p ChannelPlan) ChannelRate() float64 { return float64(p.SampleRate) / float64(p.D) }

// BinHz returns the forward bin spacing fs/N.
func (p ChannelPlan) BinHz() float64 { return float64(p.SampleRate) / float64(p.N) }

// MaxTaps returns the longest filter overlap-save can apply (V + 1).
func (p ChannelPlan) MaxTaps() int { return p.V + 1 }

// Transition is the filter transition width the plan is sized for.
const Transition = 3000.0

// blackmanTransition is the transition width of a Blackman-windowed sinc,
// in units of fs/taps.
const blackmanTransition = 5.5

// NewChannelPlan sizes a channelizer for fs so that channels run at no
// less than minRate and their filters reach a Transition-wide edge.
func NewChannelPlan(sampleRate int, minRate float64) (ChannelPlan, error) {
	if sampleRate <= 0 || minRate <= 0 || float64(sampleRate) < minRate {
		return ChannelPlan{}, fmt.Errorf("%w: rate %d, channel rate %g", ErrChannelizer, sampleRate, minRate)
	}

	d := max(1, int(float64(sampleRate)/minRate))
	taps := int(math.Ceil(blackmanTransition * float64(sampleRate) / Transition))
	mv := max(1, (taps+d-1)/d)
	v := mv * d
	l := 3 * v

	return ChannelPlan{SampleRate: sampleRate, D: d, N: v + l, M: (v + l) / d, V: v, L: l}, nil
}

// Channelizer runs the shared forward FFT of a device.
type Channelizer struct {
	plan ChannelPlan
	fft  *fourier.CmplxFFT
	in   []complex128
	fill int
	out  []complex128
	// index is the sample index of the first new sample of the block being
	// filled.
	index   uint64
	started bool
}

// NewChannelizer returns a channelizer for plan.
func NewChannelizer(plan ChannelPlan) *Channelizer {
	return &Channelizer{
		plan: plan,
		fft:  fourier.NewCmplxFFT(plan.N),
		in:   make([]complex128, plan.N),
		fill: plan.V,
		out:  make([]complex128, plan.N),
	}
}

// Plan returns the geometry.
func (c *Channelizer) Plan() ChannelPlan { return c.plan }

// Reset drops the overlap, after a gap in the input.
func (c *Channelizer) Reset() {
	clear(c.in)
	c.fill = c.plan.V
	c.started = false
}

// Push feeds IQ starting at sample index. emit receives the spectrum of
// every completed block (valid during the call only) with the sample index
// of the block's first new sample.
func (c *Channelizer) Push(index uint64, iq []complex64, emit func(first uint64, bins []complex128)) {
	for len(iq) > 0 {
		if !c.started || c.fill == c.plan.V {
			c.index = index
			c.started = true
		}

		n := min(len(iq), c.plan.N-c.fill)
		for i, v := range iq[:n] {
			c.in[c.fill+i] = complex128(v)
		}

		c.fill += n
		iq = iq[n:]
		index += uint64(n)

		if c.fill == c.plan.N {
			c.fft.Coefficients(c.out, c.in)
			emit(c.index, c.out)
			copy(c.in, c.in[c.plan.L:])
			c.fill = c.plan.V
		}
	}
}

// Channel extracts one listener channel from the channelizer blocks.
type Channel struct {
	plan ChannelPlan
	// d is the decimation of this channel (a divisor of plan.D), m = N/d
	// its inverse FFT size.
	d, m int
	k0   int
	h    []complex128
	sel  []complex128
	seq  []complex128
	ifft *fourier.CmplxFFT
	// rot is the per-block phase correction of the bin shift, phase the
	// accumulated one.
	rot, phase complex128
	residual   float64
}

// Decimation returns the decimation of a channel running at no less than
// minRate: the largest divisor of D that keeps fs/d ≥ minRate, so that the
// channel shares the block geometry of the plan (wide channels, such as
// broadcast FM, run at a higher rate than the plan's).
func (p ChannelPlan) Decimation(minRate float64) (int, error) {
	if minRate <= 0 || float64(p.SampleRate) < minRate {
		return 0, fmt.Errorf("%w: channel rate %g above the device rate %d", ErrChannelizer, minRate, p.SampleRate)
	}

	for d := p.D; d > 1; d-- {
		if p.D%d == 0 && float64(p.SampleRate)/float64(d) >= minRate {
			return d, nil
		}
	}

	return 1, nil
}

// NewChannel returns the channel running at no less than minRate, centred
// offsetHz from the device centre, with the pass band [lowHz, highHz]
// relative to offsetHz. The band must fit in the channel rate minus the
// transition.
func (p ChannelPlan) NewChannel(minRate, offsetHz, lowHz, highHz float64) (*Channel, error) {
	d, err := p.Decimation(minRate)
	if err != nil {
		return nil, err
	}

	rate := float64(p.SampleRate) / float64(d)
	half := rate/2 - Transition/2

	if lowHz >= highHz || lowHz < -half || highHz > half || math.Abs(offsetHz) > float64(p.SampleRate)/2 {
		return nil, fmt.Errorf("%w: offset %g band [%g, %g] at channel rate %g", ErrChannelizer, offsetHz, lowHz, highHz, rate)
	}

	m := p.N / d
	ch := &Channel{
		plan:  p,
		d:     d,
		m:     m,
		sel:   make([]complex128, m),
		seq:   make([]complex128, m),
		ifft:  fourier.NewCmplxFFT(m),
		phase: 1,
	}
	ch.tune(offsetHz, lowHz, highHz)

	return ch, nil
}

// Rate returns the channel sample rate fs/d.
func (ch *Channel) Rate() float64 { return float64(ch.plan.SampleRate) / float64(ch.d) }

// BlockLen returns the number of samples per channelizer block (L/d).
func (ch *Channel) BlockLen() int { return ch.plan.L / ch.d }

// Residual is the part of the offset below one forward bin, which the
// chain removes after the channelizer (Hz, at the channel rate).
func (ch *Channel) Residual() float64 { return ch.residual }

func (ch *Channel) tune(offsetHz, lowHz, highHz float64) {
	p := ch.plan
	ch.k0 = int(math.Round(offsetHz / p.BinHz()))
	ch.residual = offsetHz - float64(ch.k0)*p.BinHz()
	ch.rot = cmplx.Exp(complex(0, -2*math.Pi*float64(ch.k0)*float64(p.L)/float64(p.N)))

	// Complex band-pass filter at the device rate, centred on the band in
	// the device spectrum (the bins are selected around k0 afterwards).
	taps := p.MaxTaps()
	if taps%2 == 0 {
		taps--
	}

	centre := offsetHz + (lowHz+highHz)/2
	cut := (highHz - lowHz) / 2 / float64(p.SampleRate)
	h := make([]complex128, p.N)
	mid := (taps - 1) / 2

	for n := range taps {
		x := float64(n - mid)
		sinc := 2 * cut
		if x != 0 {
			sinc = math.Sin(2*math.Pi*cut*x) / (math.Pi * x)
		}

		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(n)/float64(taps-1)) + 0.08*math.Cos(4*math.Pi*float64(n)/float64(taps-1))
		h[n] = complex(sinc*w, 0) * cmplx.Exp(complex(0, 2*math.Pi*centre*x/float64(p.SampleRate)))
	}

	full := fourier.NewCmplxFFT(p.N).Coefficients(nil, h)
	ch.h = make([]complex128, ch.m)
	ch.selectBins(ch.h, full)
	// Inverse FFT scaling of the decimated output (1/N).
	for i := range ch.h {
		ch.h[i] /= complex(float64(p.N), 0)
	}
}

// selectBins copies the m bins around k0 into dst, in FFT order.
func (ch *Channel) selectBins(dst, bins []complex128) {
	n, m := ch.plan.N, ch.m
	for j := range m {
		k := j
		if j >= m/2 {
			k = j - m
		}

		dst[j] = bins[((ch.k0+k)%n+n)%n]
	}
}

// Process turns one channelizer block into L/d channel samples appended to
// dst.
func (ch *Channel) Process(bins []complex128, dst []complex64) []complex64 {
	ch.selectBins(ch.sel, bins)

	for i := range ch.sel {
		ch.sel[i] *= ch.h[i]
	}

	ch.ifft.Sequence(ch.seq, ch.sel)

	for _, v := range ch.seq[ch.plan.V/ch.d:] {
		dst = append(dst, complex64(v*ch.phase))
	}

	ch.phase *= ch.rot
	// Keep the rotator on the unit circle.
	ch.phase /= complex(cmplx.Abs(ch.phase), 0)

	return dst
}
