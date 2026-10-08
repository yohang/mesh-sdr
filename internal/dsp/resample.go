package dsp

import (
	"encoding/binary"
	"fmt"

	"github.com/yohang/mesh-sdr/internal/dsp/csdr"
)

// S16Converter turns demodulated float audio into the 16-bit signed
// little-endian samples a decoder reads on its stdin, at the decoder's
// input rate (§8.3 "Secondary decoder" audio tap, §8.4 input). It
// resamples when the rates differ.
type S16Converter struct {
	in, out int
	rs      *csdr.Stage[float32, float32]
	buf     []float32
	dst     []byte
}

// NewS16Converter converts audio at rate in to s16le at rate out.
func NewS16Converter(in, out int) (*S16Converter, error) {
	if in <= 0 || out <= 0 {
		return nil, fmt.Errorf("%w: decoder input %d → %d Hz", ErrChain, in, out)
	}

	c := &S16Converter{in: in, out: out}

	if in != out {
		rs, err := csdr.NewResampler(float64(in), out)
		if err != nil {
			return nil, err
		}

		c.rs = rs
	}

	return c, nil
}

// InRate returns the rate of the audio Convert takes.
func (c *S16Converter) InRate() int { return c.in }

// Convert returns audio as s16le at the output rate. The result is valid
// until the next call.
func (c *S16Converter) Convert(audio []float32) ([]byte, error) {
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
}
