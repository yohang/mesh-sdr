package wire

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/http/clientip"
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

	// A REST call records the client address.
	reqCtx := clientip.With(ctx, netip.MustParseAddr("192.0.2.7"))
	aud.Record(reqCtx, gridapp.AuditRecord{ActorKind: gridapp.ActorUser, Action: "node.delete", Target: "attic", Result: gridapp.ResultOK})

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
