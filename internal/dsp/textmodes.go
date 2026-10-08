package dsp

import (
	"fmt"
	"math"

	"github.com/yohang/mesh-sdr/internal/dsp/csdr"
)

// The native text decoders (DEC-006 to DEC-012) on libcsdr++ kernels, with
// the chains and parameters of OpenWebRX+ (csdr/chain/digimodes.py,
// owrx/dsp.py): they read the selector IQ of the listener's demodulator at
// TextRate, move the signal at the secondary offset to 0 Hz and band-pass
// it (the secondary selector, DEC-005), then decode it.

// TextRate is the input rate of the text decoders and of the secondary
// FFT: the selector rate of the OpenWebRX+ digital chains.
const TextRate = 12000

// TextKind is a family of text decoders.
type TextKind string

// Text decoder kinds.
const (
	// TextPSK: AGC, timing recovery, DBPSK, Varicode.
	TextPSK TextKind = "psk"
	// TextRTTY: AGC, FM demodulator, low-pass, timing recovery, RTTY
	// framing, Baudot.
	TextRTTY TextKind = "rtty"
	// TextSITORB: the RTTY chain with SITOR-B (FEC) and CCIR 476.
	TextSITORB TextKind = "sitorb"
	// TextCW: AGC, CW decoder.
	TextCW TextKind = "cw"
)

// TextConfig configures a text decoder.
type TextConfig struct {
	Kind TextKind
	// Baud is the symbol rate (PSK, RTTY, SITOR-B).
	Baud float64
	// BandwidthHz is the half width of the secondary band-pass: the
	// selector keeps ±BandwidthHz around the offset.
	BandwidthHz float64
	// Invert swaps mark and space (RTTY, SITOR-B).
	Invert bool
	// ShowCW also prints the dots and dashes (cw_showcw).
	ShowCW bool
	// OffsetHz is the frequency of the signal in the selector IQ.
	OffsetHz float64
}

// sitorbErrors are the invalid codes in a row before SITOR-B resyncs (the
// libcsdr++ default).
const sitorbErrors = 4

// lowpassTransition is the transition of the RTTY and SITOR-B low-pass
// (the pycsdr default).
const lowpassTransition = 0.05

// Validate checks the configuration.
func (c TextConfig) Validate() error {
	half := float64(TextRate) / 2

	switch {
	case !finite(c.Baud) || !finite(c.BandwidthHz) || !finite(c.OffsetHz):
		return fmt.Errorf("%w: non-finite text decoder parameters", ErrChain)
	case c.BandwidthHz <= 0 || c.BandwidthHz >= half/2:
		return fmt.Errorf("%w: text decoder bandwidth %g Hz", ErrChain, c.BandwidthHz)
	case math.Abs(c.OffsetHz) > half:
		return fmt.Errorf("%w: text decoder offset %g Hz", ErrChain, c.OffsetHz)
	case c.Kind == TextCW:
		return nil
	case c.Kind != TextPSK && c.Kind != TextRTTY && c.Kind != TextSITORB:
		return fmt.Errorf("%w: text decoder %q", ErrChain, c.Kind)
	case c.Baud < 10 || c.Baud > 300:
		return fmt.Errorf("%w: text decoder baud rate %g", ErrChain, c.Baud)
	}

	return nil
}

// TextDecoder is one native text decoder: secondary selector (shift and
// band-pass) and decoder chain. It is not safe for concurrent use.
type TextDecoder struct {
	cfg TextConfig

	shift *csdr.Shift
	bp    *csdr.Stage[complex64, complex64]
	agc   *csdr.Stage[complex64, complex64]

	// PSK
	symbols *csdr.Stage[complex64, complex64]
	bits    *csdr.ByteStage[complex64]

	// RTTY, SITOR-B
	fm      *csdr.Stage[complex64, float32]
	lp      *csdr.Stage[float32, float32]
	timing  *csdr.Stage[float32, float32]
	framing *csdr.ByteStage[float32]

	// chars turns PSK bits, Baudot or CCIR 476 codes into characters.
	chars *csdr.ByteStage[byte]

	cw *csdr.CW

	c1, c2  []complex64
	f1, f2  []float32
	b1, out []byte
}

// NewTextDecoder builds a decoder. Close releases it.
func NewTextDecoder(cfg TextConfig) (*TextDecoder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	d := &TextDecoder{cfg: cfg}
	if err := d.build(); err != nil {
		d.Close()

		return nil, err
	}

	return d, nil
}

func (d *TextDecoder) build() error {
	cfg := d.cfg
	rate := float64(TextRate)

	var err error
	if d.shift, err = csdr.NewShift(d.shiftRate()); err != nil {
		return err
	}

	cut := float32(cfg.BandwidthHz / rate)
	if d.bp, err = csdr.NewBandPass(-cut, cut, cut); err != nil {
		return err
	}

	if d.agc, err = csdr.NewComplexAGC(); err != nil {
		return err
	}

	switch cfg.Kind {
	case TextPSK:
		// Samples per symbol, a multiple of 4 (digimodes.py).
		spb := int(math.Round(rate/cfg.Baud)) &^ 3
		if d.symbols, err = csdr.NewComplexTimingRecovery(spb, 0.5, 2); err != nil {
			return err
		}

		if d.bits, err = csdr.NewDBPSK(); err != nil {
			return err
		}

		d.chars, err = csdr.NewVaricode()

		return err
	case TextRTTY, TextSITORB:
		if d.fm, err = csdr.NewFMDemod(); err != nil {
			return err
		}

		if d.lp, err = csdr.NewLowPass(float32(cfg.Baud/rate), lowpassTransition); err != nil {
			return err
		}

		spb := int(math.Round(rate / cfg.Baud))
		gain := float32(rate / cfg.BandwidthHz / 5)

		if d.timing, err = csdr.NewTimingRecovery(spb, gain, 10); err != nil {
			return err
		}

		if cfg.Kind == TextRTTY {
			if d.framing, err = csdr.NewRTTY(cfg.Invert); err != nil {
				return err
			}

			d.chars, err = csdr.NewBaudot()

			return err
		}

		if d.framing, err = csdr.NewSitorB(sitorbErrors, cfg.Invert); err != nil {
			return err
		}

		d.chars, err = csdr.NewCCIR476()

		return err
	default:
		d.cw, err = csdr.NewCW(TextRate, cfg.ShowCW)

		return err
	}
}

func (d *TextDecoder) shiftRate() float32 { return float32(-d.cfg.OffsetHz / TextRate) }

// Config returns the configuration.
func (d *TextDecoder) Config() TextConfig { return d.cfg }

// SetOffset moves the secondary selector to hz (DEC-005) and resets the CW
// timing (a dial change, DEC-012).
func (d *TextDecoder) SetOffset(hz float64) error {
	if !finite(hz) || math.Abs(hz) > TextRate/2 {
		return fmt.Errorf("%w: text decoder offset %g Hz", ErrChain, hz)
	}

	d.cfg.OffsetHz = hz
	if err := d.shift.SetRate(d.shiftRate()); err != nil {
		return err
	}

	return d.Reset()
}

// Reset forgets the state that depends on the dial frequency: the CW
// timing (DEC-012). Other decoders resynchronise by themselves.
func (d *TextDecoder) Reset() error {
	if d.cw != nil {
		return d.cw.Reset()
	}

	return nil
}

// complexStep runs a complex stage into buf.
func complexStep(s *csdr.Stage[complex64, complex64], in []complex64, buf *[]complex64) ([]complex64, error) {
	*buf = grow(*buf, len(in)+s.Pending()+stepMargin)

	n, err := s.Process(in, *buf)
	if err != nil {
		return nil, err
	}

	return (*buf)[:n], nil
}

// floatStep runs a float stage into buf.
func floatStep(s *csdr.Stage[float32, float32], in []float32, buf *[]float32) ([]float32, error) {
	*buf = grow(*buf, len(in)+s.Pending()+stepMargin)

	n, err := s.Process(in, *buf)
	if err != nil {
		return nil, err
	}

	return (*buf)[:n], nil
}

// byteStep runs a byte stage into buf.
func byteStep[T csdr.Symbol](s *csdr.ByteStage[T], in []T, buf *[]byte) ([]byte, error) {
	*buf = grow(*buf, len(in)+s.Pending()+stepMargin)

	n, err := s.Process(in, *buf)
	if err != nil {
		return nil, err
	}

	return (*buf)[:n], nil
}

// Process runs a block of selector IQ at TextRate through the decoder and
// returns the characters decoded, raw (untrusted RF text, not sanitised),
// valid until the next call.
func (d *TextDecoder) Process(iq []complex64) ([]byte, error) {
	sel, err := complexStep(d.shift.Stage, iq, &d.c1)
	if err != nil {
		return nil, err
	}

	if sel, err = complexStep(d.bp, sel, &d.c2); err != nil {
		return nil, err
	}

	if sel, err = complexStep(d.agc, sel, &d.c1); err != nil {
		return nil, err
	}

	switch {
	case d.cw != nil:
		return byteStep(d.cw.ByteStage, sel, &d.out)
	case d.symbols != nil:
		sym, err := complexStep(d.symbols, sel, &d.c2)
		if err != nil {
			return nil, err
		}

		bits, err := byteStep(d.bits, sym, &d.b1)
		if err != nil {
			return nil, err
		}

		return byteStep(d.chars, bits, &d.out)
	default:
		d.f1 = grow(d.f1, len(sel)+d.fm.Pending()+stepMargin)

		n, err := d.fm.Process(sel, d.f1)
		if err != nil {
			return nil, err
		}

		f, err := floatStep(d.lp, d.f1[:n], &d.f2)
		if err != nil {
			return nil, err
		}

		if f, err = floatStep(d.timing, f, &d.f1); err != nil {
			return nil, err
		}

		codes, err := byteStep(d.framing, f, &d.b1)
		if err != nil {
			return nil, err
		}

		return byteStep(d.chars, codes, &d.out)
	}
}

// Close releases the stages.
func (d *TextDecoder) Close() {
	if d.shift != nil {
		d.shift.Close()
	}

	for _, s := range []*csdr.Stage[complex64, complex64]{d.bp, d.agc, d.symbols} {
		s.Close()
	}

	for _, s := range []*csdr.Stage[float32, float32]{d.lp, d.timing} {
		s.Close()
	}

	if d.fm != nil {
		d.fm.Close()
	}

	d.bits.Close()
	d.framing.Close()
	d.chars.Close()

	if d.cw != nil {
		d.cw.Close()
	}
}

// IQResampler resamples complex IQ (the selector of a demodulator at its
// channel rate) to an integer rate: libsamplerate on I and Q.
type IQResampler struct {
	in   float64
	out  int
	i, q *csdr.Stage[float32, float32]

	ri, rq, oi, oq []float32
	iq             []complex64
}

// NewIQResampler resamples from in to out Hz. Close releases it.
func NewIQResampler(in float64, out int) (*IQResampler, error) {
	r := &IQResampler{in: in, out: out}

	var err error
	if r.i, err = csdr.NewResampler(in, out); err != nil {
		return nil, err
	}

	if r.q, err = csdr.NewResampler(in, out); err != nil {
		r.i.Close()

		return nil, err
	}

	return r, nil
}

// InRate returns the input rate.
func (r *IQResampler) InRate() float64 { return r.in }

// Process returns iq at the output rate, valid until the next call.
func (r *IQResampler) Process(iq []complex64) ([]complex64, error) {
	r.ri, r.rq = grow(r.ri, len(iq)), grow(r.rq, len(iq))
	for k, v := range iq {
		r.ri[k], r.rq[k] = real(v), imag(v)
	}

	n := int(float64(len(iq)+r.i.Pending())*float64(r.out)/r.in) + stepMargin
	r.oi, r.oq = grow(r.oi, n), grow(r.oq, n)

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
	r.iq = grow(r.iq, m)

	for k := range m {
		r.iq[k] = complex(r.oi[k], r.oq[k])
	}

	return r.iq, nil
}

// Close releases the resamplers.
func (r *IQResampler) Close() {
	r.i.Close()
	r.q.Close()
}

// SecondarySpectrum returns the configuration of the secondary FFT of a
// text decoder (DEC-004): size bins over TextRate, at the frame rate and
// overlap of the shared spectrum (fft_fps, fft_voverlap_factor).
func SecondarySpectrum(size int) SpectrumConfig {
	c := DefaultSpectrum(TextRate)
	c.Size = size

	return c
}
