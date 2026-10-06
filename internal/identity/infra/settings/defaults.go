// Package settings adapts the identity settings port. Defaults is the
// interim adapter: it returns the FEATURE_SPEC §9 defaults until the DB
// settings store is wired (ADR 0011).
package settings

import (
	"context"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Defaults returns the default value of every identity setting.
type Defaults struct{}

// PasswordMinLength implements app.Settings (auth.password_min_length).
func (Defaults) PasswordMinLength(context.Context) int { return domain.DefaultPasswordMinLength }
