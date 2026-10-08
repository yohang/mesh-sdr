// Package process supervises the external tools of a node (SDR connectors
// now, decoders and capability probes later): TECHNICAL_SPEC §8.2, §8.4,
// ADR 0017. It spawns argv only with a scrubbed environment in a private
// workdir under node.runtime_dir, puts each tool in its own process group
// with Pdeathsig, applies limits through the exec helper, classifies
// stderr and exits, applies timeouts and restart back-off, and reports
// every transition to a non-blocking sink. Linux only.
//
// It lives in the radio module, the first that needs it (ADR 0017 E2), and
// moves to a shared technical package when the decoder supervisor arrives.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Mode is the adapter kind (§8.4 descriptor field `kind`).
type Mode int

const (
	Streaming Mode = iota // long-lived, restarted with back-off
	Batch                 // one process per job, wall-clock deadline, never restarted
)

// Timeouts (§8.4 descriptor field `timeouts`).
type Timeouts struct {
	Start       time.Duration // readiness deadline: first stdout byte or Touch (0 = none)
	IdleOutput  time.Duration // liveness rule for tools with periodic output (0 = silence is normal)
	IdleStrikes int           // consecutive idle violations before restart (default 3)
	Job         time.Duration // batch wall clock, shared by reading and waiting
	Stop        time.Duration // SIGTERM grace before SIGKILL (default 3 s)
}

// Spec describes one supervised instance. It is the runtime form of an adapter
// descriptor (decoder) or a driver entry (connector), after argv templating.
type Spec struct {
	ID       string // instance id (session id / device id): workdir name, log attr
	Kind     string // "connector", "decoder", "probe": log and metrics label
	Mode     Mode
	Path     string   // absolute path of the tool (from tools.* or ResolveTool)
	Args     []string // argv[1:], already typed and validated by the adapter
	ToolDirs []string // PATH of the child
	Env      []string // extra declared variables, KEY=VALUE

	// Prepare runs after the workdir exists and before the first spawn
	// (generated config files, FIFOs).
	Prepare func(workdir string) error
	// Stdin, when set, is called once per run with the write end of the
	// child's stdin. Nil: stdin is /dev/null.
	Stdin func(ctx context.Context, w io.Writer) error
	// Stdout, when set, consumes the data pipe once per run. Reads mark
	// activity. Pipe back-pressure applies: a slow consumer blocks the tool.
	// Nil: stdout is drained and only marks activity.
	Stdout func(ctx context.Context, r io.Reader) error
	// Probe marks a batch version probe (§8.4 capability probing): any
	// exit code completes it (the caller judges its output) and its
	// transitions log at Debug.
	Probe bool
	// TouchOnly: stdout output is drained but marks neither readiness nor
	// activity; only Touch does (connectors: readiness is the IQ socket).
	TouchOnly bool
	// Sink, when set, also receives the events of this instance.
	Sink Sink
	// PerRun, when set, is called before every spawn: it returns the
	// arguments of this run (fresh loopback ports, §8.2 rule 3) and a side
	// channel run alongside the process until it exits.
	PerRun func() (Run, error)

	StderrRules   []Rule
	OnLine        func(Line) // every accepted stderr line (signal_info parsing)
	RingSize      int        // default 200 (§8.2 rule 5)
	UnknownPerSec int        // rate limit of unknown lines, default 10 (§8.4)

	Timeouts Timeouts
	Restart  RestartPolicy
	Limits   Limits
}

// Run is the per-spawn part of a Spec.
type Run struct {
	// Args replaces Spec.Args for this run.
	Args []string
	// Attach runs while the process runs; its ctx ends with the process.
	// touch marks readiness and activity (Instance.Touch).
	Attach func(ctx context.Context, touch func()) error
	// Done is called after the process and Attach have ended.
	Done func()
}

// Options configure the Supervisor.
type Options struct {
	RuntimeDir string // node.runtime_dir, e.g. /run/meshsdr-node
	Logger     *slog.Logger
	Sink       Sink
	Metrics    Metrics
	// HelperPath is the binary re-executed in helper mode to apply Limits.
	// Empty: os.Executable().
	HelperPath string
}

// Supervisor creates instances. One per node.
type Supervisor struct {
	opts    Options
	spawner *spawner
}

func New(opts Options) (*Supervisor, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Metrics == nil {
		opts.Metrics = noopMetrics{}
	}
	if opts.Sink == nil {
		opts.Sink = func(Event) {}
	}
	if err := PrepareRuntimeDir(opts.RuntimeDir); err != nil {
		return nil, err
	}
	return &Supervisor{opts: opts, spawner: newSpawner()}, nil
}

// Instance supervises one tool instance across restarts.
type Instance struct {
	s       *Supervisor
	spec    Spec
	log     *slog.Logger
	ring    *Ring[Line]
	unknown *bucket
	kick    chan struct{}
	cur     atomic.Pointer[run]
	dropped atomic.Int64
	argv    []string
	env     []string
	helper  string
}

var envKey = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

var reservedEnv = map[string]bool{"PATH": true, "LANG": true, "TZ": true, "HOME": true, "TMPDIR": true}

// NewInstance validates the spec. The tool's presence is not checked here: a
// missing tool surfaces as ENOENT at spawn, classified UNAVAILABLE.
func (s *Supervisor) NewInstance(spec Spec) (*Instance, error) {
	if !idPattern.MatchString(spec.ID) {
		return nil, fmt.Errorf("%w: id %q", ErrInvalidSpec, spec.ID)
	}
	if !filepath.IsAbs(spec.Path) || filepath.Clean(spec.Path) != spec.Path {
		return nil, fmt.Errorf("%w: path %q must be absolute and clean", ErrInvalidSpec, spec.Path)
	}
	if err := validArgs(spec.Args); err != nil {
		return nil, err
	}
	for _, d := range spec.ToolDirs {
		if !filepath.IsAbs(d) || strings.Contains(d, ":") {
			return nil, fmt.Errorf("%w: tool dir %q", ErrInvalidSpec, d)
		}
	}
	for _, kv := range spec.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !envKey.MatchString(k) || reservedEnv[k] || strings.ContainsAny(v, "\x00\n\r") {
			return nil, fmt.Errorf("%w: env %q", ErrInvalidSpec, k)
		}
	}
	if spec.RingSize == 0 {
		spec.RingSize = 200
	}
	if spec.UnknownPerSec == 0 {
		spec.UnknownPerSec = 10
	}
	if spec.Timeouts.Stop == 0 {
		spec.Timeouts.Stop = 3 * time.Second
	}
	if spec.Timeouts.IdleStrikes == 0 {
		spec.Timeouts.IdleStrikes = 3
	}
	in := &Instance{
		s:       s,
		spec:    spec,
		log:     s.opts.Logger.With(slog.String("instance", spec.ID), slog.String("kind", spec.Kind)),
		ring:    NewRing[Line](spec.RingSize),
		unknown: newBucket(spec.UnknownPerSec, spec.UnknownPerSec),
		kick:    make(chan struct{}, 1),
	}
	if !spec.Limits.zero() {
		in.helper = s.opts.HelperPath
		if in.helper == "" {
			exe, err := os.Executable()
			if err != nil {
				return nil, fmt.Errorf("%w: helper: %w", ErrInvalidSpec, err)
			}
			in.helper = exe
		}
	}
	in.argv = in.buildArgv(spec.Args)
	return in, nil
}

func validArgs(args []string) error {
	for _, a := range args {
		if strings.ContainsFunc(a, func(r rune) bool { return r == 0 || r == '\n' || r == '\r' }) {
			return fmt.Errorf("%w: control character in argument", ErrInvalidSpec)
		}
	}
	return nil
}

func (in *Instance) buildArgv(args []string) []string {
	argv := append([]string{in.spec.Path}, args...)
	if in.helper != "" {
		argv = helperArgv(in.helper, in.spec.Limits, in.spec.Path, argv)
	}
	return argv
}

// Workdir is the instance's private directory (exists only while Run runs).
func (in *Instance) Workdir() string {
	return filepath.Join(in.s.opts.RuntimeDir, "sessions", in.spec.ID)
}

// Kick ends a back-off or crash-loop wait immediately (admin reset, user
// re-selects the mode).
func (in *Instance) Kick() {
	select {
	case in.kick <- struct{}{}:
	default:
	}
}

// Touch marks activity from a side channel (IQ socket, stats file, status
// socket). It also counts as readiness.
func (in *Instance) Touch() {
	if r := in.cur.Load(); r != nil {
		r.activity()
	}
}

// Tail returns the last n stderr lines.
func (in *Instance) Tail(n int) []Line { return in.ring.Tail(n) }

// DroppedLines is the number of rate-limited unknown lines.
func (in *Instance) DroppedLines() int64 { return in.dropped.Load() }

func (in *Instance) buildEnv(wd string) []string {
	env := []string{
		"PATH=" + strings.Join(in.spec.ToolDirs, ":"),
		"LANG=C.UTF-8",
		"TZ=UTC",
		"HOME=" + wd,
		"TMPDIR=" + wd,
	}
	return append(env, in.spec.Env...)
}

// Run supervises the instance until ctx is cancelled (graceful stop, returns
// nil) or a terminal state is reached (returns ErrUnavailable,
// ErrDecoderError, ErrFailed or ErrJobTimeout, already emitted and logged).
// The workdir is created first and removed on return.
func (in *Instance) Run(ctx context.Context) error {
	wd, err := createWorkdir(in.s.opts.RuntimeDir, in.spec.ID)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(wd); err != nil {
			in.log.LogAttrs(context.Background(), slog.LevelWarn, "workdir cleanup failed", slog.Any("error", err))
		}
	}()
	in.env = in.buildEnv(wd)
	if in.spec.Prepare != nil {
		if err := in.spec.Prepare(wd); err != nil {
			ev := Event{State: StateUnavailable, Diag: DiagUnavailable, Reason: "prepare_failed"}
			in.emit(ev)
			return fmt.Errorf("%w: prepare: %w", ErrUnavailable, err)
		}
	}

	pol := in.spec.Restart
	attempts := 0
	var cw crashWindow
	for {
		res := in.runOnce(ctx, wd)
		ex := exitInfo(res)
		in.s.opts.Metrics.ProcessExited(in.spec.Kind, ex.Class)
		if res.stoppedBy != "ctx" {
			ex.LastErr = lineTexts(in.ring.Tail(20))
		}
		out := decide(in.spec.Mode, res, ex)
		if in.spec.Probe && errors.Is(out.err, ErrDecoderError) && res.sticky != ClassInputError {
			out = outcome{state: StateStopped, reason: "completed"}
		}
		ev := Event{PID: res.pid, State: out.state, Reason: out.reason, Diag: out.diag, Reprobe: out.reprobe, Exit: &ex}
		if !out.restart {
			in.emit(ev)
			return out.err
		}
		if res.ready && res.ran >= pol.ResetAfter {
			attempts = 0
		}
		attempts++
		if pol.MaxAttempts > 0 && attempts >= pol.MaxAttempts {
			ev.State, ev.Attempt = StateFailed, attempts
			in.emit(ev)
			return fmt.Errorf("%w: %s after %d attempts", ErrFailed, out.reason, attempts)
		}
		if cw.add(time.Now(), pol) {
			ev.State, ev.Diag, ev.Reason, ev.Delay = StateCrashLoop, DiagDecoderError, "crash_loop", pol.CrashLoopRetry
			in.emit(ev)
			if !in.wait(ctx, pol.CrashLoopRetry) {
				in.emit(Event{State: StateStopped, Reason: "stopped"})
				return nil
			}
			cw.reset()
			attempts = 0
			continue
		}
		ev.State, ev.Attempt, ev.Delay = StateRetryWait, attempts, pol.Delay(attempts)
		in.emit(ev)
		in.s.opts.Metrics.Restarted(in.spec.Kind)
		if !in.wait(ctx, ev.Delay) {
			in.emit(Event{State: StateStopped, Reason: "stopped"})
			return nil
		}
	}
}

func (in *Instance) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-in.kick:
		return true
	}
}

func (in *Instance) emit(ev Event) {
	ev.Time = time.Now()
	ev.Instance, ev.Kind = in.spec.ID, in.spec.Kind
	level := slog.LevelInfo
	switch ev.State {
	case StateStarting:
		level = slog.LevelDebug
	case StateRetryWait, StateStopping:
		level = slog.LevelWarn
	case StateCrashLoop, StateFailed, StateUnavailable, StateErrored, StateTimedOut:
		level = slog.LevelError
	}
	if ev.Diag == DiagTimeout && ev.State == StateRunning {
		level = slog.LevelWarn
	}
	if in.spec.Probe {
		level = slog.LevelDebug
	}
	attrs := []slog.Attr{slog.String("state", string(ev.State))}
	if ev.PID != 0 {
		attrs = append(attrs, slog.Int("pid", ev.PID))
	}
	if ev.Reason != "" {
		attrs = append(attrs, slog.String("reason", ev.Reason))
	}
	if ev.Diag != "" {
		attrs = append(attrs, slog.String("diag", string(ev.Diag)))
	}
	if ev.Delay > 0 {
		attrs = append(attrs, slog.Duration("delay", ev.Delay), slog.Int("attempt", ev.Attempt))
	}
	if ev.Exit != nil && ev.Exit.Class != ExitNone {
		attrs = append(attrs, slog.String("exit_class", string(ev.Exit.Class)), slog.Int("exit_code", ev.Exit.Code))
		if ev.Exit.Signal != "" {
			attrs = append(attrs, slog.String("signal", ev.Exit.Signal))
		}
	}
	in.log.LogAttrs(context.Background(), level, "supervisor transition", attrs...)
	in.s.opts.Sink(ev)
	if in.spec.Sink != nil {
		in.spec.Sink(ev)
	}
}

// run is the per-process state.
type run struct {
	in      *Instance
	lastAct atomic.Int64
	once    sync.Once
	readyCh chan struct{}
	mu      sync.Mutex
	sticky  Class
	fatal   chan Class
}

func (r *run) activity() {
	r.lastAct.Store(time.Now().UnixNano())
	r.once.Do(func() { close(r.readyCh) })
}

var stickyRank = map[Class]int{ClassResource: 1, ClassInputError: 2, ClassFatalConfig: 3}

func (r *run) line(l Line) {
	in := r.in
	if l.Class == ClassUnknown && !in.unknown.allow(l.Time) {
		in.dropped.Add(1)
		in.s.opts.Metrics.StderrLine(in.spec.Kind, l.Class, true)
		return
	}
	in.s.opts.Metrics.StderrLine(in.spec.Kind, l.Class, false)
	in.ring.Add(l)
	in.log.LogAttrs(context.Background(), slog.LevelDebug, "tool stderr", slog.String("class", string(l.Class)), slog.String("line", l.Text))
	if rank := stickyRank[l.Class]; rank > 0 {
		r.mu.Lock()
		if rank > stickyRank[r.sticky] {
			r.sticky = l.Class
		}
		r.mu.Unlock()
		if l.Class != ClassResource {
			select {
			case r.fatal <- l.Class:
			default:
			}
		}
	}
	if in.spec.OnLine != nil {
		in.spec.OnLine(l)
	}
}

type actReader struct {
	r   io.Reader
	run *run
}

func (a actReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.run.activity()
	}
	return n, err
}

type result struct {
	startErr  error
	waitErr   error
	state     *os.ProcessState
	stoppedBy string // "", ctx, start_timeout, no_tool_output, job_timeout, fatal_stderr
	ready     bool
	ran       time.Duration
	sticky    Class
	pid       int
}

func (in *Instance) runOnce(ctx context.Context, wd string) (res result) {
	var per Run
	if in.spec.PerRun != nil {
		var err error
		if per, err = in.spec.PerRun(); err == nil {
			err = validArgs(per.Args)
		}
		if err != nil {
			return result{startErr: err}
		}
		in.argv = in.buildArgv(per.Args)
		if per.Done != nil {
			defer per.Done()
		}
	}
	r := &run{in: in, readyCh: make(chan struct{}), fatal: make(chan Class, 1)}
	in.cur.Store(r)
	defer in.cur.Store(nil)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outR, outW, err := os.Pipe()
	if err != nil {
		return result{startErr: err}
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return result{startErr: err}
	}
	var inR, inW *os.File
	if in.spec.Stdin != nil {
		if inR, inW, err = os.Pipe(); err != nil {
			_ = outR.Close()
			_ = outW.Close()
			_ = errR.Close()
			_ = errW.Close()
			return result{startErr: err}
		}
	}
	cmd := &exec.Cmd{
		Path:        in.argv[0],
		Args:        in.argv,
		Env:         in.env, // never nil: nothing is inherited
		Dir:         wd,
		Stdout:      outW, // *os.File: passed as fd 1, no copy goroutine in os/exec
		Stderr:      errW,
		SysProcAttr: sysProcAttr(),
	}
	if inR != nil {
		cmd.Stdin = inR
	}
	in.emit(Event{State: StateStarting})
	err = in.s.spawner.start(cmd)
	_ = outW.Close()
	_ = errW.Close()
	if inR != nil {
		_ = inR.Close()
	}
	if err != nil {
		_ = outR.Close()
		_ = errR.Close()
		if inW != nil {
			_ = inW.Close()
		}
		return result{startErr: err}
	}
	pid := cmd.Process.Pid
	started := time.Now()
	in.s.opts.Metrics.ProcessStarted(in.spec.Kind)
	in.log.LogAttrs(ctx, slog.LevelDebug, "process spawned", slog.Int("pid", pid), slog.Any("argv", in.argv))

	var wg sync.WaitGroup
	wg.Go(func() { readLines(errR, in.spec.StderrRules, r) })
	if per.Attach != nil {
		wg.Go(func() {
			if err := per.Attach(runCtx, r.activity); err != nil && runCtx.Err() == nil {
				in.log.LogAttrs(ctx, slog.LevelDebug, "side channel ended", slog.Any("error", err))
			}
		})
	}
	wg.Go(func() {
		var ar io.Reader = actReader{r: outR, run: r}
		if in.spec.TouchOnly {
			ar = outR
		}
		if in.spec.Stdout != nil {
			if err := in.spec.Stdout(runCtx, ar); err != nil && runCtx.Err() == nil {
				in.log.LogAttrs(ctx, slog.LevelDebug, "stdout consumer ended", slog.Any("error", err))
			}
		}
		_, _ = io.Copy(io.Discard, ar) // keep draining so the tool never blocks on a dead consumer
	})
	if inW != nil {
		wg.Go(func() {
			_ = in.spec.Stdin(runCtx, inW)
			_ = inW.Close()
		})
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	res.pid = pid
	var startC, jobC, idleC <-chan time.Time
	if in.spec.Timeouts.Start > 0 {
		t := time.NewTimer(in.spec.Timeouts.Start)
		defer t.Stop()
		startC = t.C
	}
	if in.spec.Mode == Batch && in.spec.Timeouts.Job > 0 {
		t := time.NewTimer(in.spec.Timeouts.Job)
		defer t.Stop()
		jobC = t.C
	}
	if idle := in.spec.Timeouts.IdleOutput; idle > 0 {
		tk := time.NewTicker(max(idle/4, 5*time.Millisecond))
		defer tk.Stop()
		idleC = tk.C
	}
	readyC := r.readyCh
	strikes := 0
	var mark int64 // last strike time (unix nano)

loop:
	for {
		select {
		case res.waitErr = <-waitCh:
			break loop
		case <-ctx.Done():
			res.stoppedBy = "ctx"
			res.waitErr = in.terminate(pid, waitCh)
			break loop
		case <-readyC:
			readyC, startC = nil, nil
			res.ready = true
			in.emit(Event{PID: pid, State: StateRunning})
		case <-startC:
			res.stoppedBy = "start_timeout"
			res.waitErr = in.terminate(pid, waitCh)
			break loop
		case <-jobC:
			res.stoppedBy = "job_timeout"
			_ = killGroup(pid, syscall.SIGKILL) // §8.4: the process group is killed
			res.waitErr = <-waitCh
			break loop
		case <-r.fatal:
			res.stoppedBy = "fatal_stderr"
			res.waitErr = in.terminate(pid, waitCh)
			break loop
		case now := <-idleC:
			if !res.ready {
				continue
			}
			last := r.lastAct.Load()
			if last > mark {
				strikes = 0
			}
			ref := max(last, mark)
			if now.UnixNano()-ref < int64(in.spec.Timeouts.IdleOutput) {
				continue
			}
			strikes++
			mark = now.UnixNano()
			in.emit(Event{PID: pid, State: StateRunning, Diag: DiagTimeout, Reason: "no_tool_output"})
			if strikes >= in.spec.Timeouts.IdleStrikes {
				res.stoppedBy = "no_tool_output"
				res.waitErr = in.terminate(pid, waitCh)
				break loop
			}
		}
	}
	res.ran = time.Since(started)
	res.state = cmd.ProcessState
	// Grandchildren may outlive the leader: kill what is left of the group,
	// only if it still has members. The leader is reaped, so its pid could
	// be reused; probing first narrows that window to a new process that
	// both reuses the pid and leads a group of it, which only a cgroup
	// rules out (ADR 0017, A4).
	_ = killGroupIfAny(pid)
	cancel()
	in.drain(&wg, outR, errR)
	r.mu.Lock()
	res.sticky = r.sticky
	r.mu.Unlock()
	return res
}

// drain waits for the pipe readers. A process that left the group (setsid)
// could keep the pipes open forever, so after a bound the read ends are closed.
func (in *Instance) drain(wg *sync.WaitGroup, files ...*os.File) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		in.log.LogAttrs(context.Background(), slog.LevelWarn, "pipes still open after exit, closing")
		for _, f := range files {
			_ = f.Close()
		}
		<-done
	}
	for _, f := range files {
		_ = f.Close()
	}
}

// terminate sends SIGTERM to the group, waits for the grace period, then
// SIGKILL. It never holds a lock (§8.1 rule 4).
func (in *Instance) terminate(pid int, waitCh <-chan error) error {
	in.emit(Event{PID: pid, State: StateStopping})
	_ = killGroup(pid, syscall.SIGTERM)
	t := time.NewTimer(in.spec.Timeouts.Stop)
	defer t.Stop()
	select {
	case err := <-waitCh:
		return err
	case <-t.C:
		in.log.LogAttrs(context.Background(), slog.LevelWarn, "stop grace expired, sending SIGKILL", slog.Int("pid", pid))
		_ = killGroup(pid, syscall.SIGKILL)
		return <-waitCh
	}
}

func exitInfo(res result) ExitInfo {
	if res.startErr != nil {
		ex := ExitInfo{Class: ExitError, Code: -1, StartErr: res.startErr.Error()}
		switch {
		case errors.Is(res.startErr, fs.ErrNotExist):
			ex.Class = ExitNotFound
		case errors.Is(res.startErr, fs.ErrPermission):
			ex.Class = ExitPermission
		}
		return ex
	}
	ex := ExitInfo{Code: -1, Ran: res.ran}
	if res.state == nil {
		ex.Class = ExitError
		return ex
	}
	ws, _ := res.state.Sys().(syscall.WaitStatus)
	switch {
	case ws.Signaled():
		ex.Signal = ws.Signal().String()
		ours := res.stoppedBy != "" && (ws.Signal() == syscall.SIGTERM || ws.Signal() == syscall.SIGKILL)
		if ours {
			ex.Class = ExitStopped
		} else {
			ex.Class = ExitCrash
		}
	case res.stoppedBy != "":
		ex.Code = ws.ExitStatus()
		ex.Class = ExitStopped
	default:
		ex.Code = ws.ExitStatus()
		switch ex.Code {
		case 0:
			ex.Class = ExitNormal
		case 126:
			ex.Class = ExitPermission
		case 127:
			ex.Class = ExitNotFound
		default:
			ex.Class = ExitError
		}
	}
	return ex
}

type outcome struct {
	state   State
	diag    Diag
	reason  string
	restart bool
	reprobe bool
	err     error
}

// decide applies the §8.4 classification table.
func decide(mode Mode, res result, ex ExitInfo) outcome {
	switch {
	case res.stoppedBy == "ctx":
		return outcome{state: StateStopped, reason: "stopped"}
	case res.stoppedBy == "job_timeout":
		return outcome{state: StateTimedOut, diag: DiagTimeout, reason: "job_timeout", err: ErrJobTimeout}
	case ex.Class == ExitNotFound:
		return outcome{state: StateUnavailable, diag: DiagUnavailable, reason: "tool_missing", reprobe: true, err: ErrUnavailable}
	case ex.Class == ExitPermission:
		return outcome{state: StateUnavailable, diag: DiagUnavailable, reason: "permission", err: ErrUnavailable}
	case res.sticky == ClassFatalConfig:
		return outcome{state: StateUnavailable, diag: DiagUnavailable, reason: "tool_misconfigured", err: ErrUnavailable}
	case res.sticky == ClassInputError:
		return outcome{state: StateErrored, diag: DiagDecoderError, reason: "input_format", err: ErrDecoderError}
	}
	if mode == Batch {
		if ex.Class == ExitNormal {
			return outcome{state: StateStopped, reason: "completed"}
		}
		return outcome{state: StateErrored, diag: DiagDecoderError, reason: string(ex.Class), err: ErrDecoderError}
	}
	o := outcome{state: StateRetryWait, restart: true, diag: DiagDecoderError}
	switch {
	case res.stoppedBy == "start_timeout":
		o.reason, o.diag = "start_timeout", DiagNone
	case res.stoppedBy == "no_tool_output":
		o.reason, o.diag = "no_tool_output", DiagTimeout
	case res.sticky == ClassResource:
		o.reason = "resource"
	case ex.Class == ExitCrash:
		o.reason = "crash"
	case ex.Class == ExitNormal:
		o.reason = "unexpected_exit"
	default:
		o.reason = "exit_error"
	}
	return o
}

func lineTexts(ls []Line) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Text
	}
	return out
}

// ResolveTool finds a bare tool name in the configured directories (never in
// the node's own PATH, never relative).
func ResolveTool(name string, dirs []string) (string, error) {
	if name == "" || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("%w: tool name %q", ErrInvalidSpec, name)
	}
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, name)
		fi, err := os.Stat(p)
		if err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: %s", fs.ErrNotExist, name)
}
