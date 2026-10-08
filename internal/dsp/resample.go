package dsp

import (
	"encoding/binary"
	"fmt"

	"github.com/yohang/mesh-sdr/internal/dsp/csdr"
)

// S16Converter turns demodulated float audio into the 16-bit signed
// little-endian samples a decoder reads on its stdin, at the decoder's
// input rate (§8.3 "Secondary decoder" audio tap, §8.4 input). It follows
// the rate of its input: it resamples when the rates differ and only
// converts when they match.
type S16Converter struct {
	in, out int
	rs      *csdr.Stage[float32, float32]
	buf     []float32
	dst     []byte
}

// NewS16Converter converts audio to s16le at rate out.
func NewS16Converter(out int) *S16Converter { return &S16Converter{out: out} }

// Convert returns audio at rate as s16le at the output rate. The result is
// valid until the next call. A new input rate starts a new resampler.
func (c *S16Converter) Convert(audio []float32, rate int) ([]byte, error) {
	if rate <= 0 || c.out <= 0 {
		return nil, fmt.Errorf("%w: decoder input %d → %d Hz", ErrChain, rate, c.out)
	}

	if rate != c.in {
		c.Close()

		if rate != c.out {
			rs, err := csdr.NewResampler(float64(rate), c.out)
			if err != nil {
				return nil, err
			}

			c.rs = rs
		}

		c.in = rate
	}

	if c.rs != nil {
		c.buf = Grow(c.buf, int(float64(len(audio)+c.rs.Pending())*float64(c.out)/float64(c.in))+stepMargin)

		n, err := c.rs.Process(audio, c.buf)
		if err != nil {
			return nil, err
		}

		audio = c.buf[:n]
	}

	c.dst = c.dst[:0]
	for _, v := range audio {
		c.dst = binary.LittleEndian.AppendUint16(c.dst, uint16(ToS16(v)))
	}

	return c.dst, nil
}

// Close releases the resampler.
func (c *S16Converter) Close() {
	if c.rs != nil {
		c.rs.Close()
		c.rs = nil
	}

	c.in = 0
}

// IQResampler resamples complex IQ (the selector or a channel of a
// demodulator at its channel rate) to an integer rate: libsamplerate on I
// and Q. It follows the rate of its input; at the output rate the IQ
// passes through.
type IQResampler struct {
	in   float64
	out  int
	i, q *csdr.Stage[float32, float32]

	ri, rq, oi, oq []float32
	iq             []complex64
}

// NewIQResampler resamples to out Hz. Close releases it.
func NewIQResampler(out int) *IQResampler { return &IQResampler{out: out} }

// SetRate sets the input rate: a new rate starts new resamplers.
func (r *IQResampler) SetRate(in float64) error {
	if in == r.in {
		return nil
	}

	r.Close()

	if !(in > 0) || r.out <= 0 {
		return fmt.Errorf("%w: IQ %v → %d Hz", ErrChain, in, r.out)
	}

	if in != float64(r.out) {
		i, err := csdr.NewResampler(in, r.out)
		if err != nil {
			return err
		}

		q, err := csdr.NewResampler(in, r.out)
		if err != nil {
			i.Close()

			return err
		}

		r.i, r.q = i, q
	}

	r.in = in

	return nil
}

// Process returns iq, at rate in, at the output rate, valid until the next
// call.
func (r *IQResampler) Process(iq []complex64, in float64) ([]complex64, error) {
	if err := r.SetRate(in); err != nil {
		return nil, err
	}

	if r.i == nil {
		return iq, nil
	}

	r.ri, r.rq = Grow(r.ri, len(iq)), Grow(r.rq, len(iq))
	for k, v := range iq {
		r.ri[k], r.rq[k] = real(v), imag(v)
	}

	n := int(float64(len(iq)+r.i.Pending())*float64(r.out)/r.in) + stepMargin
	r.oi, r.oq = Grow(r.oi, n), Grow(r.oq, n)

	ni, err := r.i.Process(r.ri, r.oi)
	if err != nil {
		return nil, err
	}

	nq, err := r.q.Process(r.rq, r.oq)
	if err != nil {
		return nil, err
	}

	// Both resamplers see the same lengths: they release the same count.
	m := min(ni, nq)
	r.iq = Grow(r.iq, m)

	for k := range m {
		r.iq[k] = complex(r.oi[k], r.oq[k])
	}

	return r.iq, nil
}

// Close releases the resamplers; the next block starts new ones.
func (r *IQResampler) Close() {
	if r.i != nil {
		r.i.Close()
		r.q.Close()
		r.i, r.q = nil, nil
	}

	r.in = 0
}
