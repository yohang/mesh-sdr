package process

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// --- connector: streaming, stop, back-pressure -------------------------------

func TestConnectorStreamsAndStopsGracefully(t *testing.T) {
	rec := newRecorder()
	sup, rt := newSup(t, rec)
	var mu sync.Mutex
	var got int64
	in, err := sup.NewInstance(Spec{
		ID: "dev-rtl0", Kind: "connector", Path: fakeConnector,
		Args:     []string{"-interval", "1ms"},
		Stdout:   countingConsumer(&got, &mu),
		Timeouts: Timeouts{Start: 2 * time.Second, Stop: time.Second},
		Restart:  DevicePolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRunning })

	wd := filepath.Join(rt, "sessions", "dev-rtl0")
	fi, err := os.Lstat(wd)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("workdir %v mode %v err %v", wd, fi.Mode(), err)
	}
	eventually(t, 2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return got >= 64*1024 })

	if err := h.stop(t); err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
	if _, err := os.Lstat(wd); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("workdir not removed: %v", err)
	}
	want := []string{"starting", "running", "stopping", "stopped(stopped)"}
	if got := rec.summary(); !slices.Equal(got, want) {
		t.Fatalf("transitions %v, want %v", got, want)
	}
	last := rec.events()[len(rec.events())-1]
	if last.Exit == nil || last.Exit.Class != ExitStopped {
		t.Fatalf("exit %+v, want stopped", last.Exit)
	}
}

func TestStdoutBackPressure(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	var written atomic.Int64
	var consumed atomic.Int64
	release := make(chan struct{})
	in, err := sup.NewInstance(Spec{
		ID: "dev-bp", Kind: "connector", Path: fakeConnector,
		Args:        []string{"-chunk", "4096", "-report"},
		StderrRules: []Rule{{Pattern: regexp.MustCompile(`^written `), Class: ClassInfo}},
		OnLine: func(l Line) {
			if strings.HasPrefix(l.Text, "written ") {
				n, _ := strconv.ParseInt(strings.TrimPrefix(l.Text, "written "), 10, 64)
				written.Store(n)
			}
		},
		Stdout: func(ctx context.Context, r io.Reader) error {
			buf := make([]byte, 4096)
			n, err := io.ReadFull(r, buf) // read one chunk, then stall
			consumed.Add(int64(n))
			if err != nil {
				return err
			}
			<-release
			return nil
		},
		Timeouts: Timeouts{Stop: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRunning })
	time.Sleep(300 * time.Millisecond) // the tool is now blocked on a full pipe
	w1 := written.Load()
	time.Sleep(200 * time.Millisecond)
	w2 := written.Load()
	if w1 != w2 {
		t.Fatalf("tool kept writing while the consumer stalled: %d -> %d", w1, w2)
	}
	// Linux default pipe capacity is 64 KiB; allow one chunk in flight.
	if lag := w2 - consumed.Load(); lag > 64*1024+4096 {
		t.Fatalf("tool ran %d bytes ahead of the consumer", lag)
	}
	t.Logf("back-pressure: tool wrote %d bytes, consumer read %d", w2, consumed.Load())
	close(release) // consumer returns: the supervisor drains, so the tool must not stay blocked
	eventually(t, 2*time.Second, func() bool { return written.Load() > w2 })
	if written.Load() == w2 {
		t.Fatal("tool still blocked after the consumer returned (supervisor must drain)")
	}
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
}

// --- restart, back-off, crash loop --------------------------------------------

func TestRestartBackoffAndMaxAttempts(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	steps := []time.Duration{30 * time.Millisecond, 60 * time.Millisecond, 120 * time.Millisecond}
	in, err := sup.NewInstance(Spec{
		ID: "dev-flaky", Kind: "connector", Path: fakeConnector,
		Args:    []string{"-interval", "1ms", "-exit-after", "20ms", "-exit-code", "1"},
		Restart: RestartPolicy{Steps: steps, MaxAttempts: 4, ResetAfter: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	err = h.wait(t, 10*time.Second)
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("Run = %v, want ErrFailed", err)
	}
	evs := rec.events()
	var waits []Event
	for i, e := range evs {
		if e.State == StateRetryWait {
			waits = append(waits, e)
			// the next starting event comes after the delay
			for _, n := range evs[i+1:] {
				if n.State == StateStarting {
					if gap := n.Time.Sub(e.Time); gap < e.Delay {
						t.Fatalf("restart after %s, before back-off %s", gap, e.Delay)
					}
					break
				}
			}
		}
	}
	if len(waits) != 3 {
		t.Fatalf("retry waits %d, want 3: %v", len(waits), rec.summary())
	}
	for i, w := range waits {
		if w.Attempt != i+1 || w.Delay != steps[i] || w.Reason != "exit_error" || w.Exit.Code != 1 {
			t.Fatalf("wait %d = attempt %d delay %s reason %s exit %+v", i, w.Attempt, w.Delay, w.Reason, w.Exit)
		}
	}
	last := evs[len(evs)-1]
	if last.State != StateFailed || last.Attempt != 4 {
		t.Fatalf("last event %+v, want failed after 4 attempts", last)
	}
}

func TestBackoffDelayAndJitter(t *testing.T) {
	p := DecoderPolicy()
	p.Jitter = 0
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i, w := range want {
		if d := p.Delay(i + 1); d != w*time.Second {
			t.Fatalf("Delay(%d) = %s, want %s", i+1, d, w*time.Second)
		}
	}
	d := DevicePolicy()
	d.Jitter = 0
	if d.Delay(1) != 2*time.Second || d.Delay(5) != time.Minute || d.Delay(50) != time.Minute {
		t.Fatal("device steps")
	}
	p.Jitter = 0.2
	p.rnd = func() float64 { return 0 }
	if got := p.Delay(1); got != 800*time.Millisecond {
		t.Fatalf("low jitter %s", got)
	}
	p.rnd = func() float64 { return 0.999999 }
	if got := p.Delay(1); got < 1199*time.Millisecond || got > 1200*time.Millisecond {
		t.Fatalf("high jitter %s", got)
	}
	p.rnd = nil
	for range 1000 {
		if got := p.Delay(3); got < 3200*time.Millisecond || got >= 4800*time.Millisecond {
			t.Fatalf("jitter out of bounds: %s", got)
		}
	}
}

func TestCrashLoopDetectionAndKick(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	in, err := sup.NewInstance(Spec{
		ID: "sess-crashy", Kind: "decoder", Path: fakeDecoder,
		Args: []string{"-exit-code", "2", "-stderr", "segfault-ish trouble"}, // stdin is /dev/null: immediate EOF, exit 2
		Restart: RestartPolicy{
			Initial: 5 * time.Millisecond, Factor: 2, Max: 20 * time.Millisecond,
			CrashLoopCount: 3, CrashLoopWindow: 10 * time.Second, CrashLoopRetry: time.Hour,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	cl := rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateCrashLoop })
	if cl.Diag != DiagDecoderError || cl.Reason != "crash_loop" || cl.Delay != time.Hour {
		t.Fatalf("crash loop event %+v", cl)
	}
	if cl.Exit == nil || cl.Exit.Code != 2 || !slices.Contains(cl.Exit.LastErr, "segfault-ish trouble") {
		t.Fatalf("crash loop evidence %+v", cl.Exit)
	}
	if n := rec.count(StateStarting); n != 3 {
		t.Fatalf("starts before crash loop = %d, want 3", n)
	}
	in.Kick() // user re-selects the mode
	rec.waitFor(t, 2*time.Second, func(e Event) bool { return e.State == StateStarting })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
}

// --- timeouts --------------------------------------------------------------------

func TestStartTimeout(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	in, err := sup.NewInstance(Spec{
		ID: "dev-hung", Kind: "connector", Path: fakeConnector,
		Args:     []string{"-hang"},
		Timeouts: Timeouts{Start: 100 * time.Millisecond, Stop: 200 * time.Millisecond},
		Restart:  RestartPolicy{Steps: []time.Duration{10 * time.Millisecond}, MaxAttempts: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	if err := h.wait(t, 5*time.Second); !errors.Is(err, ErrFailed) {
		t.Fatalf("Run = %v", err)
	}
	evs := rec.events()
	last := evs[len(evs)-1]
	if last.Reason != "start_timeout" || last.Exit.Class != ExitStopped {
		t.Fatalf("last %+v exit %+v", last, last.Exit)
	}
}

func TestIdleOutputTimeout(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	in, err := sup.NewInstance(Spec{
		ID: "sess-dream", Kind: "decoder", Path: fakeDecoder,
		Args:     []string{"-status-every", "10ms", "-silent-after", "150ms"},
		Timeouts: Timeouts{IdleOutput: 60 * time.Millisecond, IdleStrikes: 3, Stop: time.Second},
		Restart:  RestartPolicy{Steps: []time.Duration{time.Hour}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	rw := rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRetryWait })
	if rw.Reason != "no_tool_output" || rw.Diag != DiagTimeout {
		t.Fatalf("retry wait %+v", rw)
	}
	n := 0
	for _, e := range rec.events() {
		if e.State == StateRunning && e.Diag == DiagTimeout {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("TIMEOUT signals = %d, want 3: %v", n, rec.summary())
	}
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
}

func TestStopEscalatesToSIGKILL(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	in, err := sup.NewInstance(Spec{
		ID: "dev-stubborn", Kind: "connector", Path: fakeConnector,
		Args:     []string{"-interval", "5ms", "-ignore-term"},
		Timeouts: Timeouts{Stop: 300 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRunning })
	t0 := time.Now()
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(t0); el < 300*time.Millisecond || el > 3*time.Second {
		t.Fatalf("stop took %s, want grace then SIGKILL", el)
	}
	last := rec.events()[len(rec.events())-1]
	if last.Exit.Signal != "killed" || last.Exit.Class != ExitStopped {
		t.Fatalf("exit %+v", last.Exit)
	}
}

func TestBatchJobDeadline(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	in, err := sup.NewInstance(Spec{
		ID: "job-ft8", Kind: "decoder", Mode: Batch, Path: fakeDecoder,
		Args:     []string{"-hang"},
		Timeouts: Timeouts{Job: 150 * time.Millisecond},
		Limits:   Limits{Nice: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	err = start(t, in).wait(t, 5*time.Second)
	if !errors.Is(err, ErrJobTimeout) {
		t.Fatalf("Run = %v", err)
	}
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("job kill took %s", el)
	}
	last := rec.events()[len(rec.events())-1]
	if last.State != StateTimedOut || last.Diag != DiagTimeout || last.Reason != "job_timeout" {
		t.Fatalf("last %+v", last)
	}
}

func TestBatchJobCompletes(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	buf := NewDropOldest(1 << 20)
	var mu sync.Mutex
	var decodes []string
	in, err := sup.NewInstance(Spec{
		ID: "job-ok", Kind: "decoder", Mode: Batch, Path: fakeDecoder,
		Args:  []string{"-frame", "8", "-exit-code", "0"},
		Stdin: buf.Feed,
		Stdout: func(ctx context.Context, r io.Reader) error {
			sc := bufio.NewScanner(r)
			for sc.Scan() {
				mu.Lock()
				decodes = append(decodes, sc.Text())
				mu.Unlock()
			}
			return sc.Err()
		},
		Timeouts: Timeouts{Job: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		_, _ = buf.Write(bytes.Repeat([]byte{byte(i)}, 8))
	}
	buf.Close()
	if err := start(t, in).wait(t, 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(decodes) != 3 || decodes[2] != "DECODE n=3 first=2" {
		t.Fatalf("decodes %v", decodes)
	}
	if last := rec.events()[len(rec.events())-1]; last.Reason != "completed" {
		t.Fatalf("last %+v", last)
	}
}

// --- process group and parent death ------------------------------------------------

func TestProcessGroupKill(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want error
	}{
		{"graceful stop kills grandchildren", []string{"-interval", "5ms", "-spawn-child"}, nil},
		{"leader exit kills grandchildren", []string{"-interval", "5ms", "-spawn-child", "-exit-after", "100ms"}, ErrFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder()
			sup, _ := newSup(t, rec)
			var lines lineLog
			in, err := sup.NewInstance(Spec{
				ID: "dev-tree", Kind: "connector", Path: fakeConnector, Args: tc.args,
				OnLine:   lines.add,
				Timeouts: Timeouts{Stop: 200 * time.Millisecond},
				Restart:  RestartPolicy{MaxAttempts: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			h := start(t, in)
			running := rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRunning })
			var child int
			eventually(t, 2*time.Second, func() bool {
				l, ok := lines.find("child ")
				if ok {
					child = atoiSuffix(t, l.Text)
				}
				return ok
			})
			if child == 0 {
				t.Fatal("grandchild pid not reported")
			}
			var runErr error
			if tc.want == nil {
				runErr = h.stop(t)
			} else {
				runErr = h.wait(t, 5*time.Second)
			}
			if !errors.Is(runErr, tc.want) && runErr != tc.want {
				t.Fatalf("Run = %v, want %v", runErr, tc.want)
			}
			if !eventually(t, 2*time.Second, func() bool { return processGone(running.PID) && processGone(child) }) {
				t.Fatalf("leader %d gone=%v, grandchild %d gone=%v", running.PID, processGone(running.PID), child, processGone(child))
			}
		})
	}
}

func TestPdeathsigKillsToolWhenNodeDies(t *testing.T) {
	rt := filepath.Join(t.TempDir(), "run")
	node := exec.Command(os.Args[0], "-test.run=^$")
	node.Env = append(os.Environ(), "PROCESS_TEST_FAKE_NODE=1", "PROCESS_TEST_RUNTIME="+rt, "PROCESS_TEST_TOOL="+fakeConnector)
	out, err := node.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	var pid, child int
	sc := bufio.NewScanner(out)
	deadline := time.AfterFunc(10*time.Second, func() { _ = node.Process.Kill() })
	for (pid == 0 || child == 0) && sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			n, _ := strconv.Atoi(f[1])
			switch f[0] {
			case "pid":
				pid = n
			case "child":
				child = n
			}
		}
	}
	deadline.Stop()
	if pid == 0 || child == 0 {
		t.Fatalf("fake node did not report pids (pid=%d child=%d)", pid, child)
	}
	// SIGKILL the node: no cleanup code runs.
	_ = node.Process.Kill()
	_ = node.Wait()
	if !eventually(t, 3*time.Second, func() bool { return processGone(pid) }) {
		t.Fatalf("connector %d survived the node's SIGKILL (Pdeathsig)", pid)
	}
	// Known gap: Pdeathsig is per process, the grandchild only had the
	// connector as parent. It survives a hard node kill.
	survived := !processGone(child)
	_ = syscall.Kill(child, syscall.SIGKILL)
	t.Logf("grandchild survived node SIGKILL: %v (expected true: Pdeathsig does not cover grandchildren)", survived)
	if !survived {
		t.Log("grandchild died too (environment-dependent, e.g. PID namespace teardown)")
	}
	// The workdir of the killed node is a leftover: the startup sweep removes it.
	n, err := SweepSessions(rt)
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1 leftover", n, err)
	}
}

// --- stderr classification --------------------------------------------------------

var decoderRules = []Rule{
	{regexp.MustCompile(`^banner`), ClassInfo},
	{regexp.MustCompile(`^sync `), ClassSignalInfo},
	{regexp.MustCompile(`^warning:`), ClassWarn},
	{regexp.MustCompile(`invalid option`), ClassFatalConfig},
	{regexp.MustCompile(`unexpected sample format`), ClassInputError},
	{regexp.MustCompile(`codecserver unreachable`), ClassResource},
}

func TestStderrClassification(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	var lines lineLog
	in, err := sup.NewInstance(Spec{
		ID: "sess-m17", Kind: "decoder", Path: fakeDecoder,
		Args:        []string{"-status-every", "1s", "-stderr", "banner v1.2|sync lsf=ok level=-12|warning: clock drift|weird \x1b[31mred\x1b[0m|something else"},
		StderrRules: decoderRules,
		OnLine:      lines.add,
		Timeouts:    Timeouts{Stop: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	eventually(t, 3*time.Second, func() bool { _, ok := lines.find("something else"); return ok })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	want := map[string]Class{
		"banner v1.2": ClassInfo, "sync lsf=ok level=-12": ClassSignalInfo, "warning: clock drift": ClassWarn,
		"weird [31mred[0m": ClassUnknown, "something else": ClassUnknown,
	}
	for text, class := range want {
		l, ok := lines.find(text)
		if !ok || l.Class != class || l.Text != text {
			t.Fatalf("line %q: got %+v ok=%v, want class %s", text, l, ok, class)
		}
	}
	if tail := in.Tail(20); len(tail) != 5 {
		t.Fatalf("ring tail = %d lines", len(tail))
	}
}

func TestStderrTerminalClasses(t *testing.T) {
	for _, tc := range []struct {
		line   string
		code   string
		err    error
		state  State
		diag   Diag
		reason string
	}{
		{"error: invalid option --foo", "1", ErrUnavailable, StateUnavailable, DiagUnavailable, "tool_misconfigured"},
		{"unexpected sample format s8", "1", ErrDecoderError, StateErrored, DiagDecoderError, "input_format"},
		{"codecserver unreachable", "1", nil, StateRetryWait, DiagDecoderError, "resource"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			rec := newRecorder()
			sup, _ := newSup(t, rec)
			in, err := sup.NewInstance(Spec{
				ID: "sess-term", Kind: "decoder", Path: fakeDecoder,
				// -status-every keeps the tool alive: the supervisor must stop it itself on fatal classes.
				Args:        []string{"-stderr", tc.line, "-status-every", "1s", "-exit-after", "300ms", "-exit-code", tc.code},
				StderrRules: decoderRules,
				Timeouts:    Timeouts{Stop: time.Second},
				Restart:     RestartPolicy{Steps: []time.Duration{time.Hour}},
			})
			if err != nil {
				t.Fatal(err)
			}
			h := start(t, in)
			if tc.err != nil {
				if err := h.wait(t, 5*time.Second); !errors.Is(err, tc.err) {
					t.Fatalf("Run = %v, want %v", err, tc.err)
				}
				if n := rec.count(StateStarting); n != 1 {
					t.Fatalf("restarted %d times, want no restart", n-1)
				}
			}
			e := rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == tc.state })
			if e.Diag != tc.diag || e.Reason != tc.reason || !slices.Contains(e.Exit.LastErr, tc.line) {
				t.Fatalf("event %+v exit %+v", e, e.Exit)
			}
		})
	}
}

func TestStderrRateLimitAndLineCap(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	var lines lineLog
	in, err := sup.NewInstance(Spec{
		ID: "sess-noisy", Kind: "decoder", Path: fakeDecoder,
		Args:          []string{"-status-every", "1s", "-flood", "500", "-long-line", "10000", "-stderr", "banner"},
		StderrRules:   []Rule{{regexp.MustCompile(`^(banner|after long line|x)`), ClassInfo}},
		OnLine:        lines.add,
		UnknownPerSec: 10,
		Timeouts:      Timeouts{Stop: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	eventually(t, 3*time.Second, func() bool { _, ok := lines.find("after long line"); return ok })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	noise := lines.all("noise line ")
	if len(noise) > 15 || in.DroppedLines() < 480 {
		t.Fatalf("unknown lines accepted %d, dropped %d", len(noise), in.DroppedLines())
	}
	long, ok := lines.find("xxx")
	if !ok || !long.Truncated || len(long.Text) != maxLineBytes {
		t.Fatalf("long line: truncated=%v len=%d", long.Truncated, len(long.Text))
	}
	if _, ok := lines.find("after long line"); !ok {
		t.Fatal("line after the truncated one was lost")
	}
}

// --- exit codes ----------------------------------------------------------------------

func TestExitCodeMapping(t *testing.T) {
	noexec := filepath.Join(t.TempDir(), "noexec")
	if err := os.WriteFile(noexec, []byte("#!/bin/false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		path    string
		args    []string
		limits  Limits
		class   ExitClass
		state   State
		reason  string
		reprobe bool
	}{
		{"enoent", "/nonexistent/jt9", nil, Limits{}, ExitNotFound, StateUnavailable, "tool_missing", true},
		{"enoent via helper", "/nonexistent/jt9", nil, Limits{NoNewPrivs: true}, ExitNotFound, StateUnavailable, "tool_missing", true},
		{"eacces", noexec, nil, Limits{}, ExitPermission, StateUnavailable, "permission", false},
		{"exit 127", fakeDecoder, []string{"-exit-code", "127"}, Limits{}, ExitNotFound, StateUnavailable, "tool_missing", true},
		{"exit 126", fakeDecoder, []string{"-exit-code", "126"}, Limits{}, ExitPermission, StateUnavailable, "permission", false},
		{"crash", fakeConnector, []string{"-interval", "5ms", "-crash-after", "50ms"}, Limits{}, ExitCrash, StateFailed, "crash", false},
		{"unexpected exit 0", fakeDecoder, []string{"-exit-code", "0"}, Limits{}, ExitNormal, StateFailed, "unexpected_exit", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder()
			sup, _ := newSup(t, rec)
			in, err := sup.NewInstance(Spec{
				ID: "sess-exit", Kind: "decoder", Path: tc.path, Args: tc.args, Limits: tc.limits,
				Restart: RestartPolicy{MaxAttempts: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = start(t, in).wait(t, 5*time.Second)
			evs := rec.events()
			last := evs[len(evs)-1]
			if last.State != tc.state || last.Reason != tc.reason || last.Exit.Class != tc.class || last.Reprobe != tc.reprobe {
				t.Fatalf("last = %s/%s reprobe=%v exit %+v; events %v", last.State, last.Reason, last.Reprobe, last.Exit, rec.summary())
			}
			if tc.class == ExitCrash && last.Exit.Signal != "aborted" {
				t.Fatalf("crash signal %q", last.Exit.Signal)
			}
		})
	}
}

// --- isolation: env, workdir, limits -------------------------------------------------

func TestEnvironmentScrubbedAndWorkdirIsCwd(t *testing.T) {
	t.Setenv("PROCESS_TEST_SECRET", "s3cret")
	rec := newRecorder()
	sup, rt := newSup(t, rec)
	var lines lineLog
	in, err := sup.NewInstance(Spec{
		ID: "sess-env", Kind: "decoder", Path: fakeConnector,
		Args:     []string{"-dump-env", "-interval", "10ms"},
		ToolDirs: []string{"/opt/meshsdr/tools", "/usr/bin"},
		Env:      []string{"DIREWOLF_X=1"},
		OnLine:   lines.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	running := rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRunning })
	eventually(t, 2*time.Second, func() bool { _, ok := lines.find("pgid "); return ok })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	wd := filepath.Join(rt, "sessions", "sess-env")
	env := lines.all("env ")
	slices.Sort(env)
	want := []string{"DIREWOLF_X=1", "HOME=" + wd, "LANG=C.UTF-8", "PATH=/opt/meshsdr/tools:/usr/bin", "TMPDIR=" + wd, "TZ=UTC"}
	if !slices.Equal(env, want) {
		t.Fatalf("child env %v, want %v", env, want)
	}
	if cwd := lines.all("cwd "); len(cwd) != 1 || cwd[0] != wd {
		t.Fatalf("cwd %v", cwd)
	}
	pg := lines.all("pgid ")
	if len(pg) != 1 || pg[0] != strconv.Itoa(running.PID)+" pid "+strconv.Itoa(running.PID) {
		t.Fatalf("tool is not its own group leader: %v (pid %d)", pg, running.PID)
	}
}

func TestSpecValidation(t *testing.T) {
	sup, _ := newSup(t, newRecorder())
	bad := []Spec{
		{ID: "../x", Path: fakeDecoder},
		{ID: "", Path: fakeDecoder},
		{ID: "ok", Path: "fakedecoder"},
		{ID: "ok", Path: "/usr/bin/../bin/x"},
		{ID: "ok", Path: fakeDecoder, Args: []string{"a\nb"}},
		{ID: "ok", Path: fakeDecoder, Env: []string{"PATH=/tmp"}},
		{ID: "ok", Path: fakeDecoder, Env: []string{"lower=1"}},
		{ID: "ok", Path: fakeDecoder, ToolDirs: []string{"rel/dir"}},
	}
	for _, s := range bad {
		if _, err := sup.NewInstance(s); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("spec %+v accepted (err %v)", s, err)
		}
	}
	if p, err := ResolveTool("fakedecoder", []string{"/nonexistent", filepath.Dir(fakeDecoder)}); err != nil || p != fakeDecoder {
		t.Fatalf("ResolveTool = %q, %v", p, err)
	}
	if _, err := ResolveTool("../fakedecoder", []string{filepath.Dir(fakeDecoder)}); err == nil {
		t.Fatal("ResolveTool accepted a path")
	}
}

func TestWorkdirSafety(t *testing.T) {
	rec := newRecorder()
	sup, rt := newSup(t, rec)
	spec := Spec{ID: "sess-dup", Kind: "decoder", Path: fakeDecoder, Args: []string{"-status-every", "1s"}}
	a, _ := sup.NewInstance(spec)
	b, _ := sup.NewInstance(spec)
	h := start(t, a)
	rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateStarting })
	if err := b.Run(context.Background()); !errors.Is(err, ErrWorkdir) {
		t.Fatalf("second instance with the same id: %v", err)
	}
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}

	// Startup sweep.
	for _, d := range []string{"old1", "old2/nested"} {
		if err := os.MkdirAll(filepath.Join(rt, "sessions", d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := SweepSessions(rt); err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v", n, err)
	}

	// Runtime dir checks.
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "open"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(base, "open"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRuntimeDir(filepath.Join(base, "open")); !errors.Is(err, ErrWorkdir) {
		t.Fatalf("0755 runtime dir accepted: %v", err)
	}
	if err := os.Symlink(rt, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRuntimeDir(filepath.Join(base, "link")); !errors.Is(err, ErrWorkdir) {
		t.Fatalf("symlinked runtime dir accepted: %v", err)
	}

	// Exclusive, no-follow file creation inside a workdir.
	wd := t.TempDir()
	f, err := CreateFile(wd, "direwolf.conf", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if fi, _ := os.Stat(filepath.Join(wd, "direwolf.conf")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if _, err := CreateFile(wd, "direwolf.conf", 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("re-create: %v", err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(wd, "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateFile(wd, "evil", 0o600); err == nil {
		t.Fatal("created through a symlink")
	}
	for _, name := range []string{"../x", "a/b", ".hidden", ""} {
		if _, err := CreateFile(wd, name, 0o600); !errors.Is(err, ErrWorkdir) {
			t.Fatalf("name %q accepted: %v", name, err)
		}
	}
}

func TestLimitsViaExecHelper(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	var lines lineLog
	core := uint64(0)
	in, err := sup.NewInstance(Spec{
		ID: "sess-limits", Kind: "decoder", Path: fakeDecoder,
		Args:   []string{"-dump-limits", "-status-every", "1s"},
		OnLine: lines.add,
		Limits: Limits{AddressSpace: 1 << 30, OpenFiles: 64, Core: &core, Nice: 5, NoNewPrivs: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	running := rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRunning || e.State == StateRetryWait })
	eventually(t, 2*time.Second, func() bool { _, ok := lines.find("limit nice"); return ok })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	if running.State != StateRunning {
		t.Fatalf("tool did not start under limits: %v; stderr %v", rec.summary(), lineTexts(in.Tail(20)))
	}
	got := strings.Join(lines.all("limit "), "\n")
	for _, w := range []string{"Max address space 1073741824 1073741824 bytes", "Max open files 64 64 files", "NoNewPrivs: 1", "nice 5"} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
}

// TestGoToolUnder512MiBAddressSpace records how a Go tool behaves under the
// §8.4 default RLIMIT_AS of 512 MiB (it is an experiment, not a contract).
func TestGoToolUnder512MiBAddressSpace(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	in, err := sup.NewInstance(Spec{
		ID: "sess-as512", Kind: "decoder", Path: fakeDecoder,
		Args:    []string{"-status-every", "10ms"},
		Limits:  Limits{AddressSpace: 512 << 20},
		Restart: RestartPolicy{MaxAttempts: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	e := rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRunning || e.State == StateFailed })
	t.Logf("Go tool under RLIMIT_AS=512MiB: %s %v", e.State, lineTexts(in.Tail(3)))
	_ = h.stop(t)
}

// TestUnprivilegedNetworkNamespace probes whether a tool could run without
// network (CLONE_NEWUSER|CLONE_NEWNET) in this environment. Experiment only.
func TestUnprivilegedNetworkNamespace(t *testing.T) {
	cmd := exec.Command(fakeDecoder, "-exit-code", "0")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}},
	}
	err := cmd.Run()
	t.Logf("unprivileged user+net namespace: err=%v", err)
	if err != nil {
		t.Skip("not available here (expected under Docker's default seccomp profile)")
	}
}

// --- stdin buffer -----------------------------------------------------------------------

func TestDropOldestBuffer(t *testing.T) {
	b := NewDropOldest(10)
	for _, s := range []string{"aaaa", "bbbb", "cccc"} {
		_, _ = b.Write([]byte(s))
	}
	if n, g := b.Overruns(); n != 4 || g != 1 {
		t.Fatalf("overruns = %d bytes, %d gaps", n, g)
	}
	b.Close()
	var out bytes.Buffer
	if err := b.Feed(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "bbbbcccc" {
		t.Fatalf("fed %q", out.String())
	}
}

func TestStreamingDecoderWithStdinFeed(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	buf := NewDropOldest(64 * 1024)
	var n atomic.Int64
	in, err := sup.NewInstance(Spec{
		ID: "sess-aprs", Kind: "decoder", Path: fakeDecoder,
		Args:  []string{"-frame", "16"},
		Stdin: buf.Feed,
		Stdout: func(ctx context.Context, r io.Reader) error {
			sc := bufio.NewScanner(r)
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "DECODE ") {
					n.Add(1)
				}
			}
			return sc.Err()
		},
		Timeouts: Timeouts{Stop: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	for range 10 {
		_, _ = buf.Write(make([]byte, 16))
	}
	if !eventually(t, 3*time.Second, func() bool { return n.Load() == 10 }) {
		t.Fatalf("decodes = %d, want 10", n.Load())
	}
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
}

// BenchmarkSpawn compares a direct spawn with a spawn through the exec helper
// (limits applied between fork and exec): one batch job per iteration.
func BenchmarkSpawn(b *testing.B) {
	for _, tc := range []struct {
		name   string
		limits Limits
	}{{"direct", Limits{}}, {"helper", Limits{NoNewPrivs: true, OpenFiles: 256}}} {
		b.Run(tc.name, func(b *testing.B) {
			self, _ := filepath.Abs(os.Args[0])
			sup, err := New(Options{RuntimeDir: filepath.Join(b.TempDir(), "run"), HelperPath: self})
			if err != nil {
				b.Fatal(err)
			}
			in, err := sup.NewInstance(Spec{ID: "bench", Kind: "decoder", Mode: Batch, Path: fakeDecoder,
				Args: []string{"-exit-code", "0"}, Limits: tc.limits})
			if err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				if err := in.Run(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestPerRunArgsTouchOnlyAndAttach(t *testing.T) {
	rec := newRecorder()
	sup, _ := newSup(t, rec)
	var mu sync.Mutex
	runs, done := 0, 0
	in, err := sup.NewInstance(Spec{
		ID: "dev-per-run", Kind: "connector", Path: fakeConnector,
		TouchOnly: true,
		PerRun: func() (Run, error) {
			mu.Lock()
			runs++
			n := runs
			mu.Unlock()
			args := []string{"-interval", "1ms"}
			if n == 1 {
				args = append(args, "-exit-after", "300ms")
			}
			return Run{
				Args: args,
				Attach: func(ctx context.Context, touch func()) error {
					select {
					case <-time.After(150 * time.Millisecond):
						touch()
					case <-ctx.Done():
					}
					<-ctx.Done()
					return ctx.Err()
				},
				Done: func() { mu.Lock(); done++; mu.Unlock() },
			}, nil
		},
		Timeouts: Timeouts{Start: 2 * time.Second, Stop: time.Second},
		Restart:  RestartPolicy{Steps: []time.Duration{10 * time.Millisecond}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, in)
	began := time.Now()
	rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRunning })
	// Stdout bytes flow from the first millisecond: readiness waits for touch.
	if time.Since(began) < 100*time.Millisecond {
		t.Fatal("stdout marked readiness despite TouchOnly")
	}
	// The first run exits, the second one gets new args and runs on.
	rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRetryWait })
	rec.waitFor(t, 5*time.Second, func(e Event) bool { return e.State == StateRunning })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if runs != 2 || done != 2 {
		t.Fatalf("runs %d, done %d", runs, done)
	}
}
