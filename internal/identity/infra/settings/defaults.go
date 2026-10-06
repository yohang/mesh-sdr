// Package settings adapts the identity settings port. Defaults is the
// interim adapter: it returns the FEATURE_SPEC §9 defaults until the DB
// settings store is wired (ADR 0011).
package settings

import (
	"context"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Defaults returns the default value of every identity setting.
type Defaults struct{}

// PasswordMinLength implements app.Settings (auth.password_min_length).
func (Defaults) PasswordMinLength(context.Context) int { return domain.DefaultPasswordMinLength }

// Default identity settings (ADR 0011).
const (
	DefaultInvitationTTL    = 7 * 24 * time.Hour
	DefaultPasswordResetTTL = 30 * time.Minute
)

// InvitationTTL implements app.Settings (invitations.ttl_hours).
func (Defaults) InvitationTTL(context.Context) time.Duration { return DefaultInvitationTTL }

// PasswordResetTTL implements app.Settings (password_reset.ttl_minutes).
func (Defaults) PasswordResetTTL(context.Context) time.Duration { return DefaultPasswordResetTTL }

// ListenPolicy implements app.Settings (listen_policy, default anonymous).
func (Defaults) ListenPolicy(context.Context) domain.ListenPolicy { return domain.ListenAnonymous }
