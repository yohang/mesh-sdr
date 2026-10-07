package app_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/ratelimit"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func (e *env) setup() *app.Setup {
	return app.NewSetup(app.SetupDeps{
		Users: e.users, Audit: e.audit, Tx: e.db, Hasher: e.hasher, IDs: shared.NewUUIDv7Generator(), Now: e.clock.Now,
		Policies: app.NewPolicies(nil, nil), Auth: e.auth, Limiter: ratelimit.NewIP(time.Minute, 100, 10),
		HubURL: "https://hub.example/", Logger: slog.New(slog.DiscardHandler),
	})
}

func begin(t *testing.T, s *app.Setup) string {
	t.Helper()

	u, err := s.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	token, ok := strings.CutPrefix(u, "https://hub.example/setup/")
	if !ok || len(token) != 43 {
		t.Fatalf("setup URL = %q", u)
	}

	return token
}

func TestSetupLink(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	s := e.setup()
	meta := app.RequestMeta{IP: ip}

	token := begin(t, s)

	if err := s.Check(token, meta); err != nil {
		t.Fatalf("check: %v", err)
	}

	if err := s.Check(token+"x", meta); !errors.Is(err, domain.ErrSetupTokenInvalid) {
		t.Errorf("wrong token: %v", err)
	}

	in := app.SetupInput{Token: token, Username: "root", Password: fresh, Meta: meta}

	if _, err := s.Complete(ctx, app.SetupInput{Token: token, Username: "root", Password: "short", Meta: meta}); !errors.Is(err, domain.ErrInvalidPassword) {
		t.Errorf("short password: %v", err)
	}

	res, err := s.Complete(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	if res.Principal.Role() != domain.RoleAdmin || res.Principal.MustChangePassword() {
		t.Errorf("principal role %v", res.Principal.Role())
	}

	if _, err := s.Complete(ctx, in); !errors.Is(err, domain.ErrSetupTokenInvalid) {
		t.Errorf("second use: %v", err)
	}

	if got := e.actions(t); !strings.Contains(strings.Join(got, ","), domain.ActionUserCreate+":,"+domain.ActionLoginSuccess) {
		t.Errorf("audit = %v", got)
	}

	if u, err := s.Begin(ctx); err != nil || u != "" {
		t.Errorf("begin with an admin = %q, %v", u, err)
	}
}

func TestSetupLinkExpiresAndYieldsToAnAdmin(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	s := e.setup()
	meta := app.RequestMeta{IP: ip}

	token := begin(t, s)
	e.clock.Advance(app.SetupTTL)

	if err := s.Check(token, meta); !errors.Is(err, domain.ErrSetupTokenInvalid) {
		t.Errorf("expired: %v", err)
	}

	token = begin(t, s)
	e.addUser(t, "other", "", domain.RoleAdmin)

	if _, err := s.Complete(ctx, app.SetupInput{Token: token, Username: "root", Password: fresh, Meta: meta}); !errors.Is(err, domain.ErrSetupTokenInvalid) {
		t.Errorf("admin created meanwhile: %v", err)
	}

	if err := s.Check(token, meta); !errors.Is(err, domain.ErrSetupTokenInvalid) {
		t.Errorf("link still valid: %v", err)
	}
}
