package decoder

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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

// defaultFFTSize is the secondary FFT size without hub settings.
const defaultFFTSize = 2048

// Text output of the sessions.
const (
	// TextSchema names the text.v1 payload: {"text": line}.
	TextSchema = "text.v1"
	// maxLine is the longest line: a longer one is cut, at a space when
	// there is one in its last lineSlack characters.
	maxLine   = 80
	lineSlack = 20
	// lineIdle ends a line nothing was added to for that long.
	lineIdle = 5 * time.Second
	// partialEvery is the shortest interval between two partial records.
	partialEvery = 250 * time.Millisecond
	// textTick checks the idle line while no input arrives.
	textTick = time.Second
)

// TextRecord is the text.v1 payload.
type TextRecord struct {
	Text string `json:"text"`
}

type iqItem struct {
	samples []complex64
	rate    float64
	at      time.Time
}

// iqWeight weighs a block by its duration (µs): the input queue holds
// inputBuffer of IQ.
func iqWeight(it iqItem) int { return int(float64(len(it.samples)) * 1e6 / it.rate) }

// textSession is one native text decoder session.
type textSession struct {
	sessionBase

	now  func() time.Time
	cfg  dsp.TextConfig
	size int
	in   *dropQueue[iqItem]

	// Guarded by mu.
	offset float64
	retune bool
	// fps is the frame rate of the secondary FFT; 0: off.
	fps int
}

// startText starts a native text decoder session.
func (r *Runner) startText(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
	cfg, ok := textModes[spec.Mode.Name]
	if !ok || spec.Mode.Input != domain.InputNarrowIQ {
		return nil, fmt.Errorf("%w for %s", errNoDecoder, spec.Mode.Name)
	}

	set := r.o.Settings()
	cfg.BandwidthHz, cfg.OffsetHz, cfg.ShowCW = spec.Mode.BandwidthHz, spec.OffsetHz, set.ShowCW

	// The chain is built here so that a refused configuration fails the
	// start; the goroutine owns it from now on.
	dec, err := dsp.NewTextDecoder(cfg)
	if err != nil {
		return nil, err
	}

	s := &textSession{
		sessionBase: sessionBase{ev: ev, log: r.sessionLog(spec)},
		now:         r.o.Now, cfg: cfg, size: set.FFTSize, offset: spec.OffsetHz,
		in: newDropQueue(int(inputBuffer.Microseconds()), iqWeight),
	}

	go s.run(dec)

	return s, nil
}

// IQ implements app.DecoderRun: the block is copied into the input queue;
// beyond inputBuffer the oldest blocks are dropped.
func (s *textSession) IQ(b app.IQBlock) {
	if len(b.Samples) == 0 || b.Rate <= 0 {
		return
	}

	s.in.push(iqItem{samples: append([]complex64(nil), b.Samples...), rate: b.Rate, at: b.Time}, b.Discontinuity)
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
	s.closed = true
	s.mu.Unlock()

	s.in.discard()
}

// changes returns the pending changes.
func (s *textSession) changes() (retune bool, offset float64, fps int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	retune, s.retune = s.retune, false

	return retune, s.offset, s.fps
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
	c.rs.Close()

	if c.spec != nil {
		c.spec.Close()
	}
}

func (s *textSession) run(dec *dsp.TextDecoder) {
	c := &textChain{dec: dec, rs: dsp.NewIQResampler(dsp.TextRate)}
	defer c.close()

	lines := &lineAssembler{emit: s.decode, now: s.now}
	tick := time.NewTicker(textTick)
	defer tick.Stop()

	for {
		select {
		case <-tick.C:
			lines.idle()

			continue
		case <-s.in.wake:
		}

		items, closed := s.in.take()
		if closed {
			return
		}

		retune, offset, fps := s.changes()

		if retune {
			if err := c.dec.SetOffset(offset); err != nil {
				s.log.Warn("text decoder offset not applied", slog.Float64("offset_hz", offset), slog.Any("error", err))
			}
		}

		s.setSpectrum(c, fps)

		for _, it := range items {
			if err := s.process(c, lines, it); err != nil {
				s.log.Error("text decoder failed", slog.Any("error", err))
				// The input is dropped from now on.
				s.in.discard()
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
func (s *textSession) process(c *textChain, lines *lineAssembler, q queued[iqItem]) error {
	it := q.v

	// Lost input: the line ends, the CW timing and the resampler start
	// again.
	if q.gap {
		lines.flush()

		if err := c.dec.Reset(); err != nil {
			return err
		}

		c.rs.Close()
	}

	iq, err := c.rs.Process(it.samples, it.rate)
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
// at a line break, at maxLine characters or after lineIdle without a new
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

			if len(l.line) >= maxLine {
				l.wrap(at)
			}
		}
	}

	if added {
		l.last = l.now()
	}

	if len(l.line) > l.sent && l.now().Sub(l.partial) >= partialEvery {
		l.partial, l.sent = l.now(), len(l.line)
		l.emit(record(string(l.line), l.start, true))
	}
}

// wrap ends a full line, at its last space when there is one near the
// end; the rest starts the next line.
func (l *lineAssembler) wrap(at time.Time) {
	cut := strings.LastIndexByte(string(l.line), ' ')
	if cut < len(l.line)-lineSlack {
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

// idle ends a line nothing was added to for lineIdle.
func (l *lineAssembler) idle() {
	if len(l.line) > 0 && l.now().Sub(l.last) >= lineIdle {
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
