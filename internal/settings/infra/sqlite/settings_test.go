package sqlite_test

import (
	"context"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	identitysqlite "github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/settings/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// open returns a fresh fixture: the repository and an existing user
// (settings.updated_by references users).
func open(t *testing.T) Fixture {
	a := dbtest.NewSQLite(t)

	id, err := shared.NewUUIDv7Generator().New(t0)
	if err != nil {
		t.Fatal(err)
	}

	uid, _ := identitydomain.NewUserID(id)
	name, _ := identitydomain.NewUsername("admin")
	display, _ := identitydomain.NewDisplayName("Admin")
	hash, _ := identitydomain.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA")

	u, err := identitydomain.NewLocalUser(identitydomain.NewLocalUserParams{
		ID: uid, Username: name, DisplayName: display, PasswordHash: hash, Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := identitysqlite.NewUsers(a, shared.NewUUIDv7Generator()).Add(context.Background(), u); err != nil {
		t.Fatal(err)
	}

	return Fixture{Settings: sqlite.NewSettings(a, 1), User: id, Tx: a.WithinTx}
}
