package app

import (
	"context"
	_ "embed"
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/shell/domain"
)

//go:embed default_policy.md
var defaultPolicy string

// DefaultPolicy returns the built-in usage policy, shown until the admin sets
// one (settings.receiver.usage_policy_text).
func DefaultPolicy() domain.PolicyText { return domain.MustPolicyText(defaultPolicy) }

// PolicySettings reads the admin-set usage policy. set is false when no
// policy is set (the default applies).
type PolicySettings interface {
	UsagePolicy(ctx context.Context) (text domain.PolicyText, set bool, err error)
}

// Policy returns the usage policy (UI-003).
type Policy struct {
	settings PolicySettings
	logger   *slog.Logger
}

// NewPolicy returns the use case.
func NewPolicy(settings PolicySettings, logger *slog.Logger) *Policy {
	return &Policy{settings: settings, logger: logger}
}

// Text returns the admin-set policy, or the default when none is set. A
// failing settings source falls back to the default and is logged.
func (p *Policy) Text(ctx context.Context) domain.PolicyText {
	text, set, err := p.settings.UsagePolicy(ctx)

	switch {
	case err != nil:
		p.logger.WarnContext(ctx, "usage policy unavailable, using the default", slog.Any("error", err))

		return DefaultPolicy()
	case !set:
		return DefaultPolicy()
	default:
		return text
	}
}
