package jobs_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/jobs"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// fakeJob affects rows or fails; it can block until released.
type fakeJob struct {
	name    string
	rows    int64
	err     error
	started chan struct{}
	release chan struct{}
	runs    int
}

func (j *fakeJob) Name() string { return j.name }

func (j *fakeJob) Run(context.Context) (int64, error) {
	j.runs++

	if j.started != nil {
		j.started <- struct{}{}
		<-j.release
	}

	return j.rows, j.err
}

func newScheduler(t *testing.T) (*jobs.Scheduler, *clock) {
	t.Helper()

	a := dbtest.NewSQLite(t)
	c := &clock{now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}

	return jobs.NewScheduler(jobs.NewRuns(a), a, c.Now, slog.New(slog.DiscardHandler)), c
}

func TestRunNowRecordsRuns(t *testing.T) {
	ctx := context.Background()
	s, c := newScheduler(t)

	ok := &fakeJob{name: "test.ok", rows: 42}
	bad := &fakeJob{name: "test.bad", err: errors.New("disk full")}

	s.Register(ok, time.Hour)
	s.Register(bad, time.Hour)

	if run, err := s.LastRun(ctx, "test.ok"); err != nil || !run.LastStarted().IsZero() || run.Status() != jobs.StatusNone {
		t.Fatalf("never ran: %+v %v", run, err)
	}

	if n, err := s.RunNow(ctx, "test.ok"); err != nil || n != 42 {
		t.Fatalf("run = %d, %v", n, err)
	}

	run, _ := s.LastRun(ctx, "test.ok")
	if run.Running() || run.Status() != jobs.StatusOK || run.Rows() != 42 || !run.LastFinished().Equal(c.Now()) {
		t.Errorf("run = %+v", run)
	}

	if _, err := s.RunNow(ctx, "test.bad"); err == nil {
		t.Fatal("failure not reported")
	}

	run, _ = s.LastRun(ctx, "test.bad")
	if run.Status() != jobs.StatusError || run.LastError() != "disk full" || run.Running() {
		t.Errorf("failed run = %+v", run)
	}

	if _, err := s.RunNow(ctx, "test.nope"); !errors.Is(err, jobs.ErrUnknownJob) {
		t.Errorf("unknown job: %v", err)
	}
}

func TestJobNeverOverlaps(t *testing.T) {
	ctx := context.Background()
	s, c := newScheduler(t)

	j := &fakeJob{name: "test.slow", started: make(chan struct{}), release: make(chan struct{})}
	s.Register(j, time.Hour)

	done := make(chan error, 1)

	go func() {
		_, err := s.RunNow(ctx, "test.slow")
		done <- err
	}()

	<-j.started

	if _, err := s.RunNow(ctx, "test.slow"); !errors.Is(err, jobs.ErrJobRunning) {
		t.Errorf("overlapping run: %v", err)
	}

	close(j.release)

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A run longer than StaleAfter still blocks a second run in the hub.
	j2 := &fakeJob{name: "test.long", started: make(chan struct{}, 1), release: make(chan struct{})}
	s.Register(j2, time.Hour)

	done2 := make(chan error, 1)

	go func() {
		_, err := s.RunNow(ctx, "test.long")
		done2 <- err
	}()

	<-j2.started
	c.Advance(2 * jobs.StaleAfter)

	if _, err := s.RunNow(ctx, "test.long"); !errors.Is(err, jobs.ErrJobRunning) {
		t.Errorf("overlap after StaleAfter: %v", err)
	}

	close(j2.release)

	if err := <-done2; err != nil {
		t.Fatal(err)
	}
}

// A run left in progress by a stopped hub is ended at start.
func TestSchedulerEndsInterruptedRuns(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	c := &clock{now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	repo := jobs.NewRuns(a)

	r := jobs.NewRun("test.start")
	_ = r.Start(c.Now(), time.Hour)
	_ = repo.Save(ctx, r)

	s := jobs.NewScheduler(repo, a, c.Now, slog.New(slog.DiscardHandler))
	j := &fakeJob{name: "test.start", started: make(chan struct{}, 1), release: make(chan struct{})}
	s.Register(j, time.Hour)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() { s.Run(runCtx); close(done) }()

	// The run at start is not refused as overlapping.
	<-j.started
	close(j.release)
	cancel()
	<-done

	got, _ := s.LastRun(ctx, "test.start")
	if got.Running() || got.Status() != jobs.StatusOK {
		t.Errorf("run = %+v", got)
	}
}

func TestSchedulerRunsAtStart(t *testing.T) {
	s, _ := newScheduler(t)
	j := &fakeJob{name: "test.start", rows: 1, started: make(chan struct{}, 1), release: make(chan struct{})}
	s.Register(j, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { s.Run(ctx); close(done) }()

	<-j.started
	close(j.release)
	cancel()
	<-done

	run, err := s.LastRun(context.Background(), "test.start")
	if err != nil || run.Status() != jobs.StatusOK {
		t.Errorf("run at start = %+v %v", run, err)
	}
}

type stats struct{ rows int64 }

func (s stats) Stats(context.Context) (int64, int64, bool, error) { return s.rows, 4096, true, nil }

type values struct{}

func (values) Duration(string) time.Duration { return 30 * 24 * time.Hour }

func TestRetention(t *testing.T) {
	ctx := context.Background()
	s, _ := newScheduler(t)
	s.Register(&fakeJob{name: "sessions.reap", rows: 3}, time.Hour)

	au := &audit.Records{}
	r := jobs.NewRetention([]jobs.Store{{Name: "sessions", Label: "Sessions", SettingKey: "retention.sessions", Job: "sessions.reap", Stats: stats{rows: 7}}},
		s, values{}, au)

	views, err := r.List(ctx)
	if err != nil || len(views) != 1 || views[0].Rows != 7 || views[0].Retention != 30*24*time.Hour || views[0].LastRun.Status() != jobs.StatusNone {
		t.Fatalf("list = %+v, %v", views, err)
	}

	if n, err := r.Purge(ctx, "sessions"); err != nil || n != 3 {
		t.Fatalf("purge = %d, %v", n, err)
	}

	if len(*au) != 1 || (*au)[0].TargetID != "sessions" || (*au)[0].After["rows_deleted"] != "3" || (*au)[0].Actor != audit.Caller {
		t.Errorf("audit = %+v", *au)
	}

	if _, err := r.Purge(ctx, "files"); !errors.Is(err, jobs.ErrUnknownStore) {
		t.Errorf("unknown store: %v", err)
	}
}

func TestFunc(t *testing.T) {
	s, _ := newScheduler(t)
	s.Register(jobs.Func("test.func", func(context.Context) (int64, error) { return 7, nil }), time.Hour)

	if n, err := s.RunNow(context.Background(), "test.func"); n != 7 || err != nil {
		t.Errorf("RunNow = %d, %v", n, err)
	}
}
