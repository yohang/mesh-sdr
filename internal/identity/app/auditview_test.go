package app_test

import (
	"context"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func TestAuditView(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "root", "", domain.RoleAdmin)
	u := e.addUser(t, "alice", "", domain.RoleListener)

	_, _ = e.login("alice", password)
	_, _ = e.login("root", password)

	if err := e.admin.Remove(ctx, "alice"); err != nil {
		t.Fatal(err)
	}

	v := app.NewAuditView(e.audit, e.users)

	rows, next, err := v.Search(ctx, app.AuditFilter{ActionPrefix: domain.ActionLoginSuccess})
	if err != nil || next != 0 || len(rows) != 2 {
		t.Fatalf("rows = %d, %v", len(rows), err)
	}

	if rows[0].ActorName != "root" || rows[0].TargetName != "root" {
		t.Errorf("newest = %+v", rows[0])
	}

	if want := "deleted user " + u.ID().String()[:8]; rows[1].ActorName != want || rows[1].TargetName != "" {
		t.Errorf("deleted actor = %q, want %q", rows[1].ActorName, want)
	}

	if rows, _, _ := v.Search(ctx, app.AuditFilter{Actor: "root"}); len(rows) != 1 {
		t.Errorf("by actor = %d", len(rows))
	}

	if rows, _, _ := v.Search(ctx, app.AuditFilter{Actor: "nobody"}); len(rows) != 0 {
		t.Errorf("unknown actor = %d", len(rows))
	}

	page, next, _ := v.Search(ctx, app.AuditFilter{Limit: 2})
	if len(page) != 2 || next == 0 {
		t.Fatalf("page = %d next %d", len(page), next)
	}

	n := 0
	if err := v.Each(ctx, app.AuditFilter{}, func(app.AuditRow) error { n++; return nil }); err != nil || n < 4 {
		t.Errorf("each = %d, %v", n, err)
	}
}
