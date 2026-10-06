package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// An attacker who knew the password starts an e-mail change and asks for a
// reset link; the owner recovers the account: the attacker's links must be
// dead.
func TestRecoveryKillsPendingLinks(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	u := e.addUser(t, "alice", "alice@example.org", domain.RoleListener)
	n := &recNotifier{}
	profile := e.profile(n)
	resets := e.resets(n, nil)

	attacker := e.actor(t, "alice")
	if _, err := profile.ChangeEmail(ctx, attacker, "attacker@evil.example", password); err != nil {
		t.Fatal(err)
	}

	if err := resets.Request(ctx, "alice", app.RequestMeta{IP: ip}); err != nil {
		t.Fatal(err)
	}

	var emailToken, resetToken string

	for _, s := range n.all() {
		switch s.kind {
		case "email_confirmation":
			emailToken = strings.TrimPrefix(s.link, "https://hub.example/account/email/verify/")
		case "password_reset":
			resetToken = resetTokenOf(s.link)
		}
	}

	// The owner changes the password.
	owner, _ := e.login("alice", password)
	if _, err := e.passwords().Change(ctx, app.ChangePasswordInput{Session: owner.Session, Current: password, New: fresh, Meta: app.RequestMeta{IP: ip}}); err != nil {
		t.Fatal(err)
	}

	if err := profile.ConfirmEmail(ctx, emailToken, app.RequestMeta{}); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("e-mail change after the recovery: %v", err)
	}

	if err := resets.Confirm(ctx, resetToken, "attacker chose this", app.RequestMeta{}); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("reset after the recovery: %v", err)
	}

	stored, _ := e.users.ByID(ctx, u.ID())
	if stored.Email().String() != "alice@example.org" {
		t.Errorf("address = %v", stored.Email())
	}
}

func TestAccessChangesKillPendingLinks(t *testing.T) {
	ctx := context.Background()

	for name, change := range map[string]func(e *env, u *domain.User) error{
		"disable": func(e *env, u *domain.User) error {
			_, err := e.accounts(nil).SetEnabled(ctx, e.actor(t, "root"), u.ID(), false)

			return err
		},
		"generated password": func(e *env, u *domain.User) error {
			_, err := e.accounts(nil).SetGeneratedPassword(ctx, e.actor(t, "root"), u.ID())

			return err
		},
		"sign out everywhere": func(e *env, u *domain.User) error {
			_, err := e.accounts(nil).RevokeUserSessions(ctx, e.actor(t, "root"), u.ID())

			return err
		},
		"CLI reset": func(e *env, _ *domain.User) error {
			_, err := e.admin.ResetPassword(ctx, "alice", fresh)

			return err
		},
		"e-mail change": func(e *env, _ *domain.User) error {
			_, err := e.profile(&recNotifier{disabled: true}).ChangeEmail(ctx, e.actor(t, "alice"), "new@example.org", password)

			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.addUser(t, "root", "", domain.RoleAdmin)
			u := e.addUser(t, "alice", "alice@example.org", domain.RoleListener)
			n := &recNotifier{}
			resets := e.resets(n, nil)

			if err := resets.Request(ctx, "alice", app.RequestMeta{IP: ip}); err != nil {
				t.Fatal(err)
			}

			token := resetTokenOf(n.all()[0].link)

			if err := change(e, u); err != nil {
				t.Fatal(err)
			}

			if err := resets.Check(ctx, token, app.RequestMeta{}); !errors.Is(err, domain.ErrInvalidToken) {
				t.Errorf("reset link still valid: %v", err)
			}
		})
	}
}

func TestResetRefusesDisabledAccountsAndConfirmsOnlyTheMailedAddress(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	u := e.addUser(t, "alice", "alice@example.org", domain.RoleListener)
	r := e.resets(&recNotifier{disabled: true}, nil)

	// A link shown to an admin confirms nothing.
	res, err := r.IssueByAdmin(ctx, e.actor(t, "root"), u.ID())
	if err != nil || res.Link == "" {
		t.Fatalf("issue = %+v, %v", res, err)
	}

	if err := r.Confirm(ctx, resetTokenOf(res.Link), fresh, app.RequestMeta{}); err != nil {
		t.Fatal(err)
	}

	if stored, _ := e.users.ByID(ctx, u.ID()); !stored.EmailVerifiedAt().IsZero() {
		t.Error("address confirmed by a copied link")
	}

	res, _ = r.IssueByAdmin(ctx, e.actor(t, "root"), u.ID())

	if _, err := e.admin.Disable(ctx, "alice"); err != nil {
		t.Fatal(err)
	}

	if err := r.Confirm(ctx, resetTokenOf(res.Link), fresh+"x", app.RequestMeta{}); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("reset of a disabled account: %v", err)
	}
}

func resetTokenOf(link string) string {
	return strings.TrimPrefix(link, "https://hub.example/password/reset/")
}
