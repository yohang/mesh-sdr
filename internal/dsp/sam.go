package dsp

import "math"

// PLL parameters of synchronous AM (DEM-003): a second-order loop with a
// 50 Hz natural frequency, critically damped, pulling in carriers up to
// ±1 kHz from the channel centre.
const (
	pllBandwidth = 50.0
	pllDamping   = 0.707
	pllMaxOffset = 1000.0
)

// pll locks onto the AM carrier and outputs the in-phase part of the
// derotated signal (libcsdr++ has no synchronous AM block). It is a Go
// stage of the chain: per-sample work stays in Go, no cgo call per sample.
type pll struct {
	phase, freq float64
	alpha, beta float64
	maxFreq     float64
}

// newPLL returns the carrier PLL of a channel at rate.
func newPLL(rate float64) *pll {
	wn := 2 * math.Pi * pllBandwidth / rate

	return &pll{alpha: 2 * pllDamping * wn, beta: wn * wn, maxFreq: 2 * math.Pi * pllMaxOffset / rate}
}

// Process implements stage.
func (p *pll) Process(in []complex64, out []float32) (int, error) {
	n := min(len(in), len(out))

	for i, v := range in[:n] {
		s, c := math.Sincos(p.phase)
		re := float64(real(v))*c + float64(imag(v))*s
		im := float64(imag(v))*c - float64(real(v))*s
		out[i] = float32(re)

		// Phase error normalised by the magnitude: the loop gain does not
		// follow the signal level.
		var e float64
		if m := math.Hypot(re, im); m > 1e-12 {
			e = im / m
		}

		p.freq = min(max(p.freq+p.beta*e, -p.maxFreq), p.maxFreq)

		p.phase += p.freq + p.alpha*e
		if p.phase > math.Pi {
			p.phase -= 2 * math.Pi
		} else if p.phase < -math.Pi {
			p.phase += 2 * math.Pi
		}
	}

	return n, nil
}

// Pending implements stage.
func (p *pll) Pending() int { return 0 }

// Close implements stage.
func (p *pll) Close() {}
