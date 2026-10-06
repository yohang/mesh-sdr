package app

import (
	"context"
	"time"
)

// Retention of the other identity rows (§7.1): invitations 30 days after
// expiry, redemption or revocation; one-time tokens 1 day after expiry or
// use. They are not settings.
const (
	InvitationRetention = 30 * 24 * time.Hour
	TokenRetention      = 24 * time.Hour
	// LinkPurgeEvery is the period of the purge jobs.
	LinkPurgeEvery = time.Hour
	purgeBatch     = 10_000
)

// Job names of the purges (TECHNICAL_SPEC §7.3 "Retention jobs").
const (
	JobInvitationsPurge = "invitations.purge"
	JobResetTokensPurge = "reset_tokens.purge"
	JobEmailTokensPurge = "email_tokens.purge"
)

// Ender deletes, in batches, the rows that ended before a cutoff.
type Ender interface {
	DeleteEndedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error)
}

// PurgeJob deletes the rows that ended more than a fixed retention ago (a
// job of the hub scheduler).
type PurgeJob struct {
	name      string
	retention time.Duration
	rows      Ender
	now       Clock
}

// NewPurgeJob returns the job.
func NewPurgeJob(name string, retention time.Duration, rows Ender, now Clock) *PurgeJob {
	return &PurgeJob{name: name, retention: retention, rows: rows, now: now}
}

// Name implements the jobs scheduler's Job.
func (j *PurgeJob) Name() string { return j.name }

// Run implements the jobs scheduler's Job: it returns the rows deleted.
func (j *PurgeJob) Run(ctx context.Context) (int64, error) {
	cutoff := j.now().Add(-j.retention)

	var total int64

	for {
		n, err := j.rows.DeleteEndedBefore(ctx, cutoff, purgeBatch)
		total += int64(n)

		if err != nil || n < purgeBatch {
			return total, err
		}
	}
}
