// Package repotest holds the contract suites of the grid repositories
// (ADR 0006). Every dialect adapter runs them against a fresh, migrated
// database.
package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Repos is the set of grid repositories of one dialect, on one database.
type Repos struct {
	Nodes       domain.NodeRepository
	Revocations domain.RevocationRepository
	Cursors     domain.EventCursorRepository
}

// Factory returns the repositories on a fresh, migrated database.
type Factory func(t *testing.T) Repos

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// Run runs every grid contract suite.
func Run(t *testing.T, newRepos Factory) {
	t.Run("nodes", func(t *testing.T) { testNodes(t, newRepos(t)) })
	t.Run("revocations", func(t *testing.T) { testRevocations(t, newRepos(t)) })
	t.Run("cursors", func(t *testing.T) { testCursors(t, newRepos(t)) })
}

func testCursors(t *testing.T, r Repos) {
	ctx := context.Background()
	id := domain.MustNodeID("attic")

	if err := r.Nodes.Create(ctx, domain.NewNode(id, domain.MustNodeName("a"), domain.MustNodeURL("https://x:1"), t0)); err != nil {
		t.Fatal(err)
	}

	b1 := must(shared.NewUUIDv7(t0))
	b2 := must(shared.NewUUIDv7(t0.Add(time.Second)))

	if seq, err := r.Cursors.Last(ctx, id, b1); err != nil || seq != 0 {
		t.Fatalf("empty cursor = %d, %v", seq, err)
	}

	for _, c := range []struct {
		boot shared.UUID
		seq  int64
	}{{b1, 5}, {b1, 9}, {b2, 2}} {
		if err := r.Cursors.Advance(ctx, id, c.boot, c.seq, t0); err != nil {
			t.Fatal(err)
		}
	}

	if seq, _ := r.Cursors.Last(ctx, id, b1); seq != 9 {
		t.Errorf("b1 = %d", seq)
	}

	if err := r.Cursors.Forget(ctx, id, b2); err != nil {
		t.Fatal(err)
	}

	if seq, _ := r.Cursors.Last(ctx, id, b1); seq != 0 {
		t.Errorf("forgotten b1 = %d", seq)
	}

	if seq, _ := r.Cursors.Last(ctx, id, b2); seq != 2 {
		t.Errorf("b2 = %d", seq)
	}

	// Cursors go away with their node.
	if err := r.Nodes.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}

	if seq, _ := r.Cursors.Last(ctx, id, b2); seq != 0 {
		t.Errorf("cursor survived its node: %d", seq)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}

	return v
}

func testNodes(t *testing.T, r Repos) {
	ctx := context.Background()
	repo := r.Nodes

	key := must(domain.ParseEnrollmentToken("abcdefghijklmnopqrstuvwxyz")).Key()
	cfgNode := domain.NewConfigNode(domain.MustNodeID("attic"), domain.MustNodeName("Attic"), domain.MustNodeURL("https://10.0.0.1:8074"), &key, t0)
	dbNode := domain.NewNode(domain.MustNodeID("garden"), domain.MustNodeName("Garden"), domain.MustNodeURL("https://garden.example.org:8074"), t0)
	dbNode.IssueEnrollmentKey(key, t0.Add(time.Hour), t0)

	for _, n := range []*domain.Node{dbNode, cfgNode} {
		if err := repo.Create(ctx, n); err != nil {
			t.Fatalf("create %s: %v", n.ID(), err)
		}
	}

	if err := repo.Create(ctx, cfgNode); !errors.Is(err, domain.ErrNodeExists) {
		t.Fatalf("duplicate create = %v, want node_exists", err)
	}

	got, err := repo.Get(ctx, cfgNode.ID())
	if err != nil {
		t.Fatal(err)
	}

	if got.Origin() != domain.OriginConfig || len(got.LockedFields()) != 2 || got.Name().String() != "Attic" {
		t.Errorf("config node = %+v", got.Snapshot())
	}

	if k, exp, ok := got.StoredEnrollmentKey(); !ok || k != key || !exp.IsZero() {
		t.Errorf("config key = %v %v %v", ok, exp, k == key)
	}

	g, err := repo.Get(ctx, dbNode.ID())
	if err != nil {
		t.Fatal(err)
	}

	if _, exp, _ := g.StoredEnrollmentKey(); !exp.Equal(t0.Add(time.Hour)) {
		t.Errorf("expiry = %v", exp)
	}

	if _, err := repo.Get(ctx, domain.MustNodeID("nope")); !errors.Is(err, domain.ErrNodeNotFound) {
		t.Errorf("get unknown = %v", err)
	}

	list, err := repo.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID().String() != "attic" {
		t.Fatalf("list = %v, %v", list, err)
	}

	// Enrollment and optimistic concurrency.
	cert := must(domain.NewCertInfo(make([]byte, 32), "0A1B", t0.Add(90*24*time.Hour)))
	v := g.Version()

	if err := g.CompleteEnrollment(cert, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	if err := repo.Save(ctx, g, v); err != nil {
		t.Fatal(err)
	}

	if err := repo.Save(ctx, g, v); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("stale save = %v, want version_conflict", err)
	}

	g2, _ := repo.Get(ctx, g.ID())
	if g2.Enrollment() != domain.EnrollmentEnrolled || g2.Certificate() != cert || g2.HasEnrollmentKey() || g2.Version() != v+1 {
		t.Errorf("enrolled node = %+v", g2.Snapshot())
	}

	// Runtime columns.
	boot := must(shared.NewUUIDv7(t0))
	g2.RecordWelcome(boot, "1.2.3", "rx-ctl.v1")
	g2.RecordHeartbeat(t0.Add(2*time.Minute), -42, "host", 4)
	g2.SetStatus(domain.StatusDegraded, "clock_offset")

	if err := repo.SaveRuntime(ctx, g2); err != nil {
		t.Fatal(err)
	}

	g3, _ := repo.Get(ctx, g.ID())

	rt := g3.Runtime()
	if rt.Status != domain.StatusDegraded || rt.StatusHint != "clock_offset" || rt.BootID != boot || rt.ClockOffsetMS == nil ||
		*rt.ClockOffsetMS != -42 || rt.CPUCores != 4 || !rt.LastHeartbeatAt.Equal(t0.Add(2*time.Minute)) || g3.Version() != g2.Version() {
		t.Errorf("runtime = %+v", rt)
	}

	if err := repo.Delete(ctx, g.ID()); err != nil {
		t.Fatal(err)
	}

	if err := repo.Delete(ctx, g.ID()); !errors.Is(err, domain.ErrNodeNotFound) {
		t.Errorf("second delete = %v", err)
	}
}

func testRevocations(t *testing.T, r Repos) {
	ctx := context.Background()
	id := domain.MustNodeID("attic")

	expired := must(domain.NewRevokedCertificate(must(domain.NewCertInfo(make([]byte, 32), "01", t0.Add(-time.Hour))), id, "revoked", t0))
	live := must(domain.NewRevokedCertificate(must(domain.NewCertInfo(make([]byte, 32), "02", t0.Add(time.Hour))), id, "deleted", t0))

	for _, c := range []domain.RevokedCertificate{expired, live, live} {
		if err := r.Revocations.Add(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	list, err := r.Revocations.List(ctx, t0)
	if err != nil || len(list) != 1 || list[0].Serial() != "02" || list[0].Reason() != "deleted" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}
