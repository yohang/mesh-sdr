// Package app holds the settings use cases: the settings store (load,
// resolve, validate, save, audit, publish) and the effective configuration
// view (ADM-002, ADM-010, ADR 0010). Ports are declared here, on the
// consumer side; adapters live in internal/settings/infra and the
// composition root.
package app

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"

	"github.com/yohang/mesh-sdr/internal/settings/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Catalog is the settings schema: key definitions, the values the hub
// config sets (locked) and the shared validator.
type Catalog interface {
	Definitions() []domain.Definition
	Configured(key domain.Key) (domain.Configured, bool)
	// Validate checks a value of key and returns its Go value, or an error
	// matching domain.ErrUnknownSetting or domain.ErrInvalidSetting.
	Validate(key domain.Key, v domain.Value) (any, error)
	// Check runs the checks across keys on effective Go values.
	Check(get func(key string) (any, bool)) []shared.Violation
	// Schema returns the JSON Schema of the settings namespace.
	Schema() json.RawMessage
}

// Transactor runs a unit of work in one write transaction.
type Transactor interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Clock returns the current time.
type Clock func() time.Time

// Actor is who changes settings: a signed-in admin, or the hub itself (zero
// User).
type Actor struct {
	User      shared.UUID
	IP        netip.Addr
	RequestID string
}

// IsSystem reports whether the hub itself acts.
func (a Actor) IsSystem() bool { return a.User.IsZero() }

// Audit actions and results of the settings store.
const (
	ActionUpdate  = "settings.update"
	ActionReset   = "settings.reset"
	ActionIgnored = "settings.invalid_ignored"

	ResultOK     = "ok"
	ResultDenied = "denied"
)

// AuditRecord is one audited settings event. Before and After hold JSON
// text; secrets are masked.
type AuditRecord struct {
	Actor  Actor
	At     time.Time
	Action string
	Key    string
	Result string
	Before string
	After  string
}

// Auditor appends audit records (TECHNICAL_SPEC §7.1 audit_log). It joins
// the caller's transaction.
type Auditor interface {
	Record(ctx context.Context, r AuditRecord) error
}
