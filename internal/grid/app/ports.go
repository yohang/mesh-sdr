// Package app holds the grid use cases: node declaration and enrollment,
// control-channel ingestion, status, capabilities, devices and presence.
package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
)

// Clock returns the current time.
type Clock func() time.Time

// Transactor runs fn in one write transaction (db.DB.WithinTx).
type Transactor interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// auditor appends grid audit records (ADR 0008 Q3). A record that cannot
// be written is logged and never fails the audited action.
type auditor struct {
	log    audit.Appender
	logger *slog.Logger
}

func (a auditor) Record(ctx context.Context, r audit.Record) {
	if err := a.log.Append(ctx, r); err != nil {
		a.logger.ErrorContext(ctx, "write grid audit record", slog.String("action", r.Action), slog.String("target", r.TargetID),
			slog.String("result", string(r.Result)), slog.Any("error", err))
	}
}

// VerificationKeys are the published access-token keys (§5.8).
type VerificationKeys struct {
	Keys        []token.JWK
	RevokedKids []string
}

// KeySource gives the public keys that verify access tokens, pushed to the
// nodes over ctl.keys.update. The identity keyring implements it (ACC-007);
// an interim adapter holds one ephemeral key per hub process.
type KeySource interface {
	VerificationKeys(ctx context.Context) (VerificationKeys, error)
	// Changed is signalled when the keys change (rotation, revocation). A
	// nil channel means the keys never change.
	Changed() <-chan struct{}
}

// TokenIssuer signs access tokens with the current signing key.
type TokenIssuer interface {
	Issue(ctx context.Context, c token.Claims) (string, error)
}

// RevocationBroadcaster pushes revoked sessions (token.SessionRef) and users
// to every node, which close the matching media connections and refuse the
// tokens issued up to at (hub clock; zero means now) (§5.8).
type RevocationBroadcaster interface {
	BroadcastRevocations(ctx context.Context, at time.Time, sessions, users []string)
}

// Timings are the grid durations, with the FEATURE_SPEC defaults. The
// heartbeat interval and the offline delay follow the DB settings
// grid.heartbeat_interval_s and grid.offline_after_s (ADR 0018).
type Timings struct {
	HeartbeatInterval time.Duration // grid.heartbeat_interval_s
	OfflineAfter      time.Duration // grid.offline_after_s
	EnrollmentTTL     time.Duration // grid.enrollment_ttl_minutes
	PresenceStale     time.Duration // presence.stale_after (§7.3)
}

// timingsCell holds the current Timings of a service: the heartbeat
// interval and the offline delay are DB settings that change at run time
// (ADR 0018).
type timingsCell struct {
	p       atomic.Pointer[Timings]
	changes chan struct{}
}

func newTimingsCell(t Timings) *timingsCell {
	c := &timingsCell{changes: make(chan struct{}, 1)}
	c.p.Store(&t)

	return c
}

func (c *timingsCell) get() Timings { return *c.p.Load() }

// set stores t and wakes the service loop, which resets its ticker.
func (c *timingsCell) set(t Timings) {
	c.p.Store(&t)

	select {
	case c.changes <- struct{}{}:
	default:
	}
}

// DefaultTimings returns the defaults.
func DefaultTimings() Timings {
	return Timings{
		HeartbeatInterval: 10 * time.Second,
		OfflineAfter:      60 * time.Second,
		EnrollmentTTL:     60 * time.Minute,
		PresenceStale:     45 * time.Second,
	}
}
