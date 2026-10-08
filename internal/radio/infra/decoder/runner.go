package decoder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
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

// adapter is the descriptor of a decoder tool (§8.4): its program, the
// argv of each of its processes, stderr rules and stdout parser. Later
// decoders add theirs to adapters.
type adapter struct {
	tool string
	// argv holds the arguments of every process of a session: one process
	// per decoder when several decoders of one tool would mix their output
	// on one stdout (multimon-ng writes a line in pieces).
	argv  [][]string
	rules []process.Rule
	parse func(line string, at time.Time) (app.DecodeRecord, bool)
}

// adapters are the adapters of the digital modes, by mode name.
var adapters = map[string]adapter{
	"selcall": {tool: "multimon-ng", argv: multimonArgv(selcallDecoders), rules: multimonRules, parse: parseSelCall},
	"zvei":    {tool: "multimon-ng", argv: multimonArgv(zveiDecoders), rules: multimonRules, parse: parseSelCall},
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
	Logger  *slog.Logger
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

// Start implements app.DecoderRunner: the processes of the session run in
// private workdirs (DEC-048) under the decoder restart policy until Close.
func (r *Runner) Start(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
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
		log:    r.o.Logger.With(slog.String("session_id", spec.Session.String()), slog.String("mode", spec.Mode.Name)),
		states: make([]app.DecoderStatus, len(ad.argv)),
	}

	instances := make([]*process.Instance, len(ad.argv))

	for i, args := range ad.argv {
		buf := process.NewDropOldest(int(InputBuffer.Seconds() * float64(s.rate) * 2))
		s.bufs = append(s.bufs, buf)

		id := "dec-" + spec.Session.String()
		if len(ad.argv) > 1 {
			id += "-" + strconv.Itoa(i)
		}

		in, err := r.o.Supervisor.NewInstance(process.Spec{
			ID: id, Kind: "decoder", Mode: process.Streaming, Path: path, Args: args, ToolDirs: r.o.Tools.Dirs,
			// The first data written is the tool's readiness: most
			// decoders print nothing until they decode something.
			Stdin: func(ctx context.Context, w io.Writer) error {
				return buf.Feed(ctx, &touchWriter{w: w, touch: func() { instances[i].Touch() }})
			},
			Stdout: func(_ context.Context, rd io.Reader) error {
				process.ScanLines(rd, func(text string, _ bool) {
					if rec, ok := ad.parse(text, r.o.Now()); ok {
						ev.Decode(rec)
					}
				})

				return nil
			},
			StderrRules: ad.rules,
			Sink:        func(e process.Event) { s.event(i, e) },
			Timeouts:    process.Timeouts{Stop: StopGrace},
			Restart:     policy,
			Limits:      r.o.Limits,
		})
		if err != nil {
			cancel()

			return nil, err
		}

		instances[i] = in
	}

	for _, in := range instances {
		// Terminal errors were reported (status) and logged by the
		// supervisor.
		go func() { _ = in.Run(ctx) }()
	}

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

// session is one running decoder session: one or more processes fed with
// the same input.
type session struct {
	r      *Runner
	ev     app.DecoderEvents
	cancel context.CancelFunc
	bufs   []*process.DropOldest
	rate   int
	log    *slog.Logger

	mu     sync.Mutex
	conv   *dsp.S16Converter
	closed bool
	states []app.DecoderStatus
	last   app.DecoderStatus
}

// Audio implements app.DecoderRun: the audio is converted to s16le at the
// tool's input rate and buffered for the stdin of each process (never
// blocking).
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

	if len(out) == 0 {
		return
	}

	for _, buf := range s.bufs {
		_, _ = buf.Write(out)
	}
}

// Close implements app.DecoderRun.
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

	for _, buf := range s.bufs {
		buf.Close()
	}

	s.cancel()
}

// statusRank orders the states of the processes of a session: the session
// shows the worst one.
var statusRank = map[string]int{"": 0, app.DecoderRunning: 1, app.DecoderError: 2, app.DecoderUnavailable: 3}

// event maps a supervisor transition of process i to the session status
// (DEC-002): running once every tool reads its input, unavailable when a
// tool is missing or refuses its configuration, error on any other
// failure.
func (s *session) event(i int, e process.Event) {
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

	s.mu.Lock()
	s.states[i] = st

	// A failed process makes the session fail; it runs once every process
	// runs.
	var worst app.DecoderStatus

	running := 0

	for _, x := range s.states {
		if x.State == app.DecoderRunning {
			running++
		}

		if statusRank[x.State] > statusRank[worst.State] {
			worst = x
		}
	}

	if worst.State == app.DecoderRunning && running < len(s.states) {
		worst = app.DecoderStatus{}
	}

	changed := worst.State != "" && worst != s.last && !s.closed
	if changed {
		s.last = worst
	}
	s.mu.Unlock()

	if changed {
		s.ev.Status(worst)
	}
}
