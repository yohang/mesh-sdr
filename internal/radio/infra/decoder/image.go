package decoder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/dsp/csdr"
	"github.com/yohang/mesh-sdr/internal/radio/app"
)

// The native image decoders: SSTV (DEC-037) and HF FAX (DEC-038), run
// in the node with libcsdr++. Each row is streamed to the listener; each
// image long enough is saved to Files as PNG (FIL-005).

// ImageSchema names the image.v1 payload of the image decoders: a start
// (kept by the hub), then the live rows and the end of each image.
const ImageSchema = "image.v1"

// ImageRecord is the image.v1 payload.
type ImageRecord struct {
	// Event is start, row or end.
	Event    string `json:"event"`
	Kind     string `json:"kind"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Channels int    `json:"channels"`
	// SSTV start.
	SSTVMode string `json:"sstv_mode,omitempty"`
	VISCode  *int   `json:"vis_code,omitempty"`
	// FAX start.
	IOC int `json:"ioc,omitempty"`
	LPM int `json:"lpm,omitempty"`
	// Row: its index and its pixels (RGB or grey), base64.
	Row    *int   `json:"row,omitempty"`
	Pixels []byte `json:"pixels,omitempty"`
	// End: the lines received, whether the image is complete and whether
	// it goes to Files.
	Lines    int  `json:"lines,omitempty"`
	Complete bool `json:"complete,omitempty"`
	Saved    bool `json:"saved,omitempty"`
}

// FAXSettings are the fax_* settings the hub pushes (DEC-038).
type FAXSettings struct {
	LPM, MinLength, MaxLength int
	PostProcess, Color, AM    bool
}

// DefaultFAX are the FAX settings of a node the hub has not configured.
var DefaultFAX = FAXSettings{LPM: 120, MinLength: 200, MaxLength: 1500, PostProcess: true}

// sstvModes name the SSTV modes of libcsdr++ by VIS code.
var sstvModes = map[int]string{
	0: "Robot 12", 4: "Robot 24", 8: "Robot 36", 12: "Robot 72",
	32: "Martin 4", 36: "Martin 3", 40: "Martin 2", 44: "Martin 1",
	48: "Scottie 4", 52: "Scottie 3", 56: "Scottie 2", 60: "Scottie 1", 76: "Scottie DX",
	51: "Wraase SC2-30", 55: "Wraase SC2-180", 59: "Wraase SC2-60", 63: "Wraase SC2-120",
	93: "PD-50", 94: "PD-290", 95: "PD-120", 96: "PD-180", 97: "PD-240", 98: "PD-160", 99: "PD-90",
}

func sstvMode(vis int) string {
	if n, ok := sstvModes[vis]; ok {
		return n
	}

	return "VIS " + strconv.Itoa(vis)
}

// imageQueue is the depth of the audio queue of an image session, in
// blocks (about 2 s of 20 ms blocks): beyond it, blocks are dropped.
const imageQueue = 100

// imageSession is a running SSTV or FAX decoder: the demodulator's tap
// queues audio for its goroutine, which decodes it.
type imageSession struct {
	kind string
	rx   *dsp.ImageReceiver
	ev   app.DecoderEvents
	// minLines returns the shortest image saved, in lines.
	minLines func(m *dsp.Image) int
	lpm      int
	now      func() time.Time
	log      *slog.Logger

	mu     sync.Mutex
	in     chan app.AudioBlock
	closed bool

	// start is the reception start of the current image (goroutine only).
	start time.Time
}

// startImage starts an SSTV or FAX session.
func (r *Runner) startImage(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
	s := &imageSession{
		kind: spec.Mode.Name, ev: ev, in: make(chan app.AudioBlock, imageQueue), now: r.o.Now,
		log: r.o.Logger.With(slog.String("session_id", spec.Session.String()), slog.String("mode", spec.Mode.Name)),
	}

	var err error

	switch spec.Mode.Name {
	case app.FileSSTV:
		s.rx, err = dsp.NewSSTVReceiver()
		// Images under half height are discarded (DEC-037).
		s.minLines = func(m *dsp.Image) int { return (m.Height + 1) / 2 }
	case app.FileFAX:
		f := DefaultFAX
		if r.o.FAX != nil {
			f = r.o.FAX()
		}

		s.lpm = f.LPM
		s.rx, err = dsp.NewFAXReceiver(csdr.FAXOptions{LPM: f.LPM, MaxLines: f.MaxLength, AM: f.AM, PostProcess: f.PostProcess, Color: f.Color})
		s.minLines = func(*dsp.Image) int { return f.MinLength }
	default:
		return nil, fmt.Errorf("%w for %s", ErrNoAdapter, spec.Mode.Name)
	}

	if err != nil {
		return nil, fmt.Errorf("start %s decoder: %w", spec.Mode.Name, err)
	}

	go s.run()

	return s, nil
}

// Audio implements app.DecoderRun: the block is copied and queued; a full
// queue drops it.
func (s *imageSession) Audio(b app.AudioBlock) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}

	b.Samples = append([]float32(nil), b.Samples...)

	select {
	case s.in <- b:
	default:
		s.log.Debug("image decoder late: audio block dropped")
	}
}

// IQ implements app.DecoderRun: the image decoders read audio.
func (s *imageSession) IQ(app.IQBlock) {}

// Retune implements app.DecoderRun: no secondary selector.
func (s *imageSession) Retune(float64) {}

// SpectrumSize implements app.DecoderRun: no secondary FFT.
func (s *imageSession) SpectrumSize() int { return 0 }

// Spectrum implements app.DecoderRun: no secondary FFT.
func (s *imageSession) Spectrum(int) {}

// Close implements app.DecoderRun: the goroutine ends after the queued
// audio and saves the image in progress, if long enough.
func (s *imageSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.closed {
		s.closed = true
		close(s.in)
	}
}

func (s *imageSession) run() {
	running, failed := false, false

	for b := range s.in {
		if failed {
			continue
		}

		if !running {
			running = true

			s.ev.Status(app.DecoderStatus{State: app.DecoderRunning})
		}

		evs, err := s.rx.Feed(b.Samples, b.Rate)
		if err != nil {
			s.log.Error("image decoder failed", slog.Any("error", err))
			s.ev.Status(app.DecoderStatus{State: app.DecoderError, Reason: "decoder_failed"})

			failed = true

			continue
		}

		// The decoder works behind the last sample of the block.
		at := b.Time.Add(time.Duration(len(b.Samples))*time.Second/time.Duration(max(b.Rate, 1)) - s.rx.Lag())

		for _, e := range evs {
			s.event(e, at)
		}
	}

	if m := s.rx.Close(); m != nil && !failed {
		s.finish(m, time.Time{})
	}
}

// event reports an event of the receiver at time at.
func (s *imageSession) event(e dsp.ImageEvent, at time.Time) {
	m := e.Image

	switch e.Kind {
	case dsp.ImageStart:
		s.start = at
		rec := ImageRecord{Event: "start", Kind: s.kind, Width: m.Width, Height: m.Height, Channels: m.Channels}

		var text string

		if s.kind == app.FileSSTV {
			vis := m.VIS
			rec.SSTVMode, rec.VISCode = sstvMode(vis), &vis
			text = fmt.Sprintf("%s (VIS %d), %d×%d", rec.SSTVMode, vis, m.Width, m.Height)
		} else {
			rec.IOC, rec.LPM = m.IOC, s.lpm
			text = fmt.Sprintf("FAX IOC %d, %d LPM, %d×%d", m.IOC, s.lpm, m.Width, m.Height)
		}

		s.decode(at, text, rec, false)
	case dsp.ImageRow:
		row := e.Row
		s.decode(at, "", ImageRecord{
			Event: "row", Kind: s.kind, Width: m.Width, Height: m.Height, Channels: m.Channels, Row: &row, Pixels: m.Row(row),
		}, true)
	case dsp.ImageEnd:
		s.finish(m, at)
	}
}

// finish ends an image: one long enough goes to Files.
func (s *imageSession) finish(m *dsp.Image, end time.Time) {
	saved := m.Lines >= s.minLines(m)
	if saved {
		s.save(m, end)
	} else {
		s.log.Debug("short image discarded", slog.Int("lines", m.Lines), slog.Int("height", m.Height))
	}

	text := fmt.Sprintf("Image received: %d of %d lines", m.Lines, m.Height)
	if saved {
		text += ", saved to Files"
	}

	at := end
	if at.IsZero() {
		at = s.now()
	}

	s.decode(at, text, ImageRecord{
		Event: "end", Kind: s.kind, Width: m.Width, Height: m.Height, Channels: m.Channels, Lines: m.Lines, Complete: m.Complete, Saved: saved,
	}, true)
}

// save encodes the image received as PNG for Files, with its FIL-008
// metadata.
func (s *imageSession) save(m *dsp.Image, end time.Time) {
	if s.ev.File == nil || s.start.IsZero() {
		return
	}

	data, err := encodePNG(m)
	if err != nil {
		s.log.Error("decoded image not encoded", slog.Any("error", err))

		return
	}

	meta := map[string]any{"lines_received": m.Lines, "complete": m.Complete}
	if s.kind == app.FileSSTV {
		meta["sstv_mode"], meta["vis_code"], meta["lines_total"] = sstvMode(m.VIS), m.VIS, m.Height
	} else {
		meta["lpm"], meta["ioc"] = s.lpm, m.IOC
	}

	s.ev.File(app.ProducedFile{Kind: s.kind, Data: data, Start: s.start, End: end, Metadata: meta})
}

// encodePNG encodes the rows received.
func encodePNG(m *dsp.Image) ([]byte, error) {
	rect := image.Rect(0, 0, m.Width, m.Lines)

	var img image.Image

	if m.Channels == 1 {
		img = &image.Gray{Pix: m.Received(), Stride: m.Width, Rect: rect}
	} else {
		rgba := image.NewNRGBA(rect)
		src := m.Received()

		for i, j := 0, 0; i+2 < len(src); i, j = i+3, j+4 {
			rgba.Pix[j], rgba.Pix[j+1], rgba.Pix[j+2], rgba.Pix[j+3] = src[i], src[i+1], src[i+2], 0xff
		}

		img = rgba
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decode reports a record of the session.
func (s *imageSession) decode(at time.Time, text string, rec ImageRecord, live bool) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return
	}

	s.ev.Decode(app.DecodeRecord{Time: at, Schema: ImageSchema, Text: text, Payload: payload, Live: live})
}
