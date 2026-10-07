package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func testConnections(t *testing.T, r Repos) {
	ctx := context.Background()
	repo := r.Conns

	open := func(node string, at time.Time) shared.UUID {
		t.Helper()

		id := must(shared.NewUUIDv7(at))
		c := must(domain.NewConnection(domain.ConnectionInfo{ID: id, Kind: domain.ConnectionMedia, IP: "192.0.2.1", NodeID: node, DeviceID: "hf"}, at))

		if ok, err := repo.Open(ctx, c); err != nil || !ok {
			t.Fatalf("open: %v %v", ok, err)
		}

		if ok, _ := repo.Open(ctx, c); ok {
			t.Fatal("second open of the same id inserted a row")
		}

		return id
	}

	a := open("attic", t0)
	b := open("attic", t0)
	c := open("garden", t0)

	// The gateway authz marks the rows it issues.
	issued := must(shared.NewUUIDv7(t0))
	if _, err := repo.Open(ctx, must(domain.NewConnection(domain.ConnectionInfo{
		ID: issued, Kind: domain.ConnectionMedia, IP: "192.0.2.1", NodeID: "cellar", HubIssued: true,
	}, t0))); err != nil {
		t.Fatal(err)
	}

	if !must(repo.Get(ctx, issued)).Info().HubIssued || must(repo.Get(ctx, a)).Info().HubIssued {
		t.Error("hub_issued not stored")
	}

	// An events socket is an open row, not a listener.
	viewer := must(domain.NewConnection(domain.ConnectionInfo{ID: must(shared.NewUUIDv7(t0)), Kind: domain.ConnectionEvents, IP: "192.0.2.9"}, t0))
	if _, err := repo.Open(ctx, viewer); err != nil {
		t.Fatal(err)
	}

	if n := must(repo.CountOpenKind(ctx, domain.ConnectionMedia)); n != 4 {
		t.Errorf("open media = %d, want 4", n)
	}

	if n := must(repo.CountOpenKind(ctx, domain.ConnectionEvents)); n != 1 {
		t.Errorf("open events = %d, want 1", n)
	}

	viewer.Close(domain.CloseClient, t0.Add(3*time.Minute))

	if err := repo.Save(ctx, viewer); err != nil {
		t.Fatal(err)
	}

	if n, err := repo.CloseNode(ctx, domain.MustNodeID("cellar"), domain.CloseNodeLost, t0); err != nil || n != 1 {
		t.Fatalf("close node = %d, %v", n, err)
	}

	if n, err := repo.Heartbeat(ctx, []shared.UUID{a, b}, t0.Add(time.Minute)); err != nil || n != 2 {
		t.Fatalf("heartbeat = %d, %v", n, err)
	}

	if n, err := repo.CloseStale(ctx, t0.Add(30*time.Second)); err != nil || n != 1 {
		t.Fatalf("close stale = %d, %v", n, err)
	}

	got := must(repo.Get(ctx, c))
	if at, reason, closed := got.Closed(); !closed || reason != domain.CloseHeartbeatTimeout || !at.Equal(t0) {
		t.Errorf("stale row = %v %s", at, reason)
	}

	nodes := must(repo.OpenNodes(ctx))
	if len(nodes) != 1 || nodes[0].String() != "attic" {
		t.Errorf("open nodes = %v", nodes)
	}

	if n, err := repo.CloseNode(ctx, domain.MustNodeID("attic"), domain.CloseNodeLost, t0.Add(2*time.Minute)); err != nil || n != 2 {
		t.Fatalf("close node = %d, %v", n, err)
	}

	if n := must(repo.CountOpen(ctx)); n != 0 {
		t.Errorf("open = %d", n)
	}

	d := open("", t0.Add(3*time.Minute))

	if n, err := repo.CloseAll(ctx, domain.CloseHubRestart, t0.Add(4*time.Minute)); err != nil || n != 1 {
		t.Fatalf("close all = %d, %v", n, err)
	}

	if list := must(repo.ListOpen(ctx)); len(list) != 0 {
		t.Errorf("open list = %d", len(list))
	}

	if n, err := repo.DeleteClosedBefore(ctx, t0.Add(3*time.Minute)); err != nil || n != 4 {
		t.Fatalf("retention = %d, %v", n, err)
	}

	if _, err := repo.Get(ctx, d); err != nil {
		t.Errorf("recent closed row deleted: %v", err)
	}
}
