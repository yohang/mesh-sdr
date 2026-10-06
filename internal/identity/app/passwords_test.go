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

func (e *env) passwords() *app.Passwords { return e.passwordsWith(e.hasher) }

func (e *env) passwordsWith(h app.PasswordHasher) *app.Passwords {
	return app.NewPasswords(app.PasswordsDeps{
		Users: e.users, Sessions: e.sessions, Audit: e.audit, Tx: e.db, Hasher: h, IDs: shared.NewUUIDv7Generator(),
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

// hookHasher runs hook once, after the first verification: it interleaves
// another change between the check of the current password and the store.
type hookHasher struct {
	app.PasswordHasher

	hook func()
}

func (h *hookHasher) Verify(ctx context.Context, pw string, hash domain.PasswordHash) (bool, error) {
	ok, err := h.PasswordHasher.Verify(ctx, pw, hash)
	if h.hook != nil {
		hook := h.hook
		h.hook = nil
		hook()
	}

	return ok, err
}

func TestChangePasswordLosesToAConcurrentChange(t *testing.T) {
	ctx := context.Background()

	t.Run("reset and sessions revoked meanwhile", func(t *testing.T) {
		e := newEnv(t, nil)
		e.addUser(t, "alice", "", domain.RoleListener)
		in, _ := e.login("alice", password)

		h := &hookHasher{PasswordHasher: e.hasher, hook: func() {
			if _, err := e.admin.ResetPassword(ctx, "alice", "an admin chose this one"); err != nil {
				t.Error(err)
			}
		}}

		if _, err := e.passwordsWith(h).Change(ctx, app.ChangePasswordInput{Session: in.Session, Current: password, New: fresh}); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Fatalf("change = %v", err)
		}

		if _, err := e.login("alice", "an admin chose this one"); err != nil {
			t.Errorf("the reset was undone: %v", err)
		}
	})

	t.Run("hash replaced meanwhile", func(t *testing.T) {
		e := newEnv(t, nil)
		u := e.addUser(t, "bob", "", domain.RoleListener)
		in, _ := e.login("bob", password)

		h := &hookHasher{PasswordHasher: e.hasher, hook: func() {
			other, _ := e.hasher.Hash(ctx, "set by another request")
			stored, _ := e.users.ByID(ctx, u.ID())

			if err := stored.ChangePassword(other, e.clock.Now()); err != nil {
				t.Error(err)
			}

			if err := e.users.Save(ctx, stored); err != nil {
				t.Error(err)
			}
		}}

		if _, err := e.passwordsWith(h).Change(ctx, app.ChangePasswordInput{Session: in.Session, Current: password, New: fresh}); !errors.Is(err, domain.ErrInvalidCurrentPassword) {
			t.Fatalf("change = %v", err)
		}

		if _, err := e.login("bob", "set by another request"); err != nil {
			t.Errorf("the other change was undone: %v", err)
		}
	})
}

func TestPasswordsAreNormalisedToNFC(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)

	decomposed := "café au lait chaud"
	composed := "café au lait chaud"

	if _, err := e.admin.Add(ctx, app.AddUserInput{Username: "zoe", Password: decomposed}); err != nil {
		t.Fatal(err)
	}

	if _, err := e.login("zoe", composed); err != nil {
		t.Errorf("composed form refused: %v", err)
	}

	if _, err := e.login("zoe", decomposed); err != nil {
		t.Errorf("decomposed form refused: %v", err)
	}
}
