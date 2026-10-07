// Package app holds the grid use cases: node declaration and enrollment,
// control-channel ingestion, status, capabilities, devices and presence.
package app

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

// Clock returns the current time.
type Clock func() time.Time

// Transactor runs fn in one write transaction (db.Adapter.WithinTx).
type Transactor interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Actor kinds of audit records (§7.1 audit_log.actor_kind).
const (
	ActorSystem = "system"
	ActorCLI    = "cli"
	ActorUser   = "user"
	ActorNode   = "node"
)

// Audit results.
const (
	ResultOK     = "ok"
	ResultDenied = "denied"
	ResultError  = "error"
)

// AuditRecord is one audited action (ADR 0008 Q3).
type AuditRecord struct {
	ActorKind string
	Action    string
	Target    string
	Result    string
	Detail    map[string]string
}

// Auditor records security-relevant actions.
type Auditor interface {
	Record(ctx context.Context, r AuditRecord)
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
type timingsCell struct{ p atomic.Pointer[Timings] }

func newTimingsCell(t Timings) *timingsCell {
	c := &timingsCell{}
	c.p.Store(&t)

	return c
}

func (c *timingsCell) get() Timings { return *c.p.Load() }

func (c *timingsCell) set(t Timings) { c.p.Store(&t) }

// DefaultTimings returns the defaults.
func DefaultTimings() Timings {
	return Timings{
		HeartbeatInterval: 10 * time.Second,
		OfflineAfter:      60 * time.Second,
		EnrollmentTTL:     60 * time.Minute,
		PresenceStale:     45 * time.Second,
	}
}
