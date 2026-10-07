package jobs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/jobs"
)

func TestJobName(t *testing.T) {
	s, _ := newScheduler(t)

	if _, err := s.LastRun(context.Background(), "sessions.reap"); err != nil {
		t.Error(err)
	}

	for _, bad := range []string{"", "reap", "Sessions.reap", "a." + strings.Repeat("b", 70)} {
		if _, err := s.LastRun(context.Background(), bad); !errors.Is(err, jobs.ErrInvalidJobName) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestRunLifecycle(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := jobs.NewRun("audit.purge")

	if err := r.Start(t0, time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := r.Start(t0.Add(time.Minute), time.Hour); !errors.Is(err, jobs.ErrJobRunning) {
		t.Errorf("overlap: %v", err)
	}

	if err := r.Start(t0.Add(time.Hour), time.Hour); err != nil {
		t.Errorf("start after a dead run: %v", err)
	}

	r.Finish(t0.Add(2*time.Hour), 0, errors.New(strings.Repeat("é", 400)))

	if r.Running() || r.Status() != jobs.StatusError || len(r.LastError()) > 512 || !strings.HasPrefix(r.LastError(), "é") {
		t.Errorf("run = %+v", r)
	}

	if _, err := jobs.RehydrateRun("audit.purge", jobs.RunState{Status: "maybe"}); !errors.Is(err, jobs.ErrInvalidJobRun) {
		t.Errorf("rehydrate: %v", err)
	}
}

func TestAbandon(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := jobs.NewRun("audit.purge")

	if r.Abandon(t0) {
		t.Error("an idle run was abandoned")
	}

	_ = r.Start(t0, time.Hour)

	if !r.Abandon(t0.Add(time.Minute)) || r.Running() || r.Status() != jobs.StatusError || r.LastError() != jobs.Interrupted {
		t.Errorf("abandoned run = %+v", r)
	}
}
