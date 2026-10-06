// Package app holds the grid use cases: node declaration and enrollment,
// control-channel ingestion, status, capabilities, devices and presence.
package app

import (
	"context"
	"time"
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

// Timings are the grid durations that become DB settings later (ADR 0008
// Q2), with the FEATURE_SPEC defaults.
type Timings struct {
	HeartbeatInterval time.Duration // grid.heartbeat_interval_s
	OfflineAfter      time.Duration // grid.offline_after_s
	EnrollmentTTL     time.Duration // grid.enrollment_ttl_minutes
	ConnectionsKeep   time.Duration // retention.connections
	PresenceStale     time.Duration // presence.stale_after (§7.3)
}

// DefaultTimings returns the defaults.
func DefaultTimings() Timings {
	return Timings{
		HeartbeatInterval: 10 * time.Second,
		OfflineAfter:      60 * time.Second,
		EnrollmentTTL:     60 * time.Minute,
		ConnectionsKeep:   30 * 24 * time.Hour,
		PresenceStale:     45 * time.Second,
	}
}
