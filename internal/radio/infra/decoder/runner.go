package decoder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// adapter is the descriptor of a decoder tool (§8.4): its program, its
// argv for a variant of the mode, stderr rules and stdout parser. Later
// decoders add theirs to adapters.
type adapter struct {
	tool  string
	args  func(variant string) []string
	rules []process.Rule
	parse func(line string, at time.Time) (app.DecodeRecord, bool)
}

// adapters are the adapters of the digital modes, by mode name.
var adapters = map[string]adapter{
	"selcall": {tool: "multimon-ng", args: multimonArgs, rules: multimonRules, parse: parseSelCall},
	"zvei":    {tool: "multimon-ng", args: multimonArgs, rules: multimonRules, parse: parseSelCall},
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
	FAX    func() FAXSettings
	Logger *slog.Logger
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

	ad, ok := adapters[spec.Mode.Name]
	if !ok || spec.Mode.Input != domain.InputAudio {
		return nil, fmt.Errorf("%w for %s", ErrNoAdapter, spec.Mode.Name)
	}

	if r.o.Supervisor == nil {
		return nil, errors.New("node.runtime_dir is not set: no tool can run")
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

	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		r: r, ev: ev, cancel: cancel, rate: spec.Mode.InputRate,
		buf: process.NewDropOldest(int(InputBuffer.Seconds() * float64(spec.Mode.InputRate) * 2)),
		log: r.o.Logger.With(slog.String("session_id", spec.Session.String()), slog.String("mode", spec.Mode.Name)),
	}

	var in *process.Instance

	in, err = r.o.Supervisor.NewInstance(process.Spec{
		ID: "dec-" + spec.Session.String(), Kind: "decoder", Mode: process.Streaming, Path: path, Args: ad.args(spec.Variant),
		ToolDirs: r.o.Tools.Dirs,
		// The first data written is the tool's readiness: most decoders
		// print nothing until they decode something.
		Stdin: func(ctx context.Context, w io.Writer) error {
			return s.buf.Feed(ctx, &touchWriter{w: w, touch: func() { in.Touch() }})
		},
		Stdout: func(_ context.Context, rd io.Reader) error {
			process.ScanLines(rd, func(text string, _ bool) {
				if rec, ok := ad.parse(text, r.o.Now()); ok {
					s.decode(rec)
				}
			})

			return nil
		},
		StderrRules: ad.rules,
		Sink:        s.event,
		Timeouts:    process.Timeouts{Stop: StopGrace},
		Restart:     policy,
		Limits:      r.o.Limits,
	})
	if err != nil {
		cancel()

		return nil, err
	}

	go func() {
		// Terminal states were reported and logged by the supervisor; a
		// workdir that cannot be created stops the session before any.
		if err := in.Run(ctx); errors.Is(err, process.ErrWorkdir) {
			s.log.Error("decoder workdir not created", slog.Any("error", err))
			s.status(app.DecoderStatus{State: app.DecoderError, Reason: "workdir"})
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
	log    *slog.Logger

	mu     sync.Mutex
	conv   *dsp.S16Converter
	closed bool
	last   app.DecoderStatus
}

// Audio implements app.DecoderRun: the audio is converted to s16le at the
// tool's input rate and buffered for its stdin (never blocking).
func (s *session) Audio(b app.AudioBlock) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || b.Rate <= 0 {
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

// Close implements app.DecoderRun: nothing is reported after it.
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

// decode reports a record unless the session is closed.
func (s *session) decode(rec app.DecodeRecord) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()

	if !closed {
		s.ev.Decode(rec)
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
