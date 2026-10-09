package dsp_test

import (
	"math"
	"math/cmplx"
	"testing"

	"github.com/yohang/mesh-sdr/internal/dsp"
)

// TestFMDiscriminator: a carrier at f Hz reads f/(rate/2), across blocks.
func TestFMDiscriminator(t *testing.T) {
	const rate = 48000.0

	for _, f := range []float64{2400, -2400, 0} {
		iq := make([]complex64, 960)
		for i := range iq {
			iq[i] = complex64(cmplx.Rect(0.3, 2*math.Pi*f*float64(i)/rate))
		}

		var d dsp.FMDiscriminator

		d.Process(iq[:1])

		for _, v := range d.Process(iq[1:]) {
			if math.Abs(float64(v)-f/(rate/2)) > 1e-4 {
				t.Fatalf("%g Hz: %g", f, v)
			}
		}
	}
}
