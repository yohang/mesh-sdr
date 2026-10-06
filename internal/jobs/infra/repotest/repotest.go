// Package repotest holds the contract suite of the job_runs repository
// (ADR 0006): every dialect adapter runs it against a migrated database.
package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/jobs/domain"
)

// Factory opens a fresh repository on a migrated database.
type Factory func(t *testing.T) domain.Repository

// Run runs the repository contract.
func Run(t *testing.T, open Factory) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	name := domain.MustName("sessions.reap")

	repo := open(t)

	if r, err := repo.Get(ctx, name); err != nil || r != nil {
		t.Fatalf("never ran: %v, %v", r, err)
	}

	r := domain.NewRun(name)
	if err := r.Start(t0, time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := repo.Save(ctx, r); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, name)
	if err != nil || !got.Running() || !got.RunningSince().Equal(t0) || !got.LastStarted().Equal(t0) {
		t.Fatalf("running = %+v, %v", got, err)
	}

	got.Finish(t0.Add(time.Second), 12, errors.New("boom"))

	if err := repo.Save(ctx, got); err != nil {
		t.Fatal(err)
	}

	got, err = repo.Get(ctx, name)
	if err != nil || got.Running() || got.Status() != domain.StatusError || got.LastError() != "boom" || got.Rows() != 12 ||
		!got.LastFinished().Equal(t0.Add(time.Second)) {
		t.Errorf("finished = %+v, %v", got, err)
	}
}
