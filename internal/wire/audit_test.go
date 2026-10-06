package wire

import (
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	identitysqlite "github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
)

// Grid audit records land in the identity audit_log.
func TestGridAuditorWritesAuditLog(t *testing.T) {
	a := dbtest.NewSQLite(t)
	ctx := context.Background()

	aud := newGridAuditor(a, time.Now, quiet)
	aud.Record(ctx, gridapp.AuditRecord{ActorKind: gridapp.ActorCLI, Action: "node.add", Target: "attic", Result: gridapp.ResultOK})
	aud.Record(ctx, gridapp.AuditRecord{ActorKind: gridapp.ActorSystem, Action: "node.enroll", Target: "attic", Result: gridapp.ResultDenied,
		Detail: map[string]string{"reason": "bad proof"}})

	entries, err := identitysqlite.NewAuditLog(a).Recent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}

	got := map[string]identitydomain.AuditEntry{}
	for _, e := range entries {
		got[e.Action()] = e
	}

	add, enroll := got["node.add"], got["node.enroll"]
	if add.Actor().Kind() != identitydomain.ActorCLI || add.TargetType() != "node" || add.TargetID() != "attic" {
		t.Errorf("node.add = %+v", add)
	}

	if enroll.Result() != identitydomain.ResultDenied || enroll.After()["reason"] != "bad proof" || enroll.Actor().Kind() != identitydomain.ActorSystem {
		t.Errorf("node.enroll = %+v", enroll)
	}
}
