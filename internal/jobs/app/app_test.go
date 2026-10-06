package app_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/jobs/app"
	"github.com/yohang/mesh-sdr/internal/jobs/domain"
	"github.com/yohang/mesh-sdr/internal/jobs/infra/sqlite"
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

func newScheduler(t *testing.T) (*app.Scheduler, *clock) {
	t.Helper()

	a := dbtest.NewSQLite(t)
	c := &clock{now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}

	return app.NewScheduler(sqlite.NewRuns(a), a, c.Now, slog.New(slog.DiscardHandler)), c
}

func TestRunNowRecordsRuns(t *testing.T) {
	ctx := context.Background()
	s, c := newScheduler(t)

	ok := &fakeJob{name: "test.ok", rows: 42}
	bad := &fakeJob{name: "test.bad", err: errors.New("disk full")}

	s.Register(ok, time.Hour)
	s.Register(bad, time.Hour)

	if run, err := s.LastRun(ctx, "test.ok"); err != nil || !run.LastStarted().IsZero() || run.Status() != domain.StatusNone {
		t.Fatalf("never ran: %+v %v", run, err)
	}

	if n, err := s.RunNow(ctx, "test.ok"); err != nil || n != 42 {
		t.Fatalf("run = %d, %v", n, err)
	}

	run, _ := s.LastRun(ctx, "test.ok")
	if run.Running() || run.Status() != domain.StatusOK || run.Rows() != 42 || !run.LastFinished().Equal(c.Now()) {
		t.Errorf("run = %+v", run)
	}

	if _, err := s.RunNow(ctx, "test.bad"); err == nil {
		t.Fatal("failure not reported")
	}

	run, _ = s.LastRun(ctx, "test.bad")
	if run.Status() != domain.StatusError || run.LastError() != "disk full" || run.Running() {
		t.Errorf("failed run = %+v", run)
	}

	if _, err := s.RunNow(ctx, "test.nope"); !errors.Is(err, domain.ErrUnknownJob) {
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

	if _, err := s.RunNow(ctx, "test.slow"); !errors.Is(err, domain.ErrJobRunning) {
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
	c.Advance(2 * app.StaleAfter)

	if _, err := s.RunNow(ctx, "test.long"); !errors.Is(err, domain.ErrJobRunning) {
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
	repo := sqlite.NewRuns(a)

	r := domain.NewRun(domain.MustName("test.start"))
	_ = r.Start(c.Now(), time.Hour)
	_ = repo.Save(ctx, r)

	s := app.NewScheduler(repo, a, c.Now, slog.New(slog.DiscardHandler))
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
	if got.Running() || got.Status() != domain.StatusOK {
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
	if err != nil || run.Status() != domain.StatusOK {
		t.Errorf("run at start = %+v %v", run, err)
	}
}

func TestBatched(t *testing.T) {
	left := 25

	n, err := app.Batched(context.Background(), 10, func(_ context.Context, batch int) (int, error) {
		k := min(batch, left)
		left -= k

		return k, nil
	})
	if err != nil || n != 25 || left != 0 {
		t.Errorf("batched = %d, %v, left %d", n, err, left)
	}
}

type stats struct{ rows int64 }

func (s stats) Stats(context.Context) (int64, int64, bool, error) { return s.rows, 4096, true, nil }

type values struct{}

func (values) Duration(string) time.Duration { return 30 * 24 * time.Hour }

type audit struct{ records []app.PurgeRecord }

func (a *audit) RecordPurge(_ context.Context, r app.PurgeRecord) error {
	a.records = append(a.records, r)

	return nil
}

func TestRetention(t *testing.T) {
	ctx := context.Background()
	s, c := newScheduler(t)
	s.Register(&fakeJob{name: "sessions.reap", rows: 3}, time.Hour)

	au := &audit{}
	r := app.NewRetention([]app.Store{{Name: "sessions", Label: "Sessions", SettingKey: "retention.sessions", Job: "sessions.reap", Stats: stats{rows: 7}}},
		s, values{}, au, c.Now)

	views, err := r.List(ctx)
	if err != nil || len(views) != 1 || views[0].Rows != 7 || views[0].Retention != 30*24*time.Hour || views[0].LastRun.Status() != domain.StatusNone {
		t.Fatalf("list = %+v, %v", views, err)
	}

	actor := app.Actor{RequestID: "r1"}

	if n, err := r.Purge(ctx, actor, "sessions"); err != nil || n != 3 {
		t.Fatalf("purge = %d, %v", n, err)
	}

	if len(au.records) != 1 || au.records[0].Store != "sessions" || au.records[0].Rows != 3 || au.records[0].Actor != actor {
		t.Errorf("audit = %+v", au.records)
	}

	if _, err := r.Purge(ctx, actor, "files"); !errors.Is(err, app.ErrUnknownStore) {
		t.Errorf("unknown store: %v", err)
	}
}
