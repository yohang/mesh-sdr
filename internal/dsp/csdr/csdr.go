// Package csdr binds the libcsdr++ modules used by the node DSP through a
// small C ABI shim (shim.h, shim.cpp), as decided by ADR 0014.
//
// Only the leaf modules are used, driven synchronously by the calling
// goroutine one block at a time: the libcsdr framework (one thread per
// module, mmap ring buffers) is not. A Stage is not safe for concurrent
// use; each DSP chain owns its stages. Buffers are passed as pointer and
// length for the duration of one call (cgo pointer rules); no Go pointer is
// retained by C++.
//
// Fixed-length modules only process when strictly more than one block is
// available (csdr assumes a continuous ring), which would hold every block
// back by one; such trivial modules (FftExchangeSides) are done in Go by
// the callers instead.
//
// This is the only package of the module that uses cgo.
package csdr

/*
#cgo pkg-config: csdr
#cgo CXXFLAGS: -std=c++17 -O2
#cgo LDFLAGS: -lstdc++
#include "shim.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// Sample is an element type a stage reads or writes: complex64 is the
// layout of Csdr::complex<float>.
type Sample interface{ complex64 | float32 }

// Errors.
var (
	// ErrBuild reports a module that could not be constructed.
	ErrBuild = errors.New("csdr: cannot build module")
	// ErrProcess reports a failure inside a module.
	ErrProcess = errors.New("csdr: module failed")
	// ErrClosed reports the use of a closed stage.
	ErrClosed = errors.New("csdr: stage closed")
)

// Stage is one libcsdr++ module with its input carry. It reads T and
// writes U.
type Stage[T, U Sample] struct {
	h *C.msdr_stage
}

func newStage[T, U Sample](h *C.msdr_stage, what string) (*Stage[T, U], error) {
	if h == nil {
		return nil, fmt.Errorf("%w: %s", ErrBuild, what)
	}

	return &Stage[T, U]{h: h}, nil
}

// Process appends in to the carry, runs the module and writes at most
// len(out) items to out. It returns the number of items written. Input the
// module cannot consume yet (it needs a full block, or out is full) stays
// in the carry for the next call.
func (s *Stage[T, U]) Process(in []T, out []U) (int, error) {
	if s.h == nil {
		return 0, ErrClosed
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
func (s *Stage[T, U]) Pending() int {
	if s.h == nil {
		return 0
	}

	return int(C.msdr_stage_pending(s.h))
}

// Close releases the module. It is safe to call twice.
func (s *Stage[T, U]) Close() {
	if s != nil && s.h != nil {
		C.msdr_stage_free(s.h)
		s.h = nil
	}
}

// Window is the window function of an FFT.
type Window int

// Windows.
const (
	Blackman Window = C.MSDR_WINDOW_BLACKMAN
	Hamming  Window = C.MSDR_WINDOW_HAMMING
)

// NewFFT returns Csdr::Fft: one size-point FFT of the windowed input every
// every input samples (complex in, complex out, unshifted bins).
func NewFFT(size, every int, w Window) (*Stage[complex64, complex64], error) {
	if size < 2 || every < 1 {
		return nil, fmt.Errorf("%w: fft size %d every %d", ErrBuild, size, every)
	}

	return newStage[complex64, complex64](C.msdr_fft_new(C.uint(size), C.uint(every), C.int(w)), "fft")
}

// NewLogAveragePower returns Csdr::LogAveragePower: the power of avg
// consecutive FFTs of size bins, averaged, in dB plus addDB.
func NewLogAveragePower(size, avg int, addDB float32) (*Stage[complex64, float32], error) {
	if size < 2 || avg < 1 {
		return nil, fmt.Errorf("%w: log average power size %d avg %d", ErrBuild, size, avg)
	}

	return newStage[complex64, float32](C.msdr_logavgpower_new(C.uint(size), C.uint(avg), C.float(addDB)), "log average power")
}

// Shift is Csdr::ShiftMath: a frequency shift by rate cycles per sample.
type Shift struct {
	*Stage[complex64, complex64]
}

// NewShift returns a shift by rate cycles per sample (−0.5…0.5).
func NewShift(rate float32) (*Shift, error) {
	s, err := newStage[complex64, complex64](C.msdr_shift_new(C.float(rate)), "shift")
	if err != nil {
		return nil, err
	}

	return &Shift{Stage: s}, nil
}

// SetRate changes the shift without resetting the phase.
func (s *Shift) SetRate(rate float32) error {
	if s.h == nil {
		return ErrClosed
	}

	if C.msdr_shift_set_rate(s.h, C.float(rate)) != 0 {
		return ErrProcess
	}

	return nil
}

// NewFMDemod returns Csdr::FmDemod: phase difference per sample, scaled to
// ±1 for ±π.
func NewFMDemod() (*Stage[complex64, float32], error) {
	return newStage[complex64, float32](C.msdr_fmdemod_new(), "fm demod")
}

// NewLimit returns Csdr::Limit: clips samples to ±maxAmplitude.
func NewLimit(maxAmplitude float32) (*Stage[float32, float32], error) {
	return newStage[float32, float32](C.msdr_limit_new(C.float(maxAmplitude)), "limit")
}

// NewNFMDeemphasis returns Csdr::NfmDeephasis for sampleRate (csdr picks
// the nearest of 8000, 11025, 12000, 24000, 44100 and 48000 Hz).
func NewNFMDeemphasis(sampleRate int) (*Stage[float32, float32], error) {
	if sampleRate < 8000 {
		return nil, fmt.Errorf("%w: deemphasis rate %d", ErrBuild, sampleRate)
	}

	return newStage[float32, float32](C.msdr_nfm_deemphasis_new(C.uint(sampleRate)), "nfm deemphasis")
}

// AGC are the parameters of Csdr::Agc.
type AGC struct {
	Reference float32
	Attack    float32
	Decay     float32
	MaxGain   float32
	// Hang is the hang time in samples.
	Hang int
}

// NewAGC returns Csdr::Agc<float>. It delays its input by 100 samples
// (look-ahead).
func NewAGC(p AGC) (*Stage[float32, float32], error) {
	if p.Reference <= 0 || p.Attack <= 0 || p.Decay <= 0 || p.MaxGain <= 0 || p.Hang < 0 {
		return nil, fmt.Errorf("%w: agc %+v", ErrBuild, p)
	}

	return newStage[float32, float32](C.msdr_agc_new(C.float(p.Reference), C.float(p.Attack), C.float(p.Decay), C.float(p.MaxGain), C.uint(p.Hang)), "agc")
}

// NewResampler returns Csdr::AudioResampler (libsamplerate, sinc medium
// quality) from inRate to outRate. inRate need not be an integer: a
// channel rate is the device rate divided by an integer.
func NewResampler(inRate float64, outRate int) (*Stage[float32, float32], error) {
	if inRate <= 0 || outRate <= 0 {
		return nil, fmt.Errorf("%w: resampler %g → %d", ErrBuild, inRate, outRate)
	}

	return newStage[float32, float32](C.msdr_resampler_new(C.double(float64(outRate)/inRate)), "resampler")
}

// NewAMDemod returns Csdr::AmDemod: the magnitude of each sample.
func NewAMDemod() (*Stage[complex64, float32], error) {
	return newStage[complex64, float32](C.msdr_amdemod_new(), "am demod")
}

// NewDCBlock returns Csdr::DcBlock: a one-pole DC blocker.
func NewDCBlock() (*Stage[float32, float32], error) {
	return newStage[float32, float32](C.msdr_dcblock_new(), "dc block")
}

// NewRealPart returns Csdr::Realpart: the in-phase part of each sample.
func NewRealPart() (*Stage[complex64, float32], error) {
	return newStage[complex64, float32](C.msdr_realpart_new(), "real part")
}

// NewBandPass returns Csdr::FftBandPassFilter (Hamming window): a complex
// band-pass filter from low to high with the given transition width, all
// relative to the sample rate. It works on whole FFT blocks and holds one
// block back.
func NewBandPass(low, high, transition float32) (*Stage[complex64, complex64], error) {
	// Written so that NaN fails every comparison.
	if !(low >= -0.5 && low < high && high <= 0.5 && transition > 0 && transition <= 0.5) {
		return nil, fmt.Errorf("%w: band-pass [%g, %g] transition %g", ErrBuild, low, high, transition)
	}

	return newStage[complex64, complex64](C.msdr_bandpass_new(C.float(low), C.float(high), C.float(transition)), "band-pass")
}

// NoiseFilter is Csdr::NoiseFilter<float>: a spectral gate that keeps the
// FFT bins above the average power times a threshold.
type NoiseFilter struct {
	*Stage[float32, float32]
}

// NewNoiseFilter returns a noise filter on fftSize-sample blocks (half a
// block of output per block; one block held back).
func NewNoiseFilter(fftSize int, thresholdDB float32) (*NoiseFilter, error) {
	if fftSize < 32 {
		return nil, fmt.Errorf("%w: noise filter size %d", ErrBuild, fftSize)
	}

	s, err := newStage[float32, float32](C.msdr_noisefilter_new(C.uint(fftSize), C.float(thresholdDB)), "noise filter")
	if err != nil {
		return nil, err
	}

	return &NoiseFilter{Stage: s}, nil
}

// SetThreshold changes the gate (dB).
func (s *NoiseFilter) SetThreshold(db float32) error {
	if s.h == nil {
		return ErrClosed
	}

	if C.msdr_noisefilter_set_threshold(s.h, C.float(db)) != 0 {
		return ErrProcess
	}

	return nil
}
