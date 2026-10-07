package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var fakeConnector, fakeDecoder string

func TestMain(m *testing.M) {
	MaybeRunExecHelper()
	if os.Getenv("PROCESS_TEST_FAKE_NODE") == "1" {
		fakeNode()
		return
	}
	dir, err := os.MkdirTemp("", "process-tools-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeConnector = filepath.Join(dir, "fakeconnector")
	fakeDecoder = filepath.Join(dir, "fakedecoder")
	for out, pkg := range map[string]string{fakeConnector: "./testdata/fakeconnector", fakeDecoder: "./testdata/fakedecoder"} {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "build fakes:", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fakeNode is the test binary re-executed as a "node" that supervises one
// connector, so that the test can SIGKILL the node and check Pdeathsig.
func fakeNode() {
	var mu sync.Mutex
	say := func(s string) { mu.Lock(); fmt.Println(s); mu.Unlock() }
	sup, err := New(Options{
		RuntimeDir: os.Getenv("PROCESS_TEST_RUNTIME"),
		Sink: func(e Event) {
			if e.State == StateRunning && e.PID != 0 {
				say(fmt.Sprintf("pid %d", e.PID))
			}
		},
	})
	if err != nil {
		panic(err)
	}
	in, err := sup.NewInstance(Spec{
		ID: "node-conn", Kind: "connector", Path: os.Getenv("PROCESS_TEST_TOOL"),
		Args: []string{"-interval", "5ms", "-spawn-child"},
		OnLine: func(l Line) {
			if strings.HasPrefix(l.Text, "child ") {
				say(l.Text)
			}
		},
		Restart: RestartPolicy{Steps: []time.Duration{time.Hour}},
	})
	if err != nil {
		panic(err)
	}
	_ = in.Run(context.Background())
}

// --- helpers ---------------------------------------------------------------

type recorder struct {
	mu  sync.Mutex
	evs []Event
	ch  chan Event
}

func newRecorder() *recorder { return &recorder{ch: make(chan Event, 4096)} }

func (r *recorder) sink(e Event) {
	r.mu.Lock()
	r.evs = append(r.evs, e)
	r.mu.Unlock()
	select {
	case r.ch <- e:
	default:
	}
}

func (r *recorder) events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.evs)
}

func (r *recorder) waitFor(t *testing.T, d time.Duration, pred func(Event) bool) Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-r.ch:
			if pred(e) {
				return e
			}
		case <-deadline:
			t.Fatalf("event not seen within %s; events: %v", d, r.summary())
			return Event{}
		}
	}
}

func (r *recorder) summary() []string {
	var out []string
	for _, e := range r.events() {
		s := string(e.State)
		if e.Reason != "" {
			s += "(" + e.Reason + ")"
		}
		if e.Diag != "" {
			s += "[" + string(e.Diag) + "]"
		}
		out = append(out, s)
	}
	return out
}

func (r *recorder) count(st State) int {
	n := 0
	for _, e := range r.events() {
		if e.State == st {
			n++
		}
	}
	return n
}

func logger() *slog.Logger {
	if os.Getenv("PROCESS_TEST_LOG") == "1" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).
			With(slog.String("component", "radio.infra.process"))
	}
	return slog.New(slog.DiscardHandler)
}

func newSup(t *testing.T, rec *recorder) (*Supervisor, string) {
	t.Helper()
	rt := filepath.Join(t.TempDir(), "run")
	self, _ := filepath.Abs(os.Args[0])
	sup, err := New(Options{RuntimeDir: rt, Logger: logger(), Sink: rec.sink, HelperPath: self})
	if err != nil {
		t.Fatal(err)
	}
	return sup, rt
}

type runHandle struct {
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, in *Instance) *runHandle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &runHandle{cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- in.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-h.done })
	return h
}

func (h *runHandle) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-h.done:
		h.done <- err // keep it for Cleanup
		return err
	case <-time.After(d):
		t.Fatalf("Run did not return within %s", d)
		return nil
	}
}

func (h *runHandle) stop(t *testing.T) error {
	t.Helper()
	h.cancel()
	return h.wait(t, 10*time.Second)
}

// processGone reports whether pid no longer runs (absent or zombie).
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i > 0 && i+2 < len(s) && s[i+2] == 'Z'
}

func eventually(t *testing.T, d time.Duration, f func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f()
}

type lineLog struct {
	mu    sync.Mutex
	lines []Line
}

func (l *lineLog) add(x Line) { l.mu.Lock(); l.lines = append(l.lines, x); l.mu.Unlock() }

func (l *lineLog) find(prefix string) (Line, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.lines {
		if strings.HasPrefix(x.Text, prefix) {
			return x, true
		}
	}
	return Line{}, false
}

func (l *lineLog) all(prefix string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, x := range l.lines {
		if strings.HasPrefix(x.Text, prefix) {
			out = append(out, strings.TrimPrefix(x.Text, prefix))
		}
	}
	return out
}

func countingConsumer(n *int64, mu *sync.Mutex) func(context.Context, io.Reader) error {
	return func(ctx context.Context, r io.Reader) error {
		buf := make([]byte, 32*1024)
		for {
			k, err := r.Read(buf)
			mu.Lock()
			*n += int64(k)
			mu.Unlock()
			if err != nil {
				return err
			}
		}
	}
}

func atoiSuffix(t *testing.T, s string) int {
	t.Helper()
	f := strings.Fields(s)
	n, err := strconv.Atoi(f[len(f)-1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}
