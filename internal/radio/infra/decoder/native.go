package decoder

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
)

// The native text decoders (DEC-006 to DEC-012): libcsdr++ chains run in
// the node, one goroutine per session (ADR 0014), fed by the selector IQ
// of the listener's demodulator. They print characters; the session cuts
// them into lines: the listener sees the line being printed (partial
// records), the hub gets each whole line (decode.batch).

// textModes are the chains of the native text decoders, by mode name, with
// the OpenWebRX+ parameters (owrx/dsp.py); the selector band is the
// catalogue's BandwidthHz.
var textModes = map[string]dsp.TextConfig{
	"bpsk31":    {Kind: dsp.TextPSK, Baud: 31.25},
	"bpsk63":    {Kind: dsp.TextPSK, Baud: 62.5},
	"rtty170":   {Kind: dsp.TextRTTY, Baud: 45.45},
	"rtty450":   {Kind: dsp.TextRTTY, Baud: 50, Invert: true},
	"rtty85":    {Kind: dsp.TextRTTY, Baud: 50, Invert: true},
	"sitorb":    {Kind: dsp.TextSITORB, Baud: 100},
	"cwdecoder": {Kind: dsp.TextCW},
}

// TextSettings are the hub settings of the text decoders (Admin ›
// Decoding), read when a session starts.
type TextSettings struct {
	// FFTSize is the secondary FFT size (digimodes_fft_size).
	FFTSize int
	// ShowCW: the CW decoder also prints dots and dashes (cw_showcw).
	ShowCW bool
}

// DefaultFFTSize is the secondary FFT size without hub settings.
const DefaultFFTSize = 2048

// Text output of the sessions.
const (
	// TextSchema names the text.v1 payload: {"text": line}.
	TextSchema = "text.v1"
	// MaxLine is the longest line: a longer one is cut, at a space when
	// there is one in its last LineSlack characters.
	MaxLine   = 80
	LineSlack = 20
	// LineIdle ends a line nothing was added to for that long.
	LineIdle = 5 * time.Second
	// PartialEvery is the shortest interval between two partial records.
	PartialEvery = 250 * time.Millisecond
	// textTick checks the idle line while no input arrives.
	textTick = time.Second
)

// TextRecord is the text.v1 payload.
type TextRecord struct {
	Text string `json:"text"`
}

// textQueue is the input buffer of a session: on overflow the oldest
// blocks are dropped (§8.3).
const textQueue = InputBuffer

type iqItem struct {
	samples []complex64
	rate    float64
	at      time.Time
	gap     bool
}

// textSession is one native text decoder session.
type textSession struct {
	ev   app.DecoderEvents
	log  *slog.Logger
	now  func() time.Time
	cfg  dsp.TextConfig
	size int
	wake chan struct{}
	done chan struct{}

	mu     sync.Mutex
	queue  []iqItem
	queued int
	gap    bool
	offset float64
	retune bool
	// fps is the frame rate of the secondary FFT; 0: off.
	fps    int
	closed bool
	// failed: the decoder stopped on an error; input is dropped.
	failed bool
	last   app.DecoderStatus
}

// startText starts a native text decoder session.
func (r *Runner) startText(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
	cfg, ok := textModes[spec.Mode.Name]
	if !ok || spec.Mode.Input != domain.InputNarrowIQ {
		return nil, fmt.Errorf("%w for %s", ErrNoAdapter, spec.Mode.Name)
	}

	set := TextSettings{FFTSize: DefaultFFTSize}
	if r.o.Text != nil {
		set = r.o.Text()
		if set.FFTSize <= 0 {
			set.FFTSize = DefaultFFTSize
		}
	}

	cfg.BandwidthHz, cfg.OffsetHz, cfg.ShowCW = spec.Mode.BandwidthHz, spec.OffsetHz, set.ShowCW

	// The chain is built here so that a refused configuration fails the
	// start; the goroutine owns it from now on.
	dec, err := dsp.NewTextDecoder(cfg)
	if err != nil {
		return nil, err
	}

	s := &textSession{
		ev: ev, now: r.o.Now, cfg: cfg, size: set.FFTSize, offset: spec.OffsetHz,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		log: r.o.Logger.With(slog.String("session_id", spec.Session.String()), slog.String("mode", spec.Mode.Name)),
	}

	go s.run(dec)

	return s, nil
}

// Audio implements app.DecoderRun: text decoders read IQ.
func (s *textSession) Audio(app.AudioBlock) {}

// IQ implements app.DecoderRun: the block is copied into the input queue;
// beyond InputBuffer the oldest blocks are dropped.
func (s *textSession) IQ(b app.IQBlock) {
	if len(b.Samples) == 0 || b.Rate <= 0 {
		return
	}

	s.mu.Lock()
	if s.closed || s.failed {
		s.mu.Unlock()

		return
	}

	limit := int(textQueue.Seconds() * b.Rate)
	for len(s.queue) > 0 && s.queued+len(b.Samples) > limit {
		s.queued -= len(s.queue[0].samples)
		s.queue = s.queue[1:]
		s.gap = true
	}

	s.queue = append(s.queue, iqItem{samples: append([]complex64(nil), b.Samples...), rate: b.Rate, at: b.Time, gap: b.Discontinuity || s.gap})
	s.queued += len(b.Samples)
	s.gap = false
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Retune implements app.DecoderRun.
func (s *textSession) Retune(offsetHz float64) {
	s.mu.Lock()
	s.offset, s.retune = offsetHz, true
	s.mu.Unlock()
}

// SpectrumSize implements app.DecoderRun.
func (s *textSession) SpectrumSize() int { return s.size }

// Spectrum implements app.DecoderRun.
func (s *textSession) Spectrum(fps int) {
	s.mu.Lock()
	s.fps = max(fps, 0)
	s.mu.Unlock()
}

// Close implements app.DecoderRun: nothing is reported after it.
func (s *textSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.closed {
		s.closed = true
		s.queue = nil
		close(s.done)
	}
}

// take returns the queued blocks and the pending changes.
func (s *textSession) take() (items []iqItem, retune bool, offset float64, fps int, closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	items, s.queue, s.queued = s.queue, nil, 0
	retune, s.retune = s.retune, false

	return items, retune, s.offset, s.fps, s.closed
}

// fail stops the input of a session whose decoder failed.
func (s *textSession) fail() {
	s.mu.Lock()
	s.failed, s.queue, s.queued = true, nil, 0
	s.mu.Unlock()
}

// status reports a status change unless the session is closed.
func (s *textSession) status(st app.DecoderStatus) {
	s.mu.Lock()
	changed := !s.closed && st != s.last
	if changed {
		s.last = st
	}
	s.mu.Unlock()

	if changed {
		s.ev.Status(st)
	}
}

// emit reports a record unless the session is closed.
func (s *textSession) emit(rec app.DecodeRecord) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()

	if !closed && s.ev.Decode != nil {
		s.ev.Decode(rec)
	}
}

// textChain is the DSP of a session, owned by its goroutine.
type textChain struct {
	dec  *dsp.TextDecoder
	rs   *dsp.IQResampler
	spec *dsp.Spectrum
	// pushed counts the samples given to spec.
	pushed uint64
	fft    []byte
}

func (c *textChain) close() {
	c.dec.Close()

	if c.rs != nil {
		c.rs.Close()
	}

	if c.spec != nil {
		c.spec.Close()
	}
}

func (s *textSession) run(dec *dsp.TextDecoder) {
	c := &textChain{dec: dec}
	defer c.close()

	lines := &lineAssembler{emit: s.emit, now: s.now}
	tick := time.NewTicker(textTick)
	defer tick.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
			lines.idle()

			continue
		case <-s.wake:
		}

		items, retune, offset, fps, closed := s.take()
		if closed {
			return
		}

		if retune {
			if err := c.dec.SetOffset(offset); err != nil {
				s.log.Warn("text decoder offset not applied", slog.Float64("offset_hz", offset), slog.Any("error", err))
			}
		}

		s.setSpectrum(c, fps)

		for _, it := range items {
			if err := s.process(c, lines, it); err != nil {
				s.log.Error("text decoder failed", slog.Any("error", err))
				s.fail()
				s.status(app.DecoderStatus{State: app.DecoderError, Reason: "decoder_failed"})

				return
			}
		}

		lines.idle()
	}
}

// setSpectrum builds, rebuilds at a new frame rate or drops the secondary
// FFT.
func (s *textSession) setSpectrum(c *textChain, fps int) {
	if c.spec != nil && c.spec.Config().FPS == fps {
		return
	}

	if c.spec != nil {
		c.spec.Close()
		c.spec = nil
	}

	if fps <= 0 {
		return
	}

	spec, err := dsp.NewSpectrum(dsp.SecondarySpectrum(s.size, fps))
	if err != nil {
		s.log.Warn("secondary fft not started", slog.Int("size", s.size), slog.Int("fps", fps), slog.Any("error", err))

		return
	}

	c.spec, c.pushed = spec, 0
}

// process runs one input block: resampling to the decoder rate, secondary
// FFT, decoder.
func (s *textSession) process(c *textChain, lines *lineAssembler, it iqItem) error {
	// Lost input: the line ends, the CW timing and the resampler start
	// again.
	if it.gap {
		lines.flush()

		if err := c.dec.Reset(); err != nil {
			return err
		}

		if c.rs != nil {
			c.rs.Close()
			c.rs = nil
		}
	}

	if c.rs == nil || c.rs.InRate() != it.rate {
		if c.rs != nil {
			c.rs.Close()
		}

		rs, err := dsp.NewIQResampler(it.rate, dsp.TextRate)
		if err != nil {
			return err
		}

		c.rs = rs
	}

	iq, err := c.rs.Process(it.samples)
	if err != nil {
		return err
	}

	if c.spec != nil && s.ev.Spectrum != nil {
		at := c.pushed
		c.pushed += uint64(len(iq))

		err := c.spec.Push(at, it.at, iq, func(l dsp.Line) {
			c.fft = dsp.EncodeFFTU8(c.fft[:0], rxv1.DefaultFFTU8Scale(), l.DB)
			s.ev.Spectrum(app.SpectrumFrame{Payload: c.fft, TimestampUS: uint64(max(l.Time.UnixMicro(), 0))})
		})
		if err != nil {
			return err
		}
	}

	text, err := c.dec.Process(iq)
	if err != nil {
		return err
	}

	s.status(app.DecoderStatus{State: app.DecoderRunning})
	lines.feed(text, it.at)

	return nil
}

// lineAssembler cuts the characters of a decoder into lines: a line ends
// at a line break, at MaxLine characters or after LineIdle without a new
// character. Only printable ASCII is kept (untrusted RF text).
type lineAssembler struct {
	emit func(app.DecodeRecord)
	now  func() time.Time

	line    []byte
	start   time.Time
	last    time.Time
	sent    int
	partial time.Time
}

// feed adds the characters of a block whose first sample is at.
func (l *lineAssembler) feed(text []byte, at time.Time) {
	added := false

	for _, b := range text {
		switch {
		case b == '\n' || b == '\r':
			l.flush()
		case b >= 0x20 && b < 0x7f:
			if len(l.line) == 0 {
				if b == ' ' {
					continue
				}

				l.start = at
			}

			l.line = append(l.line, b)
			added = true

			if len(l.line) >= MaxLine {
				l.wrap(at)
			}
		}
	}

	if added {
		l.last = l.now()
	}

	if len(l.line) > l.sent && l.now().Sub(l.partial) >= PartialEvery {
		l.partial, l.sent = l.now(), len(l.line)
		l.emit(record(string(l.line), l.start, true))
	}
}

// wrap ends a full line, at its last space when there is one near the
// end; the rest starts the next line.
func (l *lineAssembler) wrap(at time.Time) {
	cut := strings.LastIndexByte(string(l.line), ' ')
	if cut < len(l.line)-LineSlack {
		cut = len(l.line)
	}

	rest := strings.TrimLeft(string(l.line[min(cut, len(l.line)):]), " ")
	l.line = l.line[:cut]
	l.flush()
	l.line = append(l.line, rest...)

	if len(l.line) > 0 {
		l.start = at
	}
}

// idle ends a line nothing was added to for LineIdle.
func (l *lineAssembler) idle() {
	if len(l.line) > 0 && l.now().Sub(l.last) >= LineIdle {
		l.flush()
	}
}

// flush reports the current line, if any, as a whole line.
func (l *lineAssembler) flush() {
	text := strings.TrimRight(string(l.line), " ")
	l.line, l.sent = l.line[:0], 0

	if text != "" {
		l.emit(record(text, l.start, false))
	}
}

func record(text string, at time.Time, partial bool) app.DecodeRecord {
	payload, err := json.Marshal(TextRecord{Text: text})
	if err != nil {
		payload = []byte(`{}`)
	}

	return app.DecodeRecord{Time: at, Schema: TextSchema, Text: text, Payload: payload, Partial: partial}
}
