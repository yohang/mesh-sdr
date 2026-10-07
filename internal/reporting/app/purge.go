package app

import (
	"context"
	"time"

	"github.com/yohang/mesh-sdr/internal/reporting/domain"
)

// Outbox retention (§7.3 "Retention jobs", ADR 0020 Q15).
const (
	// JobOutboxPurge is the name of the outbox retention job.
	JobOutboxPurge = "outbox.purge"
	// OutboxPurgeEvery is its period.
	OutboxPurgeEvery = time.Hour
	// KeySentRetention and KeyDeadRetention are its settings.
	KeySentRetention = "retention.reporting_outbox.sent"
	KeyDeadRetention = "retention.reporting_outbox.dead"
	// PendingTTL is how long the entries of a disabled network wait before
	// they become dead (§7.3 rule 4, reporting.<network>.pending_ttl).
	PendingTTL = time.Hour
	// purgeBatch bounds each delete (§7.3: ≤ 10 000 rows per transaction).
	purgeBatch = 10_000
)

// RetentionValues reads the retention settings.
type RetentionValues interface {
	Duration(key string) time.Duration
}

// PurgeJob is the outbox.purge job: the waiting entries of networks
// without an enabled transport die after PendingTTL, sent entries are
// deleted after retention.reporting_outbox.sent and dead ones after
// retention.reporting_outbox.dead. Each statement runs in its own short
// transaction.
type PurgeJob struct {
	repo   domain.Repository
	engine *Engine
	values RetentionValues
	now    Clock
}

// NewPurgeJob returns the job.
func NewPurgeJob(repo domain.Repository, engine *Engine, values RetentionValues, now Clock) *PurgeJob {
	return &PurgeJob{repo: repo, engine: engine, values: values, now: now}
}

// Name implements jobs/app.Job.
func (j *PurgeJob) Name() string { return JobOutboxPurge }

// Run implements jobs/app.Job.
func (j *PurgeJob) Run(ctx context.Context) (int64, error) {
	now := j.now()

	var total int64

	for _, n := range domain.Networks {
		if j.engine.Enabled(n) {
			continue
		}

		k, err := j.repo.KillWaiting(ctx, n, now.Add(-PendingTTL), domain.ReasonDisabled)
		if err != nil {
			return total, err
		}

		total += k
	}

	for _, p := range []struct {
		status domain.Status
		key    string
	}{{domain.StatusSent, KeySentRetention}, {domain.StatusDead, KeyDeadRetention}} {
		keep := j.values.Duration(p.key)
		if keep <= 0 {
			continue
		}

		for {
			n, err := j.repo.Purge(ctx, p.status, now.Add(-keep), purgeBatch)
			if err != nil {
				return total, err
			}

			total += n

			if n < purgeBatch {
				break
			}
		}
	}

	return total, nil
}
