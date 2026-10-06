package app_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

const fresh = "a fresh and long passphrase"

func (e *env) passwords() *app.Passwords {
	return app.NewPasswords(app.PasswordsDeps{
		Users: e.users, Sessions: e.sessions, Audit: e.audit, Tx: e.db, Hasher: e.hasher, IDs: shared.NewUUIDv7Generator(),
		Now: e.clock.Now, Policies: app.NewPolicies(nil, nil), Throttle: domain.DefaultThrottlePolicy(),
		SessionPolicy: domain.DefaultSessionPolicy(), Logger: slog.New(slog.DiscardHandler),
	})
}

func (e *env) change(t *testing.T, s *domain.Session, current, next string) (app.ChangePasswordResult, error) {
	t.Helper()

	return e.passwords().Change(context.Background(), app.ChangePasswordInput{Session: s, Current: current, New: next, Meta: app.RequestMeta{IP: ip}})
}

func TestChangePasswordRevokesEverySessionAndRotates(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	u := e.addUser(t, "alice", "", domain.RoleListener)

	current, _ := e.login("alice", password)
	other, _ := e.login("alice", password)

	e.clock.Advance(time.Hour)

	res, err := e.change(t, current.Session, password, fresh)
	if err != nil {
		t.Fatal(err)
	}

	if res.RevokedSessions != 2 || res.Forced || res.Remember || res.Principal.UserID() != u.ID() {
		t.Errorf("result = %+v", res)
	}

	if !res.Session.AbsoluteExpiresAt().Equal(current.Session.AbsoluteExpiresAt()) {
		t.Errorf("absolute expiry %v, want %v", res.Session.AbsoluteExpiresAt(), current.Session.AbsoluteExpiresAt())
	}

	for _, tok := range []domain.SessionToken{current.Token, other.Token} {
		if _, err := e.auth.Resolve(ctx, tok.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("old session resolves: %v", err)
		}
	}

	if _, err := e.auth.Resolve(ctx, res.Token.Cookie()); err != nil {
		t.Errorf("new session: %v", err)
	}

	if _, err := e.login("alice", fresh); err != nil {
		t.Errorf("login with the new password: %v", err)
	}

	if !slices.Contains(e.actions(t), domain.ActionPasswordChange+":") {
		t.Errorf("audit = %v", e.actions(t))
	}
}

func TestChangePasswordClearsTheForcedFlag(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)

	added, err := e.admin.Add(ctx, app.AddUserInput{Username: "root", Role: domain.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}

	in, err := e.login("root", added.GeneratedPassword)
	if err != nil || !in.Principal.MustChangePassword() {
		t.Fatalf("login: %v, flagged %v", err, in.Principal.MustChangePassword())
	}

	res, err := e.change(t, in.Session, added.GeneratedPassword, fresh)
	if err != nil || !res.Forced || res.Principal.MustChangePassword() {
		t.Fatalf("change: %v forced %v", err, res.Forced)
	}

	stored, _ := e.users.ByID(ctx, added.User.ID())
	if stored.MustChangePassword() {
		t.Error("flag still set")
	}
}

func TestChangePasswordRefusals(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	u := e.addUser(t, "alice", "", domain.RoleListener)
	in, _ := e.login("alice", password)

	if _, err := e.passwords().Change(ctx, app.ChangePasswordInput{Current: password, New: fresh}); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("no session: %v", err)
	}

	if _, err := e.change(t, in.Session, password, "short"); !errors.Is(err, domain.ErrInvalidPassword) {
		t.Errorf("short: %v", err)
	}

	if _, err := e.change(t, in.Session, password, password); !errors.Is(err, domain.ErrInvalidPassword) {
		t.Errorf("same: %v", err)
	}

	// A wrong current password counts as a failed login; the throttle then
	// refuses without checking the password.
	for i := range 4 {
		if _, err := e.change(t, in.Session, "wrong password", fresh); !errors.Is(err, domain.ErrInvalidCurrentPassword) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}

	if _, err := e.change(t, in.Session, "wrong password", fresh); !errors.Is(err, domain.ErrInvalidCurrentPassword) {
		t.Fatalf("5th attempt: %v", err)
	}

	var rl *domain.RateLimitError
	if _, err := e.change(t, in.Session, password, fresh); !errors.As(err, &rl) {
		t.Fatalf("throttled attempt: %v", err)
	}

	e.clock.Advance(time.Minute)

	if _, err := e.change(t, in.Session, password, fresh); err != nil {
		t.Fatalf("after the delay: %v", err)
	}

	stored, _ := e.users.ByID(ctx, u.ID())
	if stored.FailedLogins() != 0 {
		t.Errorf("failed logins = %d", stored.FailedLogins())
	}
}

func TestResetPasswordFromTheCLI(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	u := e.addUser(t, "alice", "", domain.RoleListener)

	in, _ := e.login("alice", password)

	for range 10 {
		_, _ = e.login("alice", "wrong password")
		e.clock.Advance(time.Hour)
	}

	res, err := e.admin.ResetPassword(ctx, "ALICE", "")
	if err != nil {
		t.Fatal(err)
	}

	if res.RevokedSessions != 1 || res.GeneratedPassword == "" || !res.User.MustChangePassword() {
		t.Errorf("result = %+v", res)
	}

	if _, err := e.auth.Resolve(ctx, in.Token.Cookie()); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("session survived: %v", err)
	}

	got, err := e.login("alice", res.GeneratedPassword)
	if err != nil || !got.Principal.MustChangePassword() {
		t.Fatalf("login with the generated password: %v", err)
	}

	chosen, err := e.admin.ResetPassword(ctx, "alice", fresh)
	if err != nil || chosen.GeneratedPassword != "" || chosen.User.MustChangePassword() {
		t.Errorf("chosen password: %v %+v", err, chosen)
	}

	if _, err := e.admin.ResetPassword(ctx, "nobody", fresh); !errors.Is(err, domain.ErrUserNotFound) {
		t.Errorf("unknown user: %v", err)
	}

	if _, err := e.admin.ResetPassword(ctx, "alice", "short"); !errors.Is(err, domain.ErrInvalidPassword) {
		t.Errorf("short: %v", err)
	}

	if !slices.Contains(e.actions(t), domain.ActionUserPasswordReset+":") {
		t.Errorf("audit = %v", e.actions(t))
	}

	stored, _ := e.users.ByID(ctx, u.ID())
	if stored.FailedLogins() != 0 {
		t.Errorf("failures = %d", stored.FailedLogins())
	}
}
