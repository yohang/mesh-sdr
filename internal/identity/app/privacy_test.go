package app_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// eraser records erased users.
type eraser struct{ got []domain.UserID }

func (e *eraser) EraseUser(_ context.Context, id domain.UserID) error {
	e.got = append(e.got, id)

	return nil
}

func TestDeleteAccount(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	root := e.addUser(t, "root", "", domain.RoleAdmin)
	u := e.addUser(t, "alice", "alice@example.org", domain.RoleListener)
	rv := &recRevocations{}
	acc := e.accounts(rv)
	me := e.actor(t, "alice")

	exp, err := acc.ExportOwn(ctx, me)
	if err != nil || exp.Account.Username != "alice" || exp.Account.Email != "alice@example.org" || len(exp.Sessions) != 1 {
		t.Fatalf("export = %+v, %v", exp, err)
	}

	if err := acc.DeleteOwn(ctx, me, "wrong password"); !errors.Is(err, domain.ErrInvalidCurrentPassword) {
		t.Errorf("wrong password: %v", err)
	}

	if err := acc.DeleteOwn(ctx, me, password); err != nil {
		t.Fatal(err)
	}

	if _, err := e.users.ByID(ctx, u.ID()); !errors.Is(err, domain.ErrUserNotFound) {
		t.Error("user kept")
	}

	if len(rv.got) != 1 || rv.got[0].Users[0] != u.ID() {
		t.Errorf("revocations = %v", rv.got)
	}

	// The audit keeps the id, never the username.
	entries, _ := e.audit.Recent(ctx, 100)
	for _, en := range entries {
		for _, m := range []map[string]string{en.Before(), en.After()} {
			for _, v := range m {
				if strings.Contains(v, "alice") {
					t.Errorf("audit entry %s keeps %q", en.Action(), v)
				}
			}
		}
	}

	if entries[0].Action() != domain.ActionUserDelete || entries[0].TargetID() != u.ID().String() {
		t.Errorf("last entry = %s %s", entries[0].Action(), entries[0].TargetID())
	}

	if err := acc.Delete(ctx, e.actor(t, "root"), root.ID()); !errors.Is(err, domain.ErrLastAdmin) {
		t.Errorf("delete the last admin: %v", err)
	}
}

func TestDeleteRunsErasers(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	u := e.addUser(t, "alice", "", domain.RoleListener)
	er := &eraser{}

	acc := app.NewAccounts(app.AccountsDeps{
		Users: e.users, Sessions: e.sessions, Audit: e.audit, Tx: e.db, Now: e.clock.Now, Erasers: []app.UserEraser{er},
		Logger: discard(),
	})

	if err := acc.Delete(ctx, e.actor(t, "root"), u.ID()); err != nil || len(er.got) != 1 || er.got[0] != u.ID() {
		t.Errorf("erasers = %v, %v", er.got, err)
	}
}

func TestRemoveFromTheCLI(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	e.addUser(t, "alice", "", domain.RoleListener)

	if err := e.admin.Remove(ctx, "ALICE"); err != nil {
		t.Fatal(err)
	}

	if err := e.admin.Remove(ctx, "alice"); !errors.Is(err, domain.ErrUserNotFound) {
		t.Errorf("remove twice: %v", err)
	}

	if err := e.admin.Remove(ctx, "root"); !errors.Is(err, domain.ErrLastAdmin) {
		t.Errorf("remove the last admin: %v", err)
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
