package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Session retention (TECHNICAL_SPEC §7.3 `sessions.reap`): rows are deleted
// 30 days after expiry or revocation, hourly, in batches.
const (
	SessionRetention   = 30 * 24 * time.Hour
	SessionReapEvery   = time.Hour
	sessionReapBatchSz = 10_000
)

// SessionReaper deletes ended sessions past their retention.
type SessionReaper struct {
	sessions domain.SessionRepository
	now      Clock
	logger   *slog.Logger
}

// NewSessionReaper returns the reaper.
func NewSessionReaper(sessions domain.SessionRepository, now Clock, logger *slog.Logger) *SessionReaper {
	return &SessionReaper{sessions: sessions, now: now, logger: logger}
}

// Reap deletes every session that ended more than SessionRetention ago and
// returns how many were deleted.
func (r *SessionReaper) Reap(ctx context.Context) (int, error) {
	cutoff := r.now().Add(-SessionRetention)
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
