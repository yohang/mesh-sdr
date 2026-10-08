package decoder

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// Session timings (§8.3 buffers, §8.4 timeouts).
const (
	// InputBuffer is the depth of the stdin buffer of a streaming decoder:
	// on overflow the oldest data is dropped (§8.3).
	InputBuffer = 2 * time.Second
	// StopGrace is the SIGTERM grace of a decoder (§8.4).
	StopGrace = 3 * time.Second
)

// iqFormat is how a wide IQ decoder reads its input on stdin.
type iqFormat int

const (
	// iqNone: the decoder reads audio (s16le at its input rate).
	iqNone iqFormat = iota
	// iqCF32: complex float32 little-endian (rtl_433 -r cf32:-).
	iqCF32
	// iqRealS16: the real part, as s16le (the skimmers, OpenWebRX+'s
	// RealPart on a band above the dial).
	iqRealS16
)

// adapter is the descriptor of a decoder tool (§8.4): its program, its
// argv for a session, its input, stderr rules and output parser: stdout
// lines, or the AX.25 frames of direwolf's KISS pseudo-terminal. Later
// decoders add theirs to adapters.
type adapter struct {
	tool  string
	args  func(c sessionConfig) []string
	rules []process.Rule
	// iq is the input of a wide IQ mode.
	iq iqFormat
	// lines returns the parser of the stdout lines of a session.
	lines func(c sessionConfig) lineParser
	// frames, when set, returns the parser of the AX.25 frames of a
	// session: the tool is direwolf, its frames come from its KISS
	// pseudo-terminal (direwolf.go) named on its stdout.
	frames func(c sessionConfig) frameParser
	// textLog: the live records of a session are kept as text logs and
	// sent to Files (FIL-005: the skimmers).
	textLog bool
}

// lineParser parses one output line of a tool into zero or more records.
type lineParser func(line string, at time.Time) []app.DecodeRecord

// frameParser parses one AX.25 frame into zero or one record.
type frameParser func(frame []byte, at time.Time) []app.DecodeRecord

// sessionConfig is what an adapter builds a session from.
type sessionConfig struct {
	variant  string
	settings Settings
	// dial returns the dial frequency (nil in tests).
	dial func() int64
}

// lineOf adapts a stateless parser of one record per line.
func lineOf(fn func(line string, at time.Time) (app.DecodeRecord, bool)) func(sessionConfig) lineParser {
	return func(sessionConfig) lineParser {
		return func(line string, at time.Time) []app.DecodeRecord {
			if rec, ok := fn(line, at); ok {
				return []app.DecodeRecord{rec}
			}

			return nil
		}
	}
}

// adapters are the adapters of the digital modes, by mode name.
var adapters = map[string]adapter{
	"selcall": {tool: "multimon-ng", args: multimonArgs, rules: multimonRules, lines: lineOf(parseSelCall)},
	"zvei":    {tool: "multimon-ng", args: multimonArgs, rules: multimonRules, lines: lineOf(parseSelCall)},
	"page":    {tool: "multimon-ng", args: pagingArgs, rules: multimonRules, lines: newPagingParser},
	"eas":     {tool: "multimon-ng", args: easArgs, rules: multimonRules, lines: lineOf(parseEAS)},
	"packet":  {tool: "direwolf", args: direwolfArgs, rules: direwolfRules, frames: newPacketParser},
	"cwskimmer": {
		tool: "csdr-cwskimmer", args: skimmerArgs, rules: skimmerRules, iq: iqRealS16, lines: newSkimmerParser("CW"), textLog: true,
	},
	"rttyskimmer": {
		tool: "csdr-rttyskimmer", args: skimmerArgs, rules: skimmerRules, iq: iqRealS16, lines: newSkimmerParser("RTTY"), textLog: true,
	},
	"ism":   {tool: "rtl_433", args: ismArgs, rules: rtl433Rules, iq: iqCF32, lines: newISMParser("ISM")},
	"wmbus": {tool: "rtl_433", args: wmbusArgs, rules: rtl433Rules, iq: iqCF32, lines: newISMParser("WMBUS")},
}

// Options configure the runner.
type Options struct {
	Supervisor *process.Supervisor
	Tools      process.Tools
	// Limits apply to every decoder process (decoders.process_limits).
	Limits process.Limits
	// MaxRestarts returns the crash-loop threshold (decoders.max_restarts
	// pushed by the hub); 0 keeps the §8.4 default (5).
	MaxRestarts func() int
	// Reprobe is called when a decoder tool went missing (exit 127 or
	// ENOENT): the capabilities are probed and reported again (§8.4).
	Reprobe func()
	// FAX returns the FAX settings pushed by the hub (DefaultFAX when
	// nil).
	FAX func() FAXSettings
	// Text returns the settings of the text decoders (digimodes_fft_size,
	// cw_showcw pushed by the hub); nil: the defaults.
	Text   func() TextSettings
	Logger *slog.Logger
	// Queue runs the jobs of the slot decoders (DEC-025).
	Queue *Queue
	// Settings returns the decoding settings pushed by the hub (slot
	// decoders, paging, ISM); nil: the defaults.
	Settings func() Settings
	// ClockSynced reports whether the node clock is synchronised (NTP and
	// within 1 s of the hub's): slot decoders warn when it is not
	// (DEC-026).
	ClockSynced func() bool
	// Now is the decode time source (tests).
	Now func() time.Time
}

// Runner implements app.DecoderRunner with external tools.
type Runner struct {
	o Options
}

// NewRunner returns the runner.
func NewRunner(o Options) *Runner {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}

	if o.Now == nil {
		o.Now = time.Now
	}

	return &Runner{o: o}
}

// ErrNoAdapter is a digital mode without an adapter on this node.
var ErrNoAdapter = errors.New("no decoder adapter")

// Start implements app.DecoderRunner: the tool of the session runs in a
// private workdir (DEC-048) under the decoder restart policy until Close;
// the image decoders run in the node.
func (r *Runner) Start(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
	// The native image decoders run in the node (DEC-037, DEC-038).
	if spec.Mode.Name == app.FileSSTV || spec.Mode.Name == app.FileFAX {
		return r.startImage(spec, ev)
	}

	// The slot decoders run batch jobs on the node queue (DEC-025).
	if spec.Mode.Slot > 0 && spec.Mode.Input == domain.InputAudio && len(profiles(spec.Mode.Name, Settings{})) > 0 {
		if r.o.Supervisor == nil {
			return nil, process.ErrNoRuntimeDir
		}

		return r.startSlots(spec, ev)
	}

	if spec.Mode.Cap == domain.CapNativeDSP {
		return r.startText(spec, ev)
	}

	ad, ok := adapters[spec.Mode.Name]
	if !ok || (ad.iq == iqNone) != (spec.Mode.Input == domain.InputAudio) {
		return nil, fmt.Errorf("%w for %s", ErrNoAdapter, spec.Mode.Name)
	}

	if r.o.Supervisor == nil {
		return nil, process.ErrNoRuntimeDir
	}

	path, err := r.o.Tools.Resolve(ad.tool)
	if err != nil {
		return nil, err
	}

	policy := process.DecoderPolicy()
	if r.o.MaxRestarts != nil {
		if n := r.o.MaxRestarts(); n > 0 {
			policy.CrashLoopCount = n
		}
	}

	cfg := sessionConfig{variant: spec.Variant, dial: spec.DialHz}
	if r.o.Settings != nil {
		cfg.settings = r.o.Settings()
	}

	// 2 s of input: s16 audio or real part, or cf32.
	sampleBytes := 2
	if ad.iq == iqCF32 {
		sampleBytes = 8
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		r: r, ev: ev, cancel: cancel, rate: spec.Mode.InputRate, iq: ad.iq, dial: spec.DialHz,
		buf: process.NewDropOldest(int(InputBuffer.Seconds() * float64(spec.Mode.InputRate) * float64(sampleBytes))),
		log: r.o.Logger.With(slog.String("session_id", spec.Session.String()), slog.String("mode", spec.Mode.Name)),
	}

	if ad.textLog {
		s.text = &textLog{}
	}

	var in *process.Instance

	ps := process.Spec{
		ID: "dec-" + spec.Session.String(), Kind: "decoder", Mode: process.Streaming, Path: path, Args: ad.args(cfg),
		ToolDirs: r.o.Tools.Dirs,
		// The first data written is the tool's readiness: most decoders
		// print nothing until they decode something. direwolf gets its
		// audio once the session reads its KISS pseudo-terminal: the frames
		// it decodes before are lost.
		Stdin: func(ctx context.Context, w io.Writer) error {
			if ad.frames != nil {
				if err := s.waitKISS(ctx, s.currentKISS()); err != nil {
					return err
				}
			}

			return s.buf.Feed(ctx, &touchWriter{w: w, touch: func() { in.Touch() }})
		},
		StderrRules: ad.rules,
		Sink:        s.event,
		Timeouts:    process.Timeouts{Stop: StopGrace},
		Restart:     policy,
		Limits:      r.o.Limits,
	}

	if ad.frames != nil {
		parse := ad.frames(cfg)
		ps.Prepare = writeDirewolfConfig
		ps.PerRun = s.kissRun(ps.Args)
		ps.Stdout = func(ctx context.Context, rd io.Reader) error { return s.direwolfStdout(ctx, rd, parse) }
	} else {
		parse := ad.lines(cfg)
		ps.Stdout = func(_ context.Context, rd io.Reader) error {
			process.ScanLines(rd, func(text string, _ bool) {
				for _, rec := range parse(text, r.o.Now()) {
					s.decode(rec)
				}
			})

			return nil
		}
	}

	in, err = r.o.Supervisor.NewInstance(ps)
	if err != nil {
		cancel()

		return nil, err
	}

	if s.text != nil {
		go s.logTicks(ctx)
	}

	go func() {
		// Terminal states were reported and logged by the supervisor; a
		// workdir that cannot be created stops the session before any.
		if err := in.Run(ctx); errors.Is(err, process.ErrWorkdir) {
			s.log.Error("decoder workdir not created", slog.Any("error", err))
			s.status(app.DecoderStatus{State: app.DecoderError, Reason: "workdir"})
		}

		// The tool has exited: its last lines are in the text log.
		if s.text != nil {
			s.sendLog(s.text.flush(s.r.o.Now()))
		}
	}()

	return s, nil
}

// touchWriter marks the instance ready on the first successful write.
type touchWriter struct {
	w       io.Writer
	touch   func()
	touched bool
}

func (t *touchWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err == nil && !t.touched {
		t.touched = true
		t.touch()
	}

	return n, err
}

// session is one running decoder session.
type session struct {
	r      *Runner
	ev     app.DecoderEvents
	cancel context.CancelFunc
	buf    *process.DropOldest
	rate   int
	iq     iqFormat
	log    *slog.Logger
	// text is the text log of a skimmer session (nil for other modes).
	text *textLog
	// dial returns the dial frequency (nil in tests).
	dial func() int64

	mu     sync.Mutex
	conv   *dsp.S16Converter
	raw    []byte
	closed bool
	last   app.DecoderStatus
	// kiss is the KISS link of direwolf's current run.
	kiss *kissRun
	// lastDial is the last known dial frequency.
	lastDial int64
}

// Audio implements app.DecoderRun: the audio is converted to s16le at the
// tool's input rate and buffered for its stdin (never blocking).
func (s *session) Audio(b app.AudioBlock) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || b.Rate <= 0 || s.iq != iqNone {
		return
	}

	if s.conv == nil || s.conv.InRate() != b.Rate {
		if s.conv != nil {
			s.conv.Close()
		}

		c, err := dsp.NewS16Converter(b.Rate, s.rate)
		if err != nil {
			s.log.Warn("decoder input not converted", slog.Int("rate", b.Rate), slog.Any("error", err))
			s.conv = nil

			return
		}

		s.conv = c
	}

	out, err := s.conv.Convert(b.Samples)
	if err != nil {
		s.log.Warn("decoder input not converted", slog.Any("error", err))

		return
	}

	if len(out) > 0 {
		_, _ = s.buf.Write(out)
	}
}

// IQ implements app.DecoderRun: the tools read no selector IQ.
func (s *session) IQ(app.IQBlock) {}

// Retune implements app.DecoderRun: the tools have no secondary selector.
func (s *session) Retune(float64) {}

// SpectrumSize implements app.DecoderRun: no secondary FFT.
func (s *session) SpectrumSize() int { return 0 }

// Spectrum implements app.DecoderRun: no secondary FFT.
func (s *session) Spectrum(int) {}

// WideIQ implements app.DecoderRun: the wide IQ (at the tool's input rate)
// is written as cf32 or as the s16le real part to the stdin buffer (never
// blocking).
func (s *session) WideIQ(b app.WideIQBlock) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || b.Rate != s.rate || s.iq == iqNone || len(b.Samples) == 0 {
		return
	}

	s.raw = s.raw[:0]

	for _, v := range b.Samples {
		if s.iq == iqCF32 {
			s.raw = binary.LittleEndian.AppendUint32(s.raw, math.Float32bits(real(v)))
			s.raw = binary.LittleEndian.AppendUint32(s.raw, math.Float32bits(imag(v)))
		} else {
			s.raw = binary.LittleEndian.AppendUint16(s.raw, uint16(dsp.ToS16(real(v))))
		}
	}

	_, _ = s.buf.Write(s.raw)
}

// Close implements app.DecoderRun: nothing is reported to the listener
// after it. The text log of a skimmer is saved once the tool has exited.
func (s *session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return
	}

	s.closed = true

	if s.conv != nil {
		s.conv.Close()
		s.conv = nil
	}
	s.mu.Unlock()

	s.buf.Close()
	s.cancel()
}

// decode reports a record unless the session is closed; a skimmer's live
// record also goes to its text log, until the tool has exited (its last
// lines are kept; the log is saved then).
func (s *session) decode(rec app.DecodeRecord) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()

	if s.text != nil && rec.Live {
		s.sendLog(s.text.add(rec, s.dialHz(), s.r.o.Now()))
	}

	if !closed {
		s.ev.Decode(rec)
	}
}

// dialHz returns the dial frequency, the last known one once the
// demodulator is gone (0 before any).
func (s *session) dialHz() int64 {
	var hz int64
	if s.dial != nil {
		hz = s.dial()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if hz > 0 {
		s.lastDial = hz
	}

	return s.lastDial
}

// logTicks ends the idle lines of the text log and saves it when it covers
// an hour, until ctx ends.
func (s *session) logTicks(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sendLog(s.text.tick(s.r.o.Now()))
		}
	}
}

// sendLog hands a full text log to Files.
func (s *session) sendLog(f *app.ProducedFile) {
	if f != nil && s.ev.File != nil {
		s.ev.File(*f)
	}
}

// status reports a status change unless the session is closed.
func (s *session) status(st app.DecoderStatus) {
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

// event maps a supervisor transition to the session status (DEC-002):
// running once the tool reads its input, unavailable when the tool is
// missing or refuses its configuration, error on any other failure.
func (s *session) event(e process.Event) {
	var st app.DecoderStatus

	switch e.State {
	case process.StateRunning:
		if e.Diag != process.DiagNone {
			return
		}

		st = app.DecoderStatus{State: app.DecoderRunning}
	case process.StateUnavailable:
		st = app.DecoderStatus{State: app.DecoderUnavailable, Reason: e.Reason}
	case process.StateRetryWait, process.StateCrashLoop, process.StateErrored, process.StateFailed, process.StateTimedOut:
		st = app.DecoderStatus{State: app.DecoderError, Reason: e.Reason}
	default:
		return
	}

	if e.Reprobe && s.r.o.Reprobe != nil {
		s.r.o.Reprobe()
	}

	s.status(st)
}
