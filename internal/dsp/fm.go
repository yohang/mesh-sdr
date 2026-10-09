package dsp

import (
	"math"
	"math/cmplx"
)

// FMDiscriminator is an FM discriminator without de-emphasis, limiter or
// AGC: the phase difference between consecutive IQ samples, scaled to ±1
// for ±π (Csdr::FmDemod). Data modes such as AIS (9600 Bd GMSK, MAR-001)
// need this flat output, which the NFM chain's audio is not. It is not
// safe for concurrent use.
type FMDiscriminator struct {
	last complex64
	out  []float32
}

// Process returns the discriminator output of iq, valid until the next
// call.
func (d *FMDiscriminator) Process(iq []complex64) []float32 {
	d.out = Grow(d.out, len(iq))

	for i, v := range iq {
		d.out[i] = float32(cmplx.Phase(complex128(v*complex(real(d.last), -imag(d.last)))) / math.Pi)
		d.last = v
	}

	return d.out
}

// Reset forgets the last sample (lost input).
func (d *FMDiscriminator) Reset() { d.last = 0 }
