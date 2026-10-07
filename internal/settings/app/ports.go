// Package app holds the settings use cases: the settings store (load,
// resolve, validate, save, audit, publish) and the effective configuration
// view (ADM-002, ADM-010, ADR 0010). Ports are declared here, on the
// consumer side; adapters live in internal/settings/infra and the
// composition root.
package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/settings/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Catalog is the settings schema: key definitions, the values the hub
// config sets (locked) and the shared validator.
type Catalog interface {
	Definitions() []domain.Definition
	Configured(key string) (domain.Configured, bool)
	// Validate checks a value of key and returns its Go value, or an error
	// matching domain.ErrUnknownSetting or domain.ErrInvalidSetting.
	Validate(key string, v domain.Value) (any, error)
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

// Audit actions and results of the settings store.
const (
	ActionUpdate  = "settings.update"
	ActionReset   = "settings.reset"
	ActionIgnored = "settings.invalid_ignored"
)

// record is the audit record of one settings event; before and after hold
// JSON text (secrets masked), empty when absent.
func record(action, key string, result audit.Result, before, after string) audit.Record {
	r := audit.Record{Action: action, Result: result, TargetType: "setting", TargetID: key}

	if before != "" {
		r.Before = map[string]string{"value": before}
	}

	if after != "" {
		r.After = map[string]string{"value": after}
	}

	return r
}
