// Package sqlite implements the jobs repository and the table statistics
// of the retention view for the SQLite dialect.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/jobs/domain"
)

// Runs is the SQLite job_runs repository.
type Runs struct{ db db.Adapter }

var _ domain.Repository = (*Runs)(nil)

// NewRuns returns the repository.
func NewRuns(a db.Adapter) *Runs { return &Runs{db: a} }

func fromMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}

	return time.UnixMilli(v.Int64).UTC()
}

func nullMS(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// Get implements domain.Repository. It reads through the writer inside a
// transaction (the start of a run is a read-modify-write).
func (r *Runs) Get(ctx context.Context, name domain.Name) (*domain.Run, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetJobRun(ctx, name.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // never ran
	}

	if err != nil {
		return nil, fmt.Errorf("get job run %s: %w", name, err)
	}

	run, err := domain.RehydrateRun(name, domain.RunState{
		RunningSince: fromMS(row.RunningSince), LastStarted: fromMS(row.LastStartedAt), LastFinished: fromMS(row.LastFinishedAt),
		Status: domain.Status(row.LastStatus.String), LastError: row.LastError.String, Rows: row.RowsAffected,
	})
	if err != nil {
		return nil, fmt.Errorf("job run %s: %w", name, err)
	}

	return run, nil
}

// Save implements domain.Repository.
func (r *Runs) Save(ctx context.Context, run *domain.Run) error {
	err := sqlc.New(r.db.Writer(ctx)).UpsertJobRun(ctx, sqlc.UpsertJobRunParams{
		Job:            run.Name().String(),
		RunningSince:   nullMS(run.RunningSince()),
		LastStartedAt:  nullMS(run.LastStarted()),
		LastFinishedAt: nullMS(run.LastFinished()),
		LastStatus:     nullString(string(run.Status())),
		LastError:      nullString(run.LastError()),
		RowsAffected:   max(run.Rows(), 0),
	})
	if err != nil {
		return fmt.Errorf("save job run %s: %w", run.Name(), err)
	}

	return nil
}
