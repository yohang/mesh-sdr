package sqlite_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/identity/infra/repotest"
	"github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func open(t *testing.T) repotest.Repos {
	a := dbtest.NewSQLite(t)

	return repotest.Repos{
		Users:        sqlite.NewUsers(a, shared.NewUUIDv7Generator()),
		Sessions:     sqlite.NewSessions(a),
		Audit:        sqlite.NewAuditLog(a),
		Invitations:  sqlite.NewInvitations(a),
		Resets:       sqlite.NewPasswordResets(a),
		EmailChanges: sqlite.NewEmailChanges(a),
	}
}

func TestAccounts(t *testing.T) { repotest.RunAccounts(t, open) }
func TestUsers(t *testing.T)    { repotest.RunUsers(t, open) }
func TestSessions(t *testing.T) { repotest.RunSessions(t, open) }
func TestAudit(t *testing.T)    { repotest.RunAudit(t, open) }

func TestAuditLogIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)

	e, _ := domain.NewAuditEntry(t0(), domain.SystemActor(), "test.event", domain.ResultOK)
	if err := sqlite.NewAuditLog(a).Append(ctx, e); err != nil {
		t.Fatal(err)
	}

	_, err := a.Writer(ctx).ExecContext(ctx, "UPDATE audit_log SET result = 'error'")
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("update of audit_log: %v", err)
	}
}

func TestMalformedHashKeepsUserLoadable(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	users := sqlite.NewUsers(a, shared.NewUUIDv7Generator())
	u := repotest.NewUser(t, "alice", "")

	if err := users.Add(ctx, u); err != nil {
		t.Fatal(err)
	}

	if _, err := a.Writer(ctx).ExecContext(ctx, "UPDATE users SET password_hash = 'garbage'"); err != nil {
		t.Fatal(err)
	}

	got, err := users.ByID(ctx, u.ID())
	if err != nil {
		t.Fatal(err)
	}

	if got.CanPasswordLogin() {
		t.Error("an account with a malformed hash can log in with a password")
	}
}

func TestRolesAreSeeded(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)

	rows, err := a.Reader(ctx).QueryContext(ctx, "SELECT id, name FROM roles ORDER BY rank")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = rows.Close() }()

	var got []string

	for rows.Next() {
		var (
			id   int64
			name string
		)

		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}

		r, err := domain.RoleFromID(id)
		if err != nil || r.String() != name {
			t.Errorf("role %d %s does not match the domain", id, name)
		}

		got = append(got, name)
	}

	if strings.Join(got, ",") != "anonymous,listener,operator,admin" {
		t.Errorf("roles = %v", got)
	}
}

func t0() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }
