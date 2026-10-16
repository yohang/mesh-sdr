package csdr

/*
#include "shim_text.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// The libcsdr++ modules of the native text decoders (DEC-006 to DEC-012),
// chained as OpenWebRX+ does (csdr/chain/digimodes.py). The decoders write
// bytes: bits, Baudot or CCIR 476 codes, or characters.

// Symbol is an element type a byte stage reads.
type Symbol interface{ complex64 | float32 | byte }

// ByteStage is a libcsdr++ module that writes bytes. Most of its decoders
// write without bound checks: Process needs an output of OutputLen bytes.
type ByteStage[T Symbol] struct {
	h *C.msdr_stage
	// per and extra size the output: per bytes per input item plus extra
	// (1 and 0 for the decoders that write at most one byte per item).
	per, extra int
}

func newByteStage[T Symbol](h *C.msdr_stage, what string) (*ByteStage[T], error) {
	if h == nil {
		return nil, fmt.Errorf("%w: %s", ErrBuild, what)
	}

	return &ByteStage[T]{h: h, per: 1}, nil
}

// OutputLen returns the output length Process needs for n input items:
// they and the carry, times the bytes the module may write per item.
func (s *ByteStage[T]) OutputLen(n int) int { return (n+s.Pending())*s.per + s.extra }

// Process appends in to the carry, runs the module and returns the number
// of bytes written to out. out must hold OutputLen(len(in)) bytes.
func (s *ByteStage[T]) Process(in []T, out []byte) (int, error) {
	if s.h == nil {
		return 0, ErrClosed
	}

	if need := s.OutputLen(len(in)); len(out) < need {
		return 0, fmt.Errorf("%w: output of %d bytes, %d needed", ErrProcess, len(out), need)
	}

	var inPtr, outPtr unsafe.Pointer
	if len(in) > 0 {
		inPtr = unsafe.Pointer(unsafe.SliceData(in))
	}

	if len(out) > 0 {
		outPtr = unsafe.Pointer(unsafe.SliceData(out))
	}

	n := C.msdr_stage_process(s.h, inPtr, C.size_t(len(in)), outPtr, C.size_t(len(out)))
	if n < 0 {
		return 0, ErrProcess
	}

	return int(n), nil
}

// Pending returns the number of input items waiting in the carry.
func (s *ByteStage[T]) Pending() int {
	if s.h == nil {
		return 0
	}

	return int(C.msdr_stage_pending(s.h))
}

// Close releases the module. It is safe to call twice.
func (s *ByteStage[T]) Close() {
	if s != nil && s.h != nil {
		C.msdr_stage_free(s.h)
		s.h = nil
	}
}

// NewComplexAGC returns Csdr::Agc<complex<float>> with the libcsdr++
// defaults (the AGC of the OpenWebRX+ digital chains).
func NewComplexAGC() (*Stage[complex64, complex64], error) {
	return newStage[complex64, complex64](C.msdr_agc_complex_new(), "complex agc")
}

// validTiming checks the timing recovery parameters: a correction is at
// most loopGain × maxError half symbols, which must stay within half a
// symbol (the module reads three half symbols ahead and skips one symbol
// plus the correction).
func validTiming(decimation int, loopGain, maxError float32) error {
	// Written so that NaN fails every comparison.
	if decimation < 4 || !(loopGain > 0) || !(maxError > 0) || !(loopGain*maxError <= 1) {
		return fmt.Errorf("%w: timing recovery decimation %d gain %g error %g", ErrBuild, decimation, loopGain, maxError)
	}

	return nil
}

// NewComplexTimingRecovery returns Csdr::GardnerTimingRecovery on complex
// samples: one sample per symbol of decimation input samples.
func NewComplexTimingRecovery(decimation int, loopGain, maxError float32) (*Stage[complex64, complex64], error) {
	if err := validTiming(decimation, loopGain, maxError); err != nil {
		return nil, err
	}

	return newStage[complex64, complex64](C.msdr_timing_recovery_complex_new(C.uint(decimation), C.float(loopGain), C.float(maxError)), "timing recovery")
}

// NewTimingRecovery returns Csdr::GardnerTimingRecovery on float samples.
func NewTimingRecovery(decimation int, loopGain, maxError float32) (*Stage[float32, float32], error) {
	if err := validTiming(decimation, loopGain, maxError); err != nil {
		return nil, err
	}

	return newStage[float32, float32](C.msdr_timing_recovery_float_new(C.uint(decimation), C.float(loopGain), C.float(maxError)), "timing recovery")
}

// NewLowPass returns Csdr::LowPassFilter<float> (FIR, Hamming window):
// cutoff and transition are relative to the sample rate.
func NewLowPass(cutoff, transition float32) (*Stage[float32, float32], error) {
	if !(cutoff > 0 && cutoff < 0.5 && transition > 0 && transition <= 0.5) {
		return nil, fmt.Errorf("%w: low-pass cutoff %g transition %g", ErrBuild, cutoff, transition)
	}

	return newStage[float32, float32](C.msdr_lowpass_float_new(C.float(cutoff), C.float(transition)), "low-pass")
}

// NewDBPSK returns Csdr::DBPskDecoder: one bit per complex symbol.
func NewDBPSK() (*ByteStage[complex64], error) {
	return newByteStage[complex64](C.msdr_dbpsk_new(), "dbpsk")
}

// NewVaricode returns Csdr::VaricodeDecoder: PSK31 bits to characters.
func NewVaricode() (*ByteStage[byte], error) {
	return newByteStage[byte](C.msdr_varicode_new(), "varicode")
}

func cbool(b bool) C.int {
	if b {
		return 1
	}

	return 0
}

// NewRTTY returns Csdr::RttyDecoder: one 5-bit code per start bit found.
func NewRTTY(invert bool) (*ByteStage[float32], error) {
	return newByteStage[float32](C.msdr_rtty_new(cbool(invert)), "rtty")
}

// NewBaudot returns Csdr::BaudotDecoder: ITA2 codes to characters.
func NewBaudot() (*ByteStage[byte], error) {
	return newByteStage[byte](C.msdr_baudot_new(), "baudot")
}

// NewSitorB returns Csdr::SitorBDecoder: 7-bit CCIR 476 codes with the
// FEC repetition applied; errorsAllowed invalid codes in a row resync it.
func NewSitorB(errorsAllowed int, invert bool) (*ByteStage[float32], error) {
	if errorsAllowed < 0 {
		return nil, fmt.Errorf("%w: sitor-b errors %d", ErrBuild, errorsAllowed)
	}

	return newByteStage[float32](C.msdr_sitorb_new(C.uint(errorsAllowed), cbool(invert)), "sitor-b")
}

// NewCCIR476 returns Csdr::Ccir476Decoder: CCIR 476 codes to characters.
func NewCCIR476() (*ByteStage[byte], error) {
	return newByteStage[byte](C.msdr_ccir476_new(), "ccir476")
}

// NewNAVTEX returns Csdr::NavtexDecoder: of the SITOR-B characters, it
// keeps the NAVTEX messages only (from a "ZCZC B1B2B3B4" header line to
// the NNNN end), unchanged.
func NewNAVTEX() (*ByteStage[byte], error) {
	return newByteStage[byte](C.msdr_navtex_new(), "navtex")
}

// NewCCIR493 returns Csdr::Ccir493Decoder: 10-bit CCIR 493 (DSC) symbols
// from float bits, the DX/RX time diversity applied; errorsAllowed
// invalid symbols in a row resync it.
func NewCCIR493(errorsAllowed int, invert bool) (*ByteStage[float32], error) {
	if errorsAllowed < 0 {
		return nil, fmt.Errorf("%w: ccir493 errors %d", ErrBuild, errorsAllowed)
	}

	return newByteStage[float32](C.msdr_ccir493_new(C.uint(errorsAllowed), cbool(invert)), "ccir493")
}

// DSC output bounds: Csdr::DscDecoder writes a JSON line per call (at most
// about 400 bytes for at least 20 symbols read) or per undecoded run of at
// least 4 symbols (at most about 230 bytes), and works only with dscRoom
// bytes free.
const (
	dscPerSymbol = 64
	dscRoom      = 256
)

// NewDSC returns Csdr::DscDecoder: CCIR 493 symbols to one JSON line per
// DSC call ({"format": …, "src": …, "ecc": true…}), or per run of symbols
// that is not a call ({"format": "error", "data": …}).
func NewDSC() (*ByteStage[byte], error) {
	s, err := newByteStage[byte](C.msdr_dsc_new(), "dsc")
	if err != nil {
		return nil, err
	}

	s.per, s.extra = dscPerSymbol, dscRoom

	return s, nil
}

// CW is Csdr::CwDecoder<complex<float>>.
type CW struct {
	*ByteStage[complex64]
}

// NewCW returns a CW decoder at sampleRate; showCW also prints the dots
// and dashes.
func NewCW(sampleRate int, showCW bool) (*CW, error) {
	if sampleRate < 1000 {
		return nil, fmt.Errorf("%w: cw rate %d", ErrBuild, sampleRate)
	}

	s, err := newByteStage[complex64](C.msdr_cw_new(C.uint(sampleRate), cbool(showCW)), "cw")
	if err != nil {
		return nil, err
	}

	return &CW{ByteStage: s}, nil
}

// Reset forgets the timing the decoder learnt (dial change, DEC-012).
func (c *CW) Reset() error {
	if c.h == nil {
		return ErrClosed
	}

	if C.msdr_cw_reset(c.h) != 0 {
		return ErrProcess
	}

	return nil
}
