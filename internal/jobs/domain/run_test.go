package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/jobs/domain"
)

func TestName(t *testing.T) {
	if _, err := domain.NewName("sessions.reap"); err != nil {
		t.Error(err)
	}

	for _, bad := range []string{"", "reap", "Sessions.reap", "a." + strings.Repeat("b", 70)} {
		if _, err := domain.NewName(bad); !errors.Is(err, domain.ErrInvalidJobName) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestRunLifecycle(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := domain.NewRun(domain.MustName("audit.purge"))

	if err := r.Start(t0, time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := r.Start(t0.Add(time.Minute), time.Hour); !errors.Is(err, domain.ErrJobRunning) {
		t.Errorf("overlap: %v", err)
	}

	if err := r.Start(t0.Add(time.Hour), time.Hour); err != nil {
		t.Errorf("start after a dead run: %v", err)
	}

	r.Finish(t0.Add(2*time.Hour), 0, errors.New(strings.Repeat("é", 400)))

	if r.Running() || r.Status() != domain.StatusError || len(r.LastError()) > 512 || !strings.HasPrefix(r.LastError(), "é") {
		t.Errorf("run = %+v", r)
	}

	if _, err := domain.RehydrateRun(domain.MustName("audit.purge"), domain.RunState{Status: "maybe"}); !errors.Is(err, domain.ErrInvalidJobRun) {
		t.Errorf("rehydrate: %v", err)
	}
}
