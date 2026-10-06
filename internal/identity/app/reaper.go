package app

import (
	"context"
	"log/slog"
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
	logger    *slog.Logger
}

// NewSessionReaper returns the reaper.
func NewSessionReaper(sessions domain.SessionRepository, retention Retention, now Clock, logger *slog.Logger) *SessionReaper {
	return &SessionReaper{sessions: sessions, retention: retention, now: now, logger: logger}
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

// Run reaps at start, then every interval, until ctx is done. It is a
// background worker: it logs its own errors.
func (r *SessionReaper) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		n, err := r.Reap(ctx)

		switch {
		case err != nil && ctx.Err() == nil:
			r.logger.ErrorContext(ctx, "session reaper failed", slog.Any("error", err))
		case n > 0:
			r.logger.InfoContext(ctx, "expired sessions deleted", slog.Int("rows", n))
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
