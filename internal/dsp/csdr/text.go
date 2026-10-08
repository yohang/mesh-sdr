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

// ByteStage is a libcsdr++ module that writes bytes. Its decoders write
// without bound checks: Process needs an output at least as long as its
// input plus the carry.
type ByteStage[T Symbol] struct {
	h *C.msdr_stage
}

func newByteStage[T Symbol](h *C.msdr_stage, what string) (*ByteStage[T], error) {
	if h == nil {
		return nil, fmt.Errorf("%w: %s", ErrBuild, what)
	}

	return &ByteStage[T]{h: h}, nil
}

// Process appends in to the carry, runs the module and returns the number
// of bytes written to out. out must hold len(in) + Pending() bytes.
func (s *ByteStage[T]) Process(in []T, out []byte) (int, error) {
	if s.h == nil {
		return 0, ErrClosed
	}

	if len(out) < len(in)+s.Pending() {
		return 0, fmt.Errorf("%w: output of %d bytes for %d inputs", ErrProcess, len(out), len(in)+s.Pending())
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

func validTiming(decimation int, loopGain, maxError float32) error {
	// Written so that NaN fails every comparison.
	if decimation < 4 || !(loopGain > 0) || !(maxError > 0) {
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
