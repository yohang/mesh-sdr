package settings_test

import (
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/infra/settings"
)

func TestDefaults(t *testing.T) {
	var s app.Settings = settings.Defaults{}

	ctx := context.Background()
	if s.PasswordMinLength(ctx) != 10 || s.InvitationTTL(ctx) != 7*24*time.Hour || s.PasswordResetTTL(ctx) != 30*time.Minute {
		t.Error("defaults differ from FEATURE_SPEC §9 / ADR 0011")
	}
}
