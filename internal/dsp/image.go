package dsp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp/csdr"
)

// Image receivers (DEC-037 SSTV, DEC-038 HF FAX): the libcsdr++ decoder
// writes a BMP stream, which is cut into images and rows here.

// Image is an image being received. Its rows are RGB (Channels 3) or grey
// (Channels 1), top to bottom.
type Image struct {
	Width, Height int
	Channels      int
	// Pix holds the rows written so far (Received: the rows received).
	Pix []byte
	// Rows is the number of rows written, Lines the number of rows
	// received: an SSTV image cut short ends with blank rows, a FAX page
	// with the rows of its end mark, neither counted.
	Rows, Lines int
	// VIS is the VIS code of an SSTV image; IOC the index of cooperation
	// of a FAX page.
	VIS, IOC int
	// Complete: the decoder reached the end of the image (its last line,
	// the FAX stop tone or the longest page) rather than being stopped.
	Complete bool
}

// Row returns row y.
func (m *Image) Row(y int) []byte {
	n := m.Width * m.Channels

	return m.Pix[y*n : (y+1)*n]
}

// Received returns the rows received, the first Lines rows.
func (m *Image) Received() []byte { return m.Pix[:m.Lines*m.Width*m.Channels] }

// ImageEventKind is the kind of an image event.
type ImageEventKind int

// Image events.
const (
	// ImageStart: a new image (its header).
	ImageStart ImageEventKind = iota + 1
	// ImageRow: row Row of the image was written.
	ImageRow
	// ImageEnd: the image is over.
	ImageEnd
)

// ImageEvent is an event of an image receiver. Image is shared with the
// later events of the same image.
type ImageEvent struct {
	Kind  ImageEventKind
	Image *Image
	Row   int
}

// Limits of the images a receiver accepts from its decoder.
const (
	maxImageSide = 16384
	// imageOut is the room a decoder gets per call: the SSTV decoder
	// writes the blank rows of an image cut short at once (at most 1.5 MB,
	// PD-290).
	imageOut = 2 << 20
	// imageBlock bounds the input of one decoder call: the FAX decoder
	// stalls when one call brings more samples than a line (it needs
	// 2 lines buffered at most).
	imageBlock = 1024
)

// The FAX decoder's shortest line (the highest LPM) is longer than an
// input block (compile-time check).
const _ = uint(csdr.FAXRate*60/csdr.MaxFAXLPM - imageBlock)

// faxEndMark starts the padding rows of a FAX page.
var faxEndMark = []byte("END-PAGE!")

// ImageReceiver decodes SSTV or FAX images from demodulated audio. It is
// not safe for concurrent use.
type ImageReceiver struct {
	dec  *csdr.ImageDecoder
	fax  bool
	rate int

	rs     *csdr.Stage[float32, float32]
	inRate int
	buf    []float32
	out    []byte

	// pending are the stream bytes not parsed yet.
	pending []byte
	cur     *Image
	// skip: the current image ended, its rows left are dropped (the FAX
	// end marks, or the rows beyond the cap).
	skip bool
	// maxBytes caps the pixels of an image (0: no cap).
	maxBytes int
	events   []ImageEvent
}

// NewSSTVReceiver returns a receiver of SSTV images.
func NewSSTVReceiver() (*ImageReceiver, error) {
	d, err := csdr.NewSSTVDecoder(csdr.SSTVRate)
	if err != nil {
		return nil, err
	}

	return &ImageReceiver{dec: d, rate: csdr.SSTVRate, out: make([]byte, imageOut)}, nil
}

// NewFAXReceiver returns a receiver of FAX pages; a page whose pixels
// would exceed maxBytes (0: no cap) ends there, incomplete.
func NewFAXReceiver(o csdr.FAXOptions, maxBytes int) (*ImageReceiver, error) {
	d, err := csdr.NewFAXDecoder(csdr.FAXRate, o)
	if err != nil {
		return nil, err
	}

	return &ImageReceiver{dec: d, fax: true, rate: csdr.FAXRate, out: make([]byte, imageOut), maxBytes: maxBytes}, nil
}

// Lag returns how far the decoder works behind the last input sample.
func (r *ImageReceiver) Lag() time.Duration {
	return time.Duration(r.dec.Pending()) * time.Second / time.Duration(r.rate)
}

// Feed decodes audio at rate Hz and returns the events it caused, valid
// until the next call.
func (r *ImageReceiver) Feed(audio []float32, rate int) ([]ImageEvent, error) {
	r.events = r.events[:0]

	if rate != r.inRate {
		if err := r.resampleFrom(rate); err != nil {
			return nil, err
		}
	}

	if r.rs != nil {
		r.buf = grow(r.buf, int(float64(len(audio)+r.rs.Pending())*float64(r.rate)/float64(rate))+stepMargin)

		n, err := r.rs.Process(audio, r.buf)
		if err != nil {
			return nil, err
		}

		audio = r.buf[:n]
	}

	for len(audio) > 0 {
		k := min(len(audio), imageBlock)

		n, err := r.dec.Process(audio[:k], r.out)
		if err != nil {
			return nil, err
		}

		audio = audio[k:]
		r.pending = append(r.pending, r.out[:n]...)
		r.parse()
	}

	return r.events, nil
}

func (r *ImageReceiver) resampleFrom(rate int) error {
	if rate <= 0 {
		return fmt.Errorf("%w: image decoder input %d Hz", ErrChain, rate)
	}

	if r.rs != nil {
		r.rs.Close()
		r.rs = nil
	}

	if rate != r.rate {
		rs, err := csdr.NewResampler(float64(rate), r.rate)
		if err != nil {
			return err
		}

		r.rs = rs
	}

	r.inRate = rate

	return nil
}

// parse cuts the pending stream into images and rows.
func (r *ImageReceiver) parse() {
	for {
		if r.cur == nil {
			if !r.header() {
				return
			}

			continue
		}

		// A row is longer than a header (480 bytes at least).
		n := r.cur.Width * r.cur.Channels
		if len(r.pending) < n {
			return
		}

		// A new image started before the rows of this one ended: an SSTV
		// decoder that lost the start sync of a Scottie image writes its
		// header and no row; a FAX page restarted before its end marks.
		if r.isHeader(r.pending) {
			if !r.skip {
				r.end(false)
			}

			r.cur, r.skip = nil, false

			continue
		}

		row := r.pending[:n]

		switch {
		case r.fax && bytes.HasPrefix(row, faxEndMark):
			if !r.skip {
				r.end(true)
				r.skip = true
			}
		case r.skip:
		case r.maxBytes > 0 && len(r.cur.Pix)+n > r.maxBytes:
			// The page would exceed the file cap: it ends here.
			r.end(false)
			r.skip = true
		default:
			r.row(row)
		}

		r.pending = r.pending[n:]
		r.cur.Rows++

		if r.cur.Rows == r.cur.Height {
			if !r.skip {
				// The SSTV decoder blanks the rows of an image it lost;
				// a FAX page reached its longest length.
				r.end(r.fax || r.cur.Lines == r.cur.Height)
			}

			r.cur, r.skip = nil, false
		}
	}
}

// sstvWidths are the line widths of the SSTV modes of libcsdr++.
var sstvWidths = map[int]bool{160: true, 256: true, 320: true, 512: true, 640: true, 800: true}

// isHeader reports whether b starts with a BMP header of the receiver's
// decoder: 24-bit with 's' in byte 7 for SSTV, 8-bit with IOC/4 in byte 6
// for FAX.
func (r *ImageReceiver) isHeader(b []byte) bool {
	if len(b) < 54 || b[0] != 'B' || b[1] != 'M' || binary.LittleEndian.Uint32(b[14:]) != 40 || binary.LittleEndian.Uint16(b[26:]) != 1 {
		return false
	}

	w := int(int32(binary.LittleEndian.Uint32(b[18:])))
	h := -int(int32(binary.LittleEndian.Uint32(b[22:])))
	bpp := int(binary.LittleEndian.Uint16(b[28:]))
	off := int(binary.LittleEndian.Uint32(b[10:]))

	if h < 1 || h > maxImageSide {
		return false
	}

	if !r.fax {
		return b[7] == 0x73 && bpp == 24 && off == 54 && sstvWidths[w]
	}

	return (b[6] == 144 && w == 1812 || b[6] == 72 && w == 908) && bpp == 8 && off == 54+1024
}

// header parses the BMP header at the start of the pending stream, if
// whole; garbage before it is dropped.
func (r *ImageReceiver) header() bool {
	i := bytes.Index(r.pending, []byte("BM"))
	if i < 0 {
		r.pending = r.pending[:0]

		return false
	}

	r.pending = r.pending[i:]
	if len(r.pending) < 54 {
		return false
	}

	h := r.pending
	if !r.isHeader(h) {
		r.pending = r.pending[2:]

		return true
	}

	off := int(binary.LittleEndian.Uint32(h[10:]))
	if len(r.pending) < off {
		return false
	}

	m := &Image{
		Width: int(binary.LittleEndian.Uint32(h[18:])), Height: -int(int32(binary.LittleEndian.Uint32(h[22:]))),
		Channels: int(binary.LittleEndian.Uint16(h[28:])) / 8,
	}

	if r.fax {
		m.IOC = int(h[6]) * 4
	} else {
		m.VIS = int(h[6])
	}

	size := m.Width * m.Height * m.Channels
	if r.maxBytes > 0 {
		size = min(size, r.maxBytes)
	}

	m.Pix = make([]byte, 0, size)
	r.pending = r.pending[off:]
	r.cur = m
	r.events = append(r.events, ImageEvent{Kind: ImageStart, Image: m})

	return true
}

// row appends a row, BGR turned into RGB.
func (r *ImageReceiver) row(row []byte) {
	m := r.cur
	y := m.Rows
	m.Pix = append(m.Pix, row...)

	if m.Channels == 3 {
		px := m.Pix[len(m.Pix)-len(row):]
		for i := 0; i+2 < len(px); i += 3 {
			px[i], px[i+2] = px[i+2], px[i]
		}
	}

	// A blank SSTV row is a lost or missing line.
	if r.fax || !blank(row) {
		m.Lines = y + 1
	}

	r.events = append(r.events, ImageEvent{Kind: ImageRow, Image: m, Row: y})
}

func blank(row []byte) bool {
	for _, c := range row {
		if c != 0 {
			return false
		}
	}

	return true
}

func (r *ImageReceiver) end(complete bool) {
	r.cur.Complete = complete
	r.events = append(r.events, ImageEvent{Kind: ImageEnd, Image: r.cur})
}

// Close releases the decoder and returns the image in progress, if any:
// it is not complete.
func (r *ImageReceiver) Close() *Image {
	m := r.cur
	if r.skip {
		m = nil
	}

	r.cur = nil
	r.dec.Close()

	if r.rs != nil {
		r.rs.Close()
		r.rs = nil
	}

	return m
}
