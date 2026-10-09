package decoder

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// Session timings (§8.3 buffers, §8.4 timeouts).
const (
	// inputBuffer is the depth of the input buffer of a decoder: on
	// overflow the oldest data is dropped (§8.3).
	inputBuffer = 2 * time.Second
	// stopGrace is the SIGTERM grace of a decoder (§8.4).
	stopGrace = 3 * time.Second
)

// Settings are the decoding settings the hub pushes in the desired state
// (Admin › Decoding), read when a session starts or, for the slot
// decoders, at each slot (DEC-021). A zero scalar takes the node default;
// the enabled FST4, FST4W, Q65 and JS8 lists come filled.
type Settings struct {
	// MaxRestarts is the crash-loop threshold of decoder processes
	// (decoders.max_restarts); 0 keeps the §8.4 default (5).
	MaxRestarts int
	// FFTSize is the secondary FFT size of the text decoders
	// (digimodes_fft_size; 0: defaultFFTSize).
	FFTSize int
	// ShowCW: the CW decoder also prints dots and dashes (cw_showcw).
	ShowCW bool
	// DSCShowErrors keeps the DSC error lines: runs of symbols that are
	// not a call (dsc_show_errors).
	DSCShowErrors bool
	// FAX are the FAX settings (fax_*); a zero LPM takes defaultFAX.
	FAX FAXSettings
	// WSJTDepth is wsjt_decoding_depth (1 to 3, default 3).
	WSJTDepth int
	// WSJTDepths are the per-mode depths wsjt_decoding_depths[mode]; a
	// missing or zero entry takes WSJTDepth (JT65 defaults to 1).
	WSJTDepths map[string]int
	// FST4Intervals and FST4WIntervals are the enabled T/R periods in
	// seconds (fst4_enabled_intervals, fst4w_enabled_intervals).
	FST4Intervals, FST4WIntervals []int
	// Q65Combinations are the enabled submode and period combinations
	// ("A30", q65_enabled_combinations).
	Q65Combinations []string
	// JS8Profiles are the enabled JS8 speeds (normal, slow, fast, turbo).
	JS8Profiles []string
	// JS8Depth is js8_decoding_depth (1 to 3, default 3).
	JS8Depth int
	// PagingFilter keeps only the readable pages (DEC-033).
	PagingFilter bool
	// PagingCharset is the POCSAG charset of multimon-ng (US, FR, DE, DK,
	// SE or SI); "" is US.
	PagingCharset string
	// ISMReportLevels keeps the signal levels of rtl_433 in the ISM
	// decodes (DEC-039).
	ISMReportLevels bool
}

// withDefaults fills the zero scalars with the node defaults.
func (s Settings) withDefaults() Settings {
	if s.FFTSize <= 0 {
		s.FFTSize = defaultFFTSize
	}

	if s.FAX.LPM == 0 {
		s.FAX = defaultFAX
	}

	return s
}

// Options configure the runner.
type Options struct {
	Supervisor *process.Supervisor
	Tools      process.Tools
	// Limits apply to every decoder process (decoders.process_limits).
	Limits process.Limits
	// Settings returns the decoding settings pushed by the hub; nil: the
	// defaults.
	Settings func() Settings
	// Reprobe is called when a decoder tool went missing (exit 127 or
	// ENOENT): the capabilities are probed and reported again (§8.4).
	Reprobe func()
	Logger  *slog.Logger
	// Queue runs the jobs of the slot decoders (DEC-025).
	Queue *Queue
	// ClockSynced reports whether the node clock is synchronised (NTP and
	// within 1 s of the hub's): slot decoders warn when it is not
	// (DEC-026).
	ClockSynced func() bool
	// Now is the decode time source (tests).
	Now func() time.Time
}

// Runner implements app.DecoderRunner with external tools and the native
// decoders.
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

	if o.Reprobe == nil {
		o.Reprobe = func() {}
	}

	if o.ClockSynced == nil {
		o.ClockSynced = func() bool { return true }
	}

	get := o.Settings
	o.Settings = func() Settings {
		var s Settings
		if get != nil {
			s = get()
		}

		return s.withDefaults()
	}

	return &Runner{o: o}
}

// errNoDecoder is a digital mode without a decoder on this node.
var errNoDecoder = errors.New("no decoder adapter")

// Start implements app.DecoderRunner, by decoder family: the image and
// text decoders run in the node, the slot decoders run batch jobs on the
// node queue (DEC-025) and the others a streaming tool.
func (r *Runner) Start(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
	switch spec.Mode.Family {
	case domain.FamilyImage:
		return r.startImage(spec, ev)
	case domain.FamilyTextModes, domain.FamilyDSC:
		return r.startText(spec, ev)
	case domain.FamilyWSJT, domain.FamilyJS8:
		return r.startSlots(spec, ev)
	default:
		return r.startTool(spec, ev)
	}
}

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
	// iqFMS16: the FM discriminator output, flat (no de-emphasis), as
	// s16le (AIS through direwolf, OpenWebRX+'s FmDemod).
	iqFMS16
)

// toolSpec is the descriptor of a decoder tool (§8.4): its program, its
// argv for a session, its input, stderr rules and output parser: stdout
// lines, or the AX.25 frames of direwolf's KISS pseudo-terminal.
type toolSpec struct {
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

// sessionConfig is what a toolSpec builds a session from.
type sessionConfig struct {
	variant  string
	settings Settings
	// dial returns the dial frequency (nil in tests).
	dial func() int64
}

// single turns the result of a parser of one record into records.
func single(rec app.DecodeRecord, ok bool) []app.DecodeRecord {
	if !ok {
		return nil
	}

	return []app.DecodeRecord{rec}
}

// lineOf adapts a stateless parser of one record per line.
func lineOf(fn func(line string, at time.Time) (app.DecodeRecord, bool)) func(sessionConfig) lineParser {
	return func(sessionConfig) lineParser {
		return func(line string, at time.Time) []app.DecodeRecord { return single(fn(line, at)) }
	}
}

// toolSpecs are the streaming tool decoders, by mode name.
var toolSpecs = map[string]toolSpec{
	"selcall": {tool: "multimon-ng", args: multimonArgs, rules: multimonRules, lines: lineOf(parseSelCall)},
	"zvei":    {tool: "multimon-ng", args: multimonArgs, rules: multimonRules, lines: lineOf(parseSelCall)},
	"page":    {tool: "multimon-ng", args: pagingArgs, rules: multimonRules, lines: newPagingParser},
	"eas":     {tool: "multimon-ng", args: easArgs, rules: multimonRules, lines: lineOf(parseEAS)},
	"packet":  {tool: "direwolf", args: direwolfArgs, rules: direwolfRules, frames: newPacketParser},
	"ais":     {tool: "direwolf", args: aisArgs, rules: direwolfRules, iq: iqFMS16, frames: newAISParser},
	"cwskimmer": {
		tool: "csdr-cwskimmer", args: skimmerArgs, rules: skimmerRules, iq: iqRealS16, lines: newSkimmerParser("CW"), textLog: true,
	},
	"rttyskimmer": {
		tool: "csdr-rttyskimmer", args: skimmerArgs, rules: skimmerRules, iq: iqRealS16, lines: newSkimmerParser("RTTY"), textLog: true,
	},
	"ism":   {tool: "rtl_433", args: ismArgs, rules: rtl433Rules, iq: iqCF32, lines: newISMParser("ISM")},
	"wmbus": {tool: "rtl_433", args: wmbusArgs, rules: rtl433Rules, iq: iqCF32, lines: newISMParser("WMBUS")},
}

// startTool starts a streaming tool session: the tool runs in a private
// workdir (DEC-048) under the decoder restart policy until Close.
func (r *Runner) startTool(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
	ts, ok := toolSpecs[spec.Mode.Name]
	if !ok || (ts.iq == iqNone) != (spec.Mode.Input == domain.InputAudio) {
		return nil, fmt.Errorf("%w for %s", errNoDecoder, spec.Mode.Name)
	}

	if r.o.Supervisor == nil {
		return nil, process.ErrNoRuntimeDir
	}

	path, err := r.o.Tools.Resolve(ts.tool)
	if err != nil {
		return nil, err
	}

	cfg := sessionConfig{variant: spec.Variant, settings: r.o.Settings(), dial: spec.Dial}

	policy := process.DecoderPolicy()
	if n := cfg.settings.MaxRestarts; n > 0 {
		policy.CrashLoopCount = n
	}

	// 2 s of input: s16 audio or real part, or cf32.
	sampleBytes := 2
	if ts.iq == iqCF32 {
		sampleBytes = 8
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &toolSession{
		sessionBase: sessionBase{ev: ev, log: r.sessionLog(spec)},
		r:           r, cancel: cancel, rate: spec.Mode.InputRate, iq: ts.iq, dial: spec.Dial,
		buf:  process.NewDropOldest(int(inputBuffer.Seconds() * float64(spec.Mode.InputRate) * float64(sampleBytes))),
		conv: dsp.NewS16Converter(spec.Mode.InputRate),
	}

	if ts.textLog {
		s.text = &textLog{}
	}

	var in *process.Instance

	ps := process.Spec{
		ID: "dec-" + spec.Session.String(), Kind: "decoder", Mode: process.Streaming, Path: path, Args: ts.args(cfg),
		ToolDirs: r.o.Tools.Dirs,
		// The first data written is the tool's readiness: most decoders
		// print nothing until they decode something. direwolf gets its
		// audio once the session reads its KISS pseudo-terminal: the frames
		// it decodes before are lost.
		Stdin: func(ctx context.Context, w io.Writer) error {
			if ts.frames != nil {
				if err := s.waitKISS(ctx, s.currentKISS()); err != nil {
					return err
				}
			}

			return s.buf.Feed(ctx, &touchWriter{w: w, touch: func() { in.Touch() }})
		},
		StderrRules: ts.rules,
		Sink:        r.sink(false, s.status),
		Timeouts:    process.Timeouts{Stop: stopGrace},
		Restart:     policy,
		Limits:      r.o.Limits,
	}

	if ts.frames != nil {
		parse := ts.frames(cfg)
		ps.Prepare = writeDirewolfConfig
		ps.PerRun = s.kissRun(ps.Args)
		ps.Stdout = func(ctx context.Context, rd io.Reader) error { return s.direwolfStdout(ctx, rd, parse) }
	} else {
		parse := ts.lines(cfg)
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

// toolSession is one running streaming tool session.
type toolSession struct {
	sessionBase

	r      *Runner
	cancel context.CancelFunc
	buf    *process.DropOldest
	rate   int
	iq     iqFormat
	// text is the text log of a skimmer session (nil for other modes).
	text *textLog
	// dial returns the dial frequency (nil in tests).
	dial func() int64

	// Guarded by mu.
	conv *dsp.S16Converter
	disc dsp.FMDiscriminator
	raw  []byte
	// kiss is the KISS link of direwolf's current run.
	kiss *kissRun
	// lastDial is the last known dial frequency.
	lastDial int64
}

// Audio implements app.DecoderRun: the audio is converted to s16le at the
// tool's input rate and buffered for its stdin (never blocking).
func (s *toolSession) Audio(b app.AudioBlock) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || b.Rate <= 0 || s.iq != iqNone {
		return
	}

	if out := s.convert(s.conv, b); len(out) > 0 {
		_, _ = s.buf.Write(out)
	}
}

// WideIQ implements app.DecoderRun: the wide IQ (at the tool's input rate)
// is written as cf32, as the s16le real part or as the s16le FM
// discriminator output to the stdin buffer (never blocking).
func (s *toolSession) WideIQ(b app.WideIQBlock) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || b.Rate != s.rate || s.iq == iqNone || len(b.Samples) == 0 {
		return
	}

	if s.iq == iqFMS16 {
		if b.Discontinuity {
			s.disc.Reset()
		}

		if out := s.convert(s.conv, app.AudioBlock{Samples: s.disc.Process(b.Samples), Rate: b.Rate}); len(out) > 0 {
			_, _ = s.buf.Write(out)
		}

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
func (s *toolSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return
	}

	s.closed = true
	s.conv.Close()
	s.mu.Unlock()

	s.buf.Close()
	s.cancel()
}

// decode reports a record unless the session is closed; a skimmer's live
// record also goes to its text log, until the tool has exited (its last
// lines are kept; the log is saved then).
func (s *toolSession) decode(rec app.DecodeRecord) {
	if s.text != nil && rec.Live {
		s.sendLog(s.text.add(rec, s.dialHz(), s.r.o.Now()))
	}

	s.sessionBase.decode(rec)
}

// dialHz returns the dial frequency, the last known one once the
// demodulator is gone (0 before any).
func (s *toolSession) dialHz() int64 {
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
func (s *toolSession) logTicks(ctx context.Context) {
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
func (s *toolSession) sendLog(f *app.ProducedFile) {
	if f != nil && s.ev.File != nil {
		s.ev.File(*f)
	}
}
