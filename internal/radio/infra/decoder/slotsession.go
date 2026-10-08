package decoder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// Slot session timings.
const (
	// slotInput is the audio a slot session buffers between the DSP tap
	// and its recorder (blocks, about 20 ms each): file writes never run
	// on the DSP goroutine (§8.3 rule 2).
	slotInput = 256
	// resyncAfter: the audio clock counts the samples from the sample
	// timestamps, and is set again from them after a gap or when they move
	// by more than this (the connector stamps blocks on arrival, which
	// jitters under load; WSJT decoders tolerate about 1 s of DT).
	resyncAfter = 500 * time.Millisecond
)

// slotChunk is converted audio for the recorder.
type slotChunk struct {
	t   time.Time
	pcm []byte
}

// slotSession is a batch decoder session (WSJT family, JS8): the audio is
// cut into slot files in the session workdir and each slot is decoded by
// a job of the node queue per profile (DEC-025, DEC-026).
type slotSession struct {
	r    *Runner
	spec app.DecoderSpec
	ev   app.DecoderEvents
	dir  string
	log  *slog.Logger
	in   chan slotChunk
	jobs atomic.Int64
	// lost counts the audio blocks dropped on a full input buffer.
	lost atomic.Int64
	// resyncs counts the audio clock resets on the sample timestamps.
	resyncs atomic.Int64

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	conv   *dsp.S16Converter
	next   time.Time
	closed bool
	last   app.DecoderStatus
	// users counts the users of the workdir: the recorder and the jobs
	// running; the last one removes it.
	users int
}

// startSlots starts a slot decoder session in its own workdir.
func (r *Runner) startSlots(spec app.DecoderSpec, ev app.DecoderEvents) (app.DecoderRun, error) {
	if r.o.Queue == nil {
		return nil, errors.New("no batch decoder queue")
	}

	dir, err := r.o.Supervisor.CreateWorkdir("dec-" + spec.Session.String())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &slotSession{
		r: r, spec: spec, ev: ev, dir: dir, in: make(chan slotChunk, slotInput), ctx: ctx, cancel: cancel, users: 1,
		log: r.o.Logger.With(slog.String("session_id", spec.Session.String()), slog.String("mode", spec.Mode.Name)),
	}

	go s.loop()

	return s, nil
}

// settings returns the decoding settings pushed by the hub.
func (s *slotSession) settings() Settings {
	if s.r.o.Settings == nil {
		return Settings{}
	}

	return s.r.o.Settings()
}

// periods returns the distinct slot periods of the session's profiles.
func (s *slotSession) periods() []time.Duration {
	var out []time.Duration

	for _, p := range profiles(s.spec.Mode.Name, s.settings()) {
		if !slices.Contains(out, p.period) {
			out = append(out, p.period)
		}
	}

	return out
}

// Audio implements app.DecoderRun: the audio is converted to 12 kHz s16le,
// stamped from the sample timestamps and handed to the recorder (never
// blocking: a full buffer leaves a hole in the slot).
func (s *slotSession) Audio(b app.AudioBlock) {
	s.mu.Lock()

	if s.closed || b.Rate <= 0 {
		s.mu.Unlock()

		return
	}

	if s.conv == nil || s.conv.InRate() != b.Rate {
		if s.conv != nil {
			s.conv.Close()
		}

		c, err := dsp.NewS16Converter(b.Rate, slotRate)
		if err != nil {
			s.conv = nil
			s.mu.Unlock()
			s.log.Warn("decoder input not converted", slog.Int("rate", b.Rate), slog.Any("error", err))

			return
		}

		s.conv = c
	}

	out, err := s.conv.Convert(b.Samples)
	if err != nil || len(out) < 2 {
		s.mu.Unlock()

		if err != nil {
			s.log.Warn("decoder input not converted", slog.Any("error", err))
		}

		return
	}

	t := s.next
	if b.Discontinuity || t.IsZero() || b.Time.Sub(t).Abs() > resyncAfter {
		if !t.IsZero() {
			s.resyncs.Add(1)
		}

		t = b.Time
	}

	s.next = t.Add(durationOf(len(out) / 2))
	c := slotChunk{t: t, pcm: bytes.Clone(out)}
	s.mu.Unlock()

	select {
	case s.in <- c:
	default:
		s.lost.Add(1)
	}
}

// IQ implements app.DecoderRun: slot decoders read audio only.
func (s *slotSession) IQ(app.IQBlock) {}

// Retune implements app.DecoderRun: slot decoders have no secondary
// selector; each slot keeps the dial of its start.
func (s *slotSession) Retune(float64) {}

// SpectrumSize implements app.DecoderRun: no secondary FFT.
func (s *slotSession) SpectrumSize() int { return 0 }

// Spectrum implements app.DecoderRun: no secondary FFT.
func (s *slotSession) Spectrum(int) {}

// loop records the audio until the session closes. The session is running
// from its first audio (never from Start: the caller may hold the lock its
// status callback takes).
func (s *slotSession) loop() {
	rec := newRecorder(s.dir, s.ev.Dial, s.log)
	periods := s.periods()
	first := true

	defer func() {
		rec.close()
		s.release()
	}()

	for {
		select {
		case <-s.ctx.Done():
			return
		case c := <-s.in:
			if first {
				first = false
				s.status(app.DecoderStatus{State: app.DecoderRunning})
			}

			done := rec.write(c.t, c.pcm, periods)
			for _, f := range done {
				s.slotDone(f)
			}

			if len(done) > 0 {
				// A settings change applies from the next slot (DEC-021).
				periods = s.periods()
				s.status(app.DecoderStatus{State: app.DecoderRunning})
			}
		}
	}
}

// slotDone queues one job per profile of the slot's period; the slot file
// goes with the last one.
func (s *slotSession) slotDone(f *slotFile) {
	var ps []profile

	for _, p := range profiles(s.spec.Mode.Name, s.settings()) {
		if p.period == f.period {
			ps = append(ps, p)
		}
	}

	if len(ps) == 0 || f.real < samplesIn(SlotGuard) || s.ctx.Err() != nil {
		f.remove(s.log)

		return
	}

	partial := f.partial()
	if partial {
		s.log.Debug("partial slot", slog.Time("slot", f.start), slog.Int64("samples", f.real), slog.Int64("full", f.full()),
			slog.Int64("lost_blocks", s.lost.Load()), slog.Int64("resyncs", s.resyncs.Load()))
	}

	f.refs.Store(int32(len(ps)))

	for _, p := range ps {
		s.r.o.Queue.Put(Job{
			Owner:    s,
			Deadline: f.start.Add(f.period).Add(p.job),
			Run:      func(ctx context.Context, budget time.Duration) { s.runJob(ctx, budget, p, f, partial) },
			Drop: func(reason string) {
				f.release(s.log)
				s.status(app.DecoderStatus{State: app.DecoderError, Reason: reason})
			},
		})
	}
}

// acquire takes the workdir for a job; false once the session closed.
func (s *slotSession) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.users == 0 {
		return false
	}

	s.users++

	return true
}

// release gives the workdir back; the last user removes it.
func (s *slotSession) release() {
	s.mu.Lock()
	s.users--
	last := s.users == 0
	s.mu.Unlock()

	if last {
		if err := os.RemoveAll(s.dir); err != nil {
			s.log.Warn("decoder workdir not removed", slog.Any("error", err))
		}
	}
}

// runJob decodes one slot with one profile under the supervisor, in the
// session workdir (wsprd keeps its hash table there across slots, §8.4
// batch rule 3), within budget.
func (s *slotSession) runJob(ctx context.Context, budget time.Duration, p profile, f *slotFile, partial bool) {
	defer f.release(s.log)

	if !s.acquire() {
		return
	}
	defer s.release()

	jctx, stop := context.WithCancel(s.ctx)
	defer stop()

	unhook := context.AfterFunc(ctx, stop)
	defer unhook()

	path, err := s.r.o.Tools.Resolve(p.tool)
	if err != nil {
		s.log.Error("slot decoder not run", slog.Any("error", err))
		s.status(app.DecoderStatus{State: app.DecoderUnavailable, Reason: "tool_missing"})

		if s.r.o.Reprobe != nil {
			s.r.o.Reprobe()
		}

		return
	}

	start, seq := f.start.UTC(), s.jobs.Add(1)

	// Each job has its own directory for the files of a tool with fixed
	// names (wisdom, timers): concurrent jobs of a session never share
	// them. wsprd runs in the session workdir, where its hash table lives;
	// its logs that only grow are removed after each job.
	dir := filepath.Join(s.dir, fmt.Sprintf("j%d", seq))
	if p.shared {
		dir = s.dir

		defer s.removeWSPRLogs()
	} else {
		if err := os.Mkdir(dir, 0o700); err != nil {
			s.log.Error("slot decoder not run", slog.Any("error", err))
			s.status(app.DecoderStatus{State: app.DecoderError, Reason: "workdir"})

			return
		}

		defer func() {
			if err := os.RemoveAll(dir); err != nil {
				s.log.Warn("job directory not removed", slog.Any("error", err))
			}
		}()
	}

	in, err := s.r.o.Supervisor.NewInstance(process.Spec{
		ID:   fmt.Sprintf("dec-%s-%d", s.spec.Session, seq),
		Kind: "decoder", Mode: process.Batch, Path: path, Args: p.args(f.path, dir), ToolDirs: s.r.o.Tools.Dirs, Workdir: s.dir,
		Stdout: func(_ context.Context, rd io.Reader) error {
			process.ScanLines(rd, func(text string, _ bool) {
				if rec, ok := p.parse(p, text, partial); ok {
					// The decode date is the slot's UTC date (DEC-027),
					// its frequency the slot's dial plus the signal's.
					rec.Time, rec.DialHz = start, f.dial
					s.decode(rec)
				}
			})

			return nil
		},
		StderrRules: wsjtRules,
		Sink:        s.jobEvent,
		Timeouts:    process.Timeouts{Job: budget, Stop: StopGrace},
		Limits:      s.r.o.Limits,
	})
	if err != nil {
		s.log.Error("slot decoder not run", slog.Any("error", err))
		s.status(app.DecoderStatus{State: app.DecoderError, Reason: "start_failed"})

		return
	}

	// Terminal states were reported (jobEvent) and logged by the
	// supervisor.
	_ = in.Run(jctx)
}

// wsprLogs are the files wsprd appends to on every run; the hash table
// (hashtable.txt) and FFTW wisdom stay.
var wsprLogs = []string{"ALL_WSPR.TXT", "wspr_spots.txt", "wspr_timer.out"}

// removeWSPRLogs removes the logs of wsprd so that they do not grow for the
// whole session.
func (s *slotSession) removeWSPRLogs() {
	for _, name := range wsprLogs {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("wsprd log not removed", slog.String("file", name), slog.Any("error", err))
		}
	}
}

// jobEvent maps the end of a job to the session status: running after a
// completed job, unavailable when the tool is missing or refuses its
// options, error on a failure or a job past its deadline.
func (s *slotSession) jobEvent(e process.Event) {
	var st app.DecoderStatus

	switch e.State {
	case process.StateStopped:
		if e.Reason != "completed" {
			return
		}

		st = app.DecoderStatus{State: app.DecoderRunning}
	case process.StateUnavailable:
		st = app.DecoderStatus{State: app.DecoderUnavailable, Reason: e.Reason}
	case process.StateErrored, process.StateTimedOut:
		st = app.DecoderStatus{State: app.DecoderError, Reason: e.Reason}
	default:
		return
	}

	if e.Reprobe && s.r.o.Reprobe != nil {
		s.r.o.Reprobe()
	}

	s.status(st)
}

// decode reports a record unless the session is closed.
func (s *slotSession) decode(rec app.DecodeRecord) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()

	if !closed {
		s.ev.Decode(rec)
	}
}

// status reports a status change unless the session is closed. A running
// session carries the clock warning while the node clock is not
// synchronised (DEC-026).
func (s *slotSession) status(st app.DecoderStatus) {
	if st.State == app.DecoderRunning && s.r.o.ClockSynced != nil && !s.r.o.ClockSynced() {
		st.Warning = app.WarningClock
	}

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

// Close implements app.DecoderRun: the slot in progress is dropped, the
// running jobs are stopped and the workdir goes with the last of them.
func (s *slotSession) Close() {
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

	s.cancel()

	// The jobs still waiting are dropped (off the caller's goroutine: they
	// remove their slot files).
	go s.r.o.Queue.Purge(s)
}
