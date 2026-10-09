package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
)

// Runs is the SQLite job_runs repository. Writes join the caller's
// transaction.
type Runs struct{ db *db.DB }

// NewRuns returns the repository.
func NewRuns(a *db.DB) *Runs { return &Runs{db: a} }

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

// Get returns the run of name, or nil when the job never ran. It reads
// through the writer inside a transaction (the start of a run is a
// read-modify-write).
func (r *Runs) Get(ctx context.Context, name string) (*Run, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetJobRun(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // never ran
	}

	if err != nil {
		return nil, fmt.Errorf("get job run %s: %w", name, err)
	}

	run, err := RehydrateRun(name, RunState{
		RunningSince: fromMS(row.RunningSince), LastStarted: fromMS(row.LastStartedAt), LastFinished: fromMS(row.LastFinishedAt),
		Status: Status(row.LastStatus.String), LastError: row.LastError.String, Rows: row.RowsAffected,
	})
	if err != nil {
		return nil, fmt.Errorf("job run %s: %w", name, err)
	}

	return run, nil
}

// Save stores a run.
func (r *Runs) Save(ctx context.Context, run *Run) error {
	err := sqlc.New(r.db.Writer(ctx)).UpsertJobRun(ctx, sqlc.UpsertJobRunParams{
		Job:            run.Name(),
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
