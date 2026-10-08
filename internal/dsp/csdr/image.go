package csdr

/*
#include "shim_image.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// ImageDecoder is a libcsdr++ image decoder, Csdr::SstvDecoder or
// Csdr::FaxDecoder: it reads float audio and writes a BMP byte stream, a
// header when an image starts and then its rows, top to bottom. An
// ImageDecoder is not safe for concurrent use.
type ImageDecoder struct {
	h *C.msdr_image
}

// Input rates the image decoders are built for (OpenWebRX+ chains), and
// the LPM range of the FAX decoder.
const (
	SSTVRate  = 24000
	FAXRate   = 12000
	MinFAXLPM = 30
	MaxFAXLPM = 480
)

// NewSSTVDecoder returns Csdr::SstvDecoder at rate Hz: it finds the VIS
// header, then writes a 24-bit BMP header (VIS code in byte 6) and one BGR
// row per scanline. It works on 2 s of input at least: its output lags
// that much behind.
func NewSSTVDecoder(rate int) (*ImageDecoder, error) {
	if rate < 8000 || rate > 192000 {
		return nil, fmt.Errorf("%w: sstv rate %d", ErrBuild, rate)
	}

	return newImage(C.msdr_sstv_new(C.uint(rate)), "sstv")
}

// FAXOptions are the parameters of Csdr::FaxDecoder (the fax_* settings).
type FAXOptions struct {
	// LPM is the line rate in lines per minute.
	LPM int
	// MaxLines ends a page that runs this long.
	MaxLines int
	// AM decodes amplitude instead of frequency modulation.
	AM bool
	// PostProcess filters the noise of each row with its neighbours.
	PostProcess bool
}

// NewFAXDecoder returns Csdr::FaxDecoder at rate Hz, greyscale: on a
// start tone it writes an 8-bit BMP header with its palette and IOC/4 in
// byte 6, then one row per line. A page that ends before MaxLines (stop
// tone) is padded with rows that start with "END-PAGE!". Colour pages
// await a libcsdr++ fix (shim_image.h).
func NewFAXDecoder(rate int, o FAXOptions) (*ImageDecoder, error) {
	if rate < 8000 || rate > 192000 || o.LPM < MinFAXLPM || o.LPM > MaxFAXLPM || o.MaxLines < 1 || o.MaxLines > 100000 {
		return nil, fmt.Errorf("%w: fax rate %d %+v", ErrBuild, rate, o)
	}

	var opt C.uint
	if o.AM {
		opt |= C.MSDR_FAX_AM
	}

	if o.PostProcess {
		opt |= C.MSDR_FAX_POST
	}

	return newImage(C.msdr_fax_new(C.uint(rate), C.uint(o.LPM), C.uint(o.MaxLines), opt), "fax")
}

func newImage(h *C.msdr_image, what string) (*ImageDecoder, error) {
	if h == nil {
		return nil, fmt.Errorf("%w: %s", ErrBuild, what)
	}

	return &ImageDecoder{h: h}, nil
}

// Process appends in to the carry, runs the decoder and writes at most
// len(out) bytes of its stream to out. It returns the number of bytes
// written. out must have room for the rows a call may write: the SSTV
// decoder writes nothing of the blank rows that complete a cut image when
// they do not fit at once.
func (d *ImageDecoder) Process(in []float32, out []byte) (int, error) {
	if d.h == nil {
		return 0, ErrClosed
	}

	var inPtr *C.float
	if len(in) > 0 {
		inPtr = (*C.float)(unsafe.Pointer(unsafe.SliceData(in)))
	}

	var outPtr *C.uchar
	if len(out) > 0 {
		outPtr = (*C.uchar)(unsafe.Pointer(unsafe.SliceData(out)))
	}

	n := C.msdr_image_process(d.h, inPtr, C.size_t(len(in)), outPtr, C.size_t(len(out)))
	if n < 0 {
		return 0, ErrProcess
	}

	return int(n), nil
}

// Pending returns the number of input samples waiting in the carry.
func (d *ImageDecoder) Pending() int {
	if d.h == nil {
		return 0
	}

	return int(C.msdr_image_pending(d.h))
}

// canaryIntact reports whether the SSTV decoder stayed within half its
// slack (the PD-290 overflow, tests).
func (d *ImageDecoder) canaryIntact() bool { return C.msdr_sstv_canary_intact(d.h) != 0 }

// Close releases the decoder. It is safe to call twice.
func (d *ImageDecoder) Close() {
	if d != nil && d.h != nil {
		C.msdr_image_free(d.h)
		d.h = nil
	}
}
