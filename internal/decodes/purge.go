package decodes

import (
	"context"
	"time"
)

// Retention job of the decoded messages (TECHNICAL_SPEC §7.3 decodes.purge,
// ADR 0028: one age for every mode, plus a row cap).
const (
	JobPurge   = "decodes.purge"
	PurgeEvery = time.Hour
)

// Purge is the retention job.
type Purge struct{ m *Module }

// Purge returns the retention job.
func (m *Module) Purge() *Purge { return &Purge{m: m} }

// Name implements the jobs scheduler's Job.
func (j *Purge) Name() string { return JobPurge }

// Run implements the jobs scheduler's Job: it returns the rows deleted.
func (j *Purge) Run(ctx context.Context) (int64, error) {
	d := j.m.d

	return j.m.repo.Purge(ctx, d.Now().Add(-d.Retention()), d.MaxRows())
}
