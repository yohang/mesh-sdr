package sqlite_test

import (
	"context"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/identity/infra/repotest"
	identitysqlite "github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	settingsrepotest "github.com/yohang/mesh-sdr/internal/settings/infra/repotest"
	"github.com/yohang/mesh-sdr/internal/settings/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func open(t *testing.T) settingsrepotest.Fixture {
	a := dbtest.NewSQLite(t)
	u := repotest.NewUser(t, "admin", "")

	if err := identitysqlite.NewUsers(a, shared.NewUUIDv7Generator()).Add(context.Background(), u); err != nil {
		t.Fatal(err)
	}

	id, err := shared.UUIDFromBytes(u.ID().Bytes())
	if err != nil {
		t.Fatal(err)
	}

	return settingsrepotest.Fixture{Settings: sqlite.NewSettings(a, 1), User: id, Tx: a.WithinTx}
}

func TestSettings(t *testing.T) { settingsrepotest.Run(t, open) }
