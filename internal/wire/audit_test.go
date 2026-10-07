package wire

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/http/clientip"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	identitysqlite "github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
)

// Audit records land in the identity audit_log with their actor.
func TestAuditAppenderWritesAuditLog(t *testing.T) {
	a := dbtest.NewSQLite(t)
	ctx := context.Background()

	aud := newAuditAppender(a, time.Now)
	mustAppend(t, aud, ctx, audit.Record{Actor: audit.CLI, Action: "node.add", TargetType: "node", TargetID: "attic", Result: audit.ResultOK})
	mustAppend(t, aud, ctx, audit.Record{Actor: audit.System, Action: "node.enroll", TargetType: "node", TargetID: "attic", Result: audit.ResultDenied,
		After: map[string]string{"reason": "bad proof"}})

	// A REST call records the client address.
	reqCtx := clientip.With(ctx, netip.MustParseAddr("192.0.2.7"))
	mustAppend(t, aud, reqCtx, audit.Record{Actor: audit.Caller, Action: "node.delete", TargetType: "node", TargetID: "attic", Result: audit.ResultOK})

	entries, err := identitysqlite.NewAuditLog(a).Recent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 3 {
		t.Fatalf("entries = %d", len(entries))
	}

	got := map[string]identitydomain.AuditEntry{}
	for _, e := range entries {
		got[e.Action()] = e
	}

	add, enroll := got["node.add"], got["node.enroll"]

	if del := got["node.delete"]; del.Actor().IP() != netip.MustParseAddr("192.0.2.7") {
		t.Errorf("node.delete actor = %+v", del.Actor())
	}
	if add.Actor().Kind() != identitydomain.ActorCLI || add.TargetType() != "node" || add.TargetID() != "attic" {
		t.Errorf("node.add = %+v", add)
	}

	if enroll.Result() != identitydomain.ResultDenied || enroll.After()["reason"] != "bad proof" || enroll.Actor().Kind() != identitydomain.ActorSystem {
		t.Errorf("node.enroll = %+v", enroll)
	}
}

func mustAppend(t *testing.T, a auditAppender, ctx context.Context, r audit.Record) {
	t.Helper()

	if err := a.Append(ctx, r); err != nil {
		t.Fatal(err)
	}
}
