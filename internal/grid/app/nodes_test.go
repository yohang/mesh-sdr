package app_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
)

var discard = slog.New(slog.DiscardHandler)

type auditSpy struct {
	mu      sync.Mutex
	records []audit.Record
}

func (a *auditSpy) Append(_ context.Context, r audit.Record) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.records = append(a.records, r)

	return nil
}

func (a *auditSpy) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]string, len(a.records))
	for i, r := range a.records {
		out[i] = r.Action + ":" + r.TargetID
	}

	return out
}

type fakeCA struct{}

func (fakeCA) Fingerprint() (string, error) { return "AA:BB", nil }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

type env struct {
	db    *db.DB
	nodes *sqlite.NodeRepository
	revs  *sqlite.RevocationRepository
	audit *auditSpy
	clock *clock
	svc   *app.Nodes
}

func newEnv(t *testing.T) *env {
	t.Helper()

	a := dbtest.NewSQLite(t)
	e := &env{
		db: a, nodes: sqlite.NewNodeRepository(a), revs: sqlite.NewRevocationRepository(a),
		audit: &auditSpy{}, clock: &clock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)},
	}
	e.svc = app.NewNodes(e.nodes, e.revs, a, e.audit, fakeCA{}, app.DefaultTimings(), e.clock.now, discard)

	return e
}

func token(t *testing.T, s string) *domain.EnrollmentToken {
	t.Helper()

	tok, err := domain.ParseEnrollmentToken(s)
	if err != nil {
		t.Fatal(err)
	}

	return &tok
}

func TestSyncConfig(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	attic := app.DeclaredNode{ID: domain.MustNodeID("attic"), Name: domain.MustNodeName("Attic"), URL: domain.MustNodeURL("https://10.0.0.1:8074"), Token: token(t, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")}
	garden := app.DeclaredNode{ID: domain.MustNodeID("garden"), Name: domain.MustNodeName("garden"), URL: domain.MustNodeURL("https://garden:8074")}

	if err := e.svc.SyncConfig(ctx, []app.DeclaredNode{attic, garden}); err != nil {
		t.Fatal(err)
	}

	n, err := e.nodes.Get(ctx, attic.ID)
	if err != nil {
		t.Fatal(err)
	}

	if n.Origin() != domain.OriginConfig || !n.HasEnrollmentKey() {
		t.Errorf("attic = %+v", n.Snapshot())
	}

	// Idempotent: a second sync changes nothing.
	if err := e.svc.SyncConfig(ctx, []app.DeclaredNode{attic, garden}); err != nil {
		t.Fatal(err)
	}

	if got := len(e.audit.actions()); got != 2 {
		t.Fatalf("audit = %v", e.audit.actions())
	}

	// garden leaves the config: it becomes an admin-managed node.
	attic.URL = domain.MustNodeURL("https://10.0.0.2:8074")
	if err := e.svc.SyncConfig(ctx, []app.DeclaredNode{attic}); err != nil {
		t.Fatal(err)
	}

	g, _ := e.nodes.Get(ctx, garden.ID)
	if g.Origin() != domain.OriginDB || len(g.LockedFields()) != 0 {
		t.Errorf("released garden = %+v", g.Snapshot())
	}

	a, _ := e.nodes.Get(ctx, attic.ID)
	if a.URL() != attic.URL {
		t.Errorf("attic url = %s", a.URL())
	}

	want := []string{"node.config.declare:attic", "node.config.declare:garden", "node.config.update:attic", "node.config.release:garden"}
	if got := e.audit.actions(); len(got) != len(want) {
		t.Fatalf("audit = %v, want %v", got, want)
	}
}
