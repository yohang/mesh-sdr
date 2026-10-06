package app

import (
	"context"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Session retention (TECHNICAL_SPEC §7.3 `sessions.reap`): rows are deleted
// 30 days after expiry or revocation by default (retention.sessions),
// hourly, in batches.
const (
	DefaultSessionRetention = 30 * 24 * time.Hour
	SessionReapEvery        = time.Hour
	sessionReapBatchSz      = 10_000
)

// Retention gives the current retention of the identity stores (DB
// settings retention.sessions and retention.audit_log, ADR 0010).
type Retention interface {
	SessionRetention() time.Duration
	AuditRetention() time.Duration
}

// FixedRetention is a constant retention (tests, defaults).
type FixedRetention struct{ Sessions, Audit time.Duration }

// SessionRetention implements Retention.
func (r FixedRetention) SessionRetention() time.Duration { return r.Sessions }

// AuditRetention implements Retention.
func (r FixedRetention) AuditRetention() time.Duration { return r.Audit }

// SessionReaper deletes ended sessions past their retention.
type SessionReaper struct {
	sessions  domain.SessionRepository
	retention Retention
	now       Clock
}

// NewSessionReaper returns the reaper.
func NewSessionReaper(sessions domain.SessionRepository, retention Retention, now Clock) *SessionReaper {
	return &SessionReaper{sessions: sessions, retention: retention, now: now}
}

// Reap deletes every session that ended more than the session retention
// ago and returns how many were deleted.
func (r *SessionReaper) Reap(ctx context.Context) (int, error) {
	cutoff := r.now().Add(-r.retention.SessionRetention())
	total := 0

	for {
		n, err := r.sessions.DeleteEndedBefore(ctx, cutoff, sessionReapBatchSz)
		total += n

		if err != nil || n < sessionReapBatchSz {
			return total, err
		}
	}
}

// Job names and periods (TECHNICAL_SPEC §7.3 "Retention jobs").
const (
	JobSessionsReap = "sessions.reap"
	JobAuditPurge   = "audit.purge"
	AuditPurgeEvery = 24 * time.Hour
	auditPurgeBatch = 10_000
)

// Name implements the jobs scheduler's Job: the hub runs the reaper every
// SessionReapEvery and logs its outcome.
func (r *SessionReaper) Name() string { return JobSessionsReap }

// Run implements the jobs scheduler's Job.
func (r *SessionReaper) Run(ctx context.Context) (int64, error) {
	n, err := r.Reap(ctx)

	return int64(n), err
}

// AuditPurger deletes audit entries older than the audit retention
// (retention.audit_log, never less than 30 days; TECHNICAL_SPEC §7.3
// `audit.purge`), in batches.
type AuditPurger struct {
	audit     domain.AuditPurge
	retention Retention
	now       Clock
}

// NewAuditPurger returns the job.
func NewAuditPurger(audit domain.AuditPurge, retention Retention, now Clock) *AuditPurger {
	return &AuditPurger{audit: audit, retention: retention, now: now}
}

// Name implements the jobs scheduler's Job.
func (p *AuditPurger) Name() string { return JobAuditPurge }

// Run implements the jobs scheduler's Job: it returns the entries deleted.
func (p *AuditPurger) Run(ctx context.Context) (int64, error) {
	cutoff := p.now().Add(-p.retention.AuditRetention())

	var total int64

	for {
		n, err := p.audit.DeleteBefore(ctx, cutoff, auditPurgeBatch)
		total += int64(n)

		if err != nil || n < auditPurgeBatch {
			return total, err
		}
	}
}
