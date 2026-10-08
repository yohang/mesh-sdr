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
// in the node with libcsdr++. The rows are streamed to the listener; each
// image long enough is sent to Files as PNG (FIL-005).

// ImageSchema names the image.v1 payload of the image decoders: a start
// (kept by the hub), then the live rows and the end of each image.
const ImageSchema = "image.v1"

// ImageRecord is the image.v1 payload.
type ImageRecord struct {
	// Event is start, rows or end.
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
	// Rows: the first row, the number of rows and their pixels (RGB or
	// grey, row after row), base64.
	Row    *int   `json:"row,omitempty"`
	Count  int    `json:"count,omitempty"`
	Pixels []byte `json:"pixels,omitempty"`
	// End: the lines received, whether the image is complete, whether it
	// was too short to be kept and whether it was sent to Files.
	Lines    int  `json:"lines,omitempty"`
	Complete bool `json:"complete,omitempty"`
	Short    bool `json:"short,omitempty"`
	Sent     bool `json:"sent,omitempty"`
}

// FAXSettings are the fax_* settings the hub pushes (DEC-038).
type FAXSettings struct {
	LPM, MinLength, MaxLength int
	PostProcess, Color, AM    bool
}

// DefaultFAX are the FAX settings of a node the hub has not configured.
var DefaultFAX = FAXSettings{LPM: 120, MinLength: 200, MaxLength: 1500, PostProcess: true}

// File caps of FIL-005: 8 MiB, 16 MiB for FAX. A FAX page whose pixels
// would exceed its cap ends there (colour pages).
const (
	maxImageFile = 8 << 20
	maxFAXFile   = 16 << 20
)

// maxRowsPayload bounds the pixels of one rows message (the media
// WebSocket carries messages of at most 1 MiB of JSON).
const maxRowsPayload = 256 << 10

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

// Queue of an image session.
const (
	// imageQueue is the depth of the audio queue, in blocks (about 2 s of
	// 20 ms blocks): beyond it, blocks are dropped.
	imageQueue = 100
	// dropReport is the shortest interval between two warnings about
	// dropped blocks.
	dropReport = 10 * time.Second
)

// imageSession is a running SSTV or FAX decoder: the demodulator's tap
// queues audio for its goroutine, which decodes it.
type imageSession struct {
	kind string
	rx   *dsp.ImageReceiver
	ev   app.DecoderEvents
	// minLines returns the shortest image kept, in lines.
	minLines func(m *dsp.Image) int
	// maxFile is the size cap of a file of the session.
	maxFile int
	lpm     int
	now     func() time.Time
	log     *slog.Logger

	mu       sync.Mutex
	in       chan app.AudioBlock
	closed   bool
	dropped  int
	reported time.Time

	// start is the reception start of the current image (goroutine only).
	start time.Time
}

// startImage starts an SSTV or FAX session.
func (r *Runner) startImage(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
	s := &imageSession{
		kind: spec.Mode.Name, ev: ev, in: make(chan app.AudioBlock, imageQueue), now: r.o.Now, maxFile: maxImageFile,
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

		s.lpm, s.maxFile = f.LPM, maxFAXFile
		s.rx, err = dsp.NewFAXReceiver(csdr.FAXOptions{LPM: f.LPM, MaxLines: f.MaxLength, AM: f.AM, PostProcess: f.PostProcess, Color: f.Color}, maxFAXFile)
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
		s.dropped++

		if now := s.now(); now.Sub(s.reported) >= dropReport {
			s.log.Warn("image decoder too slow: audio blocks dropped", slog.Int("blocks", s.dropped))
			s.dropped, s.reported = 0, now
		}
	}
}

// Close implements app.DecoderRun: the goroutine ends after the queued
// audio and keeps the image in progress, if long enough.
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
		s.events(evs, at)
	}

	if m := s.rx.Close(); m != nil && !failed {
		s.finish(m, time.Time{})
	}
}

// events reports the events of one Feed at time at: the rows that follow
// each other go in one message.
func (s *imageSession) events(evs []dsp.ImageEvent, at time.Time) {
	var (
		rows        *dsp.Image
		first, last int
	)

	flush := func() {
		if rows == nil {
			return
		}

		m, row := rows, first
		n := m.Width * m.Channels
		s.decode(at, "", ImageRecord{
			Event: "rows", Kind: s.kind, Width: m.Width, Height: m.Height, Channels: m.Channels,
			Row: &row, Count: last - first + 1, Pixels: m.Pix[first*n : (last+1)*n],
		}, true)

		rows = nil
	}

	for _, e := range evs {
		m := e.Image

		if e.Kind == dsp.ImageRow {
			if rows != m || e.Row != last+1 || (e.Row-first+1)*m.Width*m.Channels > maxRowsPayload {
				flush()

				rows, first = m, e.Row
			}

			last = e.Row

			continue
		}

		flush()

		switch e.Kind {
		case dsp.ImageStart:
			s.started(m, at)
		case dsp.ImageEnd:
			s.finish(m, at)
		case dsp.ImageRow:
		}
	}

	flush()
}

// started reports the start of an image (kept by the hub).
func (s *imageSession) started(m *dsp.Image, at time.Time) {
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
}

// finish ends an image: one long enough is sent to Files.
func (s *imageSession) finish(m *dsp.Image, end time.Time) {
	short := m.Lines < s.minLines(m)
	text := fmt.Sprintf("Image received: %d of %d lines", m.Lines, m.Height)
	sent := false

	if short {
		s.log.Debug("short image discarded", slog.Int("lines", m.Lines), slog.Int("height", m.Height))

		text += ", too short to be kept"
	} else if err := s.send(m, end); err != nil {
		s.log.Warn("decoded image not sent to Files", slog.Int("lines", m.Lines), slog.Any("error", err))

		text += ", not sent to Files"
	} else {
		sent = true
		text += ", sent to Files"
	}

	at := end
	if at.IsZero() {
		at = s.now()
	}

	s.decode(at, text, ImageRecord{
		Event: "end", Kind: s.kind, Width: m.Width, Height: m.Height, Channels: m.Channels,
		Lines: m.Lines, Complete: m.Complete, Short: short, Sent: sent,
	}, true)
}

// send encodes the image received as PNG for Files, with its FIL-008
// metadata.
func (s *imageSession) send(m *dsp.Image, end time.Time) error {
	if s.ev.File == nil || s.start.IsZero() {
		return fmt.Errorf("no file publisher or no image start")
	}

	data, err := encodePNG(m)
	if err != nil {
		return fmt.Errorf("encode PNG: %w", err)
	}

	if len(data) > s.maxFile {
		return fmt.Errorf("PNG of %d bytes, at most %d", len(data), s.maxFile)
	}

	meta := map[string]any{"lines_received": m.Lines, "complete": m.Complete}
	if s.kind == app.FileSSTV {
		meta["sstv_mode"], meta["vis_code"], meta["lines_total"] = sstvMode(m.VIS), m.VIS, m.Height
	} else {
		meta["lpm"], meta["ioc"] = s.lpm, m.IOC
	}

	return s.ev.File(app.ProducedFile{Kind: s.kind, Data: data, Start: s.start, End: end, Metadata: meta})
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
