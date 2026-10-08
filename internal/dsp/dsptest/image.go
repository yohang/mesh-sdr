// Package dsptest synthesizes test signals for the decoders: SSTV and
// HF FAX transmissions of a picture, as audio.
package dsptest

import "math"

// Pixel returns the colour of the pixel at x, y.
type Pixel func(x, y int) (r, g, b uint8)

// osc is a phase-continuous FM oscillator.
type osc struct {
	rate  float64
	phase float64
	out   []float32
}

// tone appends d seconds of f Hz; the fraction of a sample is carried in
// the phase only.
func (o *osc) tone(f, d float64) {
	for range int(math.Round(d * o.rate)) {
		o.phase += 2 * math.Pi * f / o.rate
		o.out = append(o.out, float32(0.5*math.Sin(o.phase)))
	}
}

// scan appends a scan of d seconds whose frequency follows level(i) for
// the n pixels i of the scan: 1500 Hz (black) to 2300 Hz (white).
func (o *osc) scan(d float64, n int, level func(i int) uint8) {
	samples := int(math.Round(d * o.rate))
	for s := range samples {
		f := 1500 + 800*float64(level(s*n/samples))/255
		o.phase += 2 * math.Pi * f / o.rate
		o.out = append(o.out, float32(0.5*math.Sin(o.phase)))
	}
}

// vis appends the SSTV calibration header and the VIS code (7 bits LSB
// first, even parity).
func (o *osc) vis(code int) {
	o.tone(1900, 0.300)
	o.tone(1200, 0.010)
	o.tone(1900, 0.300)
	o.tone(1200, 0.030)

	parity := 0

	for bit := range 7 {
		f := 1300.0
		if code>>bit&1 == 1 {
			f = 1100
			parity ^= 1
		}

		o.tone(f, 0.030)
	}

	if parity == 1 {
		o.tone(1100, 0.030)
	} else {
		o.tone(1300, 0.030)
	}

	o.tone(1200, 0.030)
}

// SSTV modes.
const (
	Robot36 = 8
	Martin1 = 44
)

// SSTV returns rate Hz audio of the first lines lines of a picture sent
// in mode (Robot36: 320×240, Martin1: 320×256), followed by 3 s of silence.
func SSTV(mode, rate, lines int, px Pixel) []float32 {
	o := &osc{rate: float64(rate)}
	o.vis(mode)

	for y := range lines {
		switch mode {
		case Robot36:
			robot36Line(o, y, px)
		case Martin1:
			martin1Line(o, y, px)
		}
	}

	o.out = append(o.out, make([]float32, 3*rate)...)

	return o.out
}

// yuv is the BT.601 YCbCr of r, g, b on the SSTV scale.
func yuv(r, g, b uint8) (y, u, v uint8) {
	fr, fg, fb := float64(r), float64(g), float64(b)
	yy := 0.299*fr + 0.587*fg + 0.114*fb
	uu := 128 + (fb-yy)*0.564
	vv := 128 + (fr-yy)*0.713

	clamp := func(f float64) uint8 { return uint8(math.Max(0, math.Min(255, math.Round(f)))) }

	return clamp(yy), clamp(uu), clamp(vv)
}

// robot36Line: sync, porch, Y, then R-Y on even lines and B-Y on odd ones.
func robot36Line(o *osc, line int, px Pixel) {
	const width = 320

	o.tone(1200, 0.009)
	o.tone(1500, 0.003)
	o.scan(0.088, width, func(x int) uint8 { y, _, _ := yuv(px(x, line)); return y })

	if line%2 == 0 {
		o.tone(1500, 0.0045)
		o.tone(1900, 0.0015)
		o.scan(0.044, width, func(x int) uint8 { _, _, v := yuv(px(x, line)); return v })
	} else {
		o.tone(2300, 0.0045)
		o.tone(1900, 0.0015)
		o.scan(0.044, width, func(x int) uint8 { _, u, _ := yuv(px(x, line)); return u })
	}
}

// martin1Line: sync, porch, then green, blue and red scans, each followed
// by a separator.
func martin1Line(o *osc, line int, px Pixel) {
	const width = 320

	o.tone(1200, 0.004862)
	o.tone(1500, 0.000572)

	for ch := range 3 {
		o.scan(0.146432, width, func(x int) uint8 {
			r, g, b := px(x, line)
			return [3]uint8{g, b, r}[ch]
		})
		o.tone(1500, 0.000572)
	}
}

// FAX returns rate Hz audio of an HF FAX page at lpm lines per minute
// (IOC 576): 5 s of start tone, 40 lines of phasing, the lines of the
// picture (grey levels), 5 s of stop tone and 3 s of silence.
func FAX(rate, lpm, lines int, grey func(x, y int) uint8) []float32 {
	o := &osc{rate: float64(rate)}
	line := 60 / float64(lpm)

	// Start and stop tones: black and white alternating at 300 and 450 Hz.
	square := func(f, d float64) {
		samples := int(math.Round(d * o.rate))
		o.scan(d, samples, func(i int) uint8 {
			if math.Sin(2*math.Pi*f*float64(i)/o.rate) >= 0 {
				return 255
			}

			return 0
		})
	}

	square(300, 5)

	// Phasing: 40 black lines (the decoder counts them) with a white pulse
	// at their start.
	for range 40 {
		o.scan(line*0.05, 1, func(int) uint8 { return 255 })
		o.scan(line*0.95, 1, func(int) uint8 { return 0 })
	}

	const width = 1809

	for y := range lines {
		o.scan(line, width, func(x int) uint8 { return grey(x, y) })
	}

	square(450, 5)
	o.out = append(o.out, make([]float32, 3*rate)...)

	return o.out
}
