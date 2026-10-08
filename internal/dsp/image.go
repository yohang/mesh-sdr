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
	// stops if it gets more than one line of samples at once.
	imageBlock = 1024
)

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
	// padding: the rows left of the current image are FAX end marks.
	padding bool
	events  []ImageEvent
}

// NewSSTVReceiver returns a receiver of SSTV images.
func NewSSTVReceiver() (*ImageReceiver, error) {
	d, err := csdr.NewSSTVDecoder(csdr.SSTVRate)
	if err != nil {
		return nil, err
	}

	return &ImageReceiver{dec: d, rate: csdr.SSTVRate, out: make([]byte, imageOut)}, nil
}

// NewFAXReceiver returns a receiver of FAX pages.
func NewFAXReceiver(o csdr.FAXOptions) (*ImageReceiver, error) {
	d, err := csdr.NewFAXDecoder(csdr.FAXRate, o)
	if err != nil {
		return nil, err
	}

	return &ImageReceiver{dec: d, fax: true, rate: csdr.FAXRate, out: make([]byte, imageOut)}, nil
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
		r.buf = Grow(r.buf, int(float64(len(audio)+r.rs.Pending())*float64(r.rate)/float64(rate))+stepMargin)

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

		n := r.cur.Width * r.cur.Channels
		if len(r.pending) < n {
			return
		}

		row := r.pending[:n]

		if r.fax && bytes.HasPrefix(row, faxEndMark) {
			if !r.padding {
				r.end(true)
				r.padding = true
			}
		} else if r.padding {
			// A new page started before the end marks of the last one
			// were all written.
			r.cur, r.padding = nil, false

			continue
		} else {
			r.row(row)
		}

		r.pending = r.pending[n:]
		r.cur.Rows++

		if r.cur.Rows == r.cur.Height {
			if !r.padding {
				// The SSTV decoder blanks the rows of an image it lost;
				// a FAX page reached its longest length.
				r.end(r.fax || r.cur.Lines == r.cur.Height)
			}

			r.cur, r.padding = nil, false
		}
	}
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
	off := int(binary.LittleEndian.Uint32(h[10:]))
	w := int(int32(binary.LittleEndian.Uint32(h[18:])))
	height := -int(int32(binary.LittleEndian.Uint32(h[22:])))
	bpp := int(binary.LittleEndian.Uint16(h[28:]))

	if off < 54 || off > 54+1024 || w < 1 || w > maxImageSide || height < 1 || height > maxImageSide || (bpp != 8 && bpp != 24) {
		r.pending = r.pending[2:]

		return true
	}

	if len(r.pending) < off {
		return false
	}

	m := &Image{Width: w, Height: height, Channels: bpp / 8}
	if r.fax {
		m.IOC = int(h[6]) * 4
	} else {
		m.VIS = int(h[6])
	}

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
	if r.padding {
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
