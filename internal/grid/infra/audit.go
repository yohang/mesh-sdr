// Package infra holds the grid adapters that are not tied to a dialect.
package infra

import (
	"context"
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/grid/app"
)

// LogAuditor writes audit records to the log until the audit_log table
// exists (ADR 0008 Q3).
type LogAuditor struct{ logger *slog.Logger }

// NewLogAuditor returns the auditor.
func NewLogAuditor(logger *slog.Logger) *LogAuditor { return &LogAuditor{logger: logger} }

// Record implements app.Auditor.
func (a *LogAuditor) Record(ctx context.Context, r app.AuditRecord) {
	attrs := []slog.Attr{
		slog.String("actor_kind", r.ActorKind),
		slog.String("action", r.Action),
		slog.String("target", r.Target),
		slog.String("result", r.Result),
	}

	for k, v := range r.Detail {
		attrs = append(attrs, slog.String(k, v))
	}

	a.logger.LogAttrs(ctx, slog.LevelInfo, "audit", attrs...)
}
