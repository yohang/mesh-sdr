package dsp

// deemphasis is the one-pole broadcast FM de-emphasis (DEM-016), a Go
// stage of the chain at the output rate. It is not libcsdr++'s
// WfmDeemphasis, whose coefficient is dt/tau + dt instead of
// dt/(tau + dt).
type deemphasis struct {
	rate  float64
	alpha float32
	last  float32
}

func newDeemphasis(rate, us int) *deemphasis {
	d := &deemphasis{rate: float64(rate)}
	d.set(us)

	return d
}

// set changes the time constant (µs; 0: 50 µs), keeping the filter state.
func (d *deemphasis) set(us int) {
	if us == 0 {
		us = WFMDeemphasis50
	}

	dt := 1 / d.rate
	tau := float64(us) * 1e-6
	d.alpha = float32(dt / (tau + dt))
}

// Process implements stage.
func (d *deemphasis) Process(in, out []float32) (int, error) {
	n := min(len(in), len(out))

	for i, v := range in[:n] {
		d.last += d.alpha * (v - d.last)
		out[i] = d.last
	}

	return n, nil
}

// Pending implements stage.
func (d *deemphasis) Pending() int { return 0 }

// Close implements stage.
func (d *deemphasis) Close() {}
