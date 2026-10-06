// Package repotest holds the contract suites of the grid repositories
// (ADR 0006). Every dialect adapter runs them against a fresh, migrated
// database.
package repotest

import (
	"context"
	"encoding/json"
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
	Caps        domain.CapabilityRepository
	Devices     domain.DeviceRepository
	Conns       domain.ConnectionRepository
}

// Factory returns the repositories on a fresh, migrated database.
type Factory func(t *testing.T) Repos

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// Run runs every grid contract suite.
func Run(t *testing.T, newRepos Factory) {
	t.Run("nodes", func(t *testing.T) { testNodes(t, newRepos(t)) })
	t.Run("revocations", func(t *testing.T) { testRevocations(t, newRepos(t)) })
	t.Run("cursors", func(t *testing.T) { testCursors(t, newRepos(t)) })
	t.Run("capabilities", func(t *testing.T) { testCapabilities(t, newRepos(t)) })
	t.Run("devices", func(t *testing.T) { testDevices(t, newRepos(t)) })
	t.Run("connections", func(t *testing.T) { testConnections(t, newRepos(t)) })
}

func testDevices(t *testing.T, r Repos) {
	ctx := context.Background()
	attic, garden := domain.MustNodeID("attic"), domain.MustNodeID("garden")

	for _, id := range []domain.NodeID{attic, garden} {
		if err := r.Nodes.Create(ctx, domain.NewNode(id, domain.MustNodeName("n"), domain.MustNodeURL("https://x:1"), t0)); err != nil {
			t.Fatal(err)
		}
	}

	spec := domain.DeviceSpec{ID: domain.MustDeviceID("hf"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2, SampleRates: []int64{48_000}}
	hf := must(domain.NewReportedDevice(attic, spec, 0, t0))

	if err := r.Devices.Save(ctx, hf); err != nil {
		t.Fatal(err)
	}

	// The row of another node is never overwritten.
	stolen := must(domain.NewReportedDevice(garden, spec, 3, t0))
	if err := r.Devices.Save(ctx, stolen); err != nil {
		t.Fatal(err)
	}

	got := must(r.Devices.Get(ctx, spec.ID))
	if got.Node() != attic || got.Type() != "rtl_sdr" || got.SampleRates()[0] != 48_000 || !got.Flags().Enabled {
		t.Errorf("device = %+v", got.Snapshot())
	}

	got.ApplyState(domain.StateRunning, "", nil, shared.UUID{}, t0)

	if err := r.Devices.Save(ctx, got); err != nil {
		t.Fatal(err)
	}

	if err := r.Devices.SetNodeOffline(ctx, attic); err != nil {
		t.Fatal(err)
	}

	if d := must(r.Devices.Get(ctx, spec.ID)); d.Online() {
		t.Error("device still online")
	}

	// Only a missing device is deleted.
	if deleted, err := r.Devices.DeleteMissing(ctx, spec.ID); err != nil || deleted {
		t.Errorf("delete a reported device = %v, %v", deleted, err)
	}

	missing := must(r.Devices.Get(ctx, spec.ID))
	missing.MarkUnavailable(t0)

	if err := r.Devices.Save(ctx, missing); err != nil {
		t.Fatal(err)
	}

	if since, ok := must(r.Devices.Get(ctx, spec.ID)).Missing(); !ok || !since.Equal(t0) {
		t.Errorf("missing = %v %v", since, ok)
	}

	if deleted, err := r.Devices.DeleteMissing(ctx, spec.ID); err != nil || !deleted {
		t.Errorf("delete a missing device = %v, %v", deleted, err)
	}

	if err := r.Devices.Save(ctx, hf); err != nil {
		t.Fatal(err)
	}

	if list := must(r.Devices.ListByNode(ctx, garden)); len(list) != 0 {
		t.Errorf("garden devices = %d", len(list))
	}

	if list := must(r.Devices.List(ctx)); len(list) != 1 {
		t.Errorf("devices = %d", len(list))
	}
}

func testCapabilities(t *testing.T, r Repos) {
	ctx := context.Background()
	id := domain.MustNodeID("attic")

	if err := r.Nodes.Create(ctx, domain.NewNode(id, domain.MustNodeName("a"), domain.MustNodeURL("https://x:1"), t0)); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Caps.Get(ctx, id); !errors.Is(err, domain.ErrCapabilitiesNotReported) {
		t.Fatalf("get before report = %v", err)
	}

	jt9 := must(domain.NewCapability("tool:jt9", true, domain.CapabilityOK, "2.6.1", json.RawMessage(`{"path":"/usr/bin/jt9"}`), ""))
	ft8 := must(domain.NewCapability("mode:ft8", true, domain.CapabilityOK, "", nil, ""))
	dream := must(domain.NewCapability("tool:dream", false, domain.CapabilityMissing, "", nil, "not found"))

	first := must(domain.NewCapabilityReport(id, "h1", "1.0.0", []string{"rx-ctl.v1"}, json.RawMessage(`{"os":"linux"}`),
		json.RawMessage(`{"product_version":"1.0.0"}`), t0, []domain.Capability{jt9, ft8, dream}))

	if err := r.Caps.Replace(ctx, first); err != nil {
		t.Fatal(err)
	}

	got, err := r.Caps.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	caps := got.Capabilities()
	if got.Hash() != "h1" || len(caps) != 3 || caps[0].Key() != "mode:ft8" || caps[2].Key() != "tool:jt9" ||
		caps[2].Version() != "2.6.1" || caps[1].Error() != "not found" || caps[1].Available() || !got.ReportedAt().Equal(t0) {
		t.Errorf("report = %+v %+v", got, caps)
	}

	// A new report replaces every row.
	second := must(domain.NewCapabilityReport(id, "h2", "1.1.0", []string{"rx-ctl.v1"}, json.RawMessage(`{}`),
		json.RawMessage(`{}`), t0.Add(time.Minute), []domain.Capability{jt9}))

	if err := r.Caps.Replace(ctx, second); err != nil {
		t.Fatal(err)
	}

	if got, _ := r.Caps.Get(ctx, id); got.Hash() != "h2" || len(got.Capabilities()) != 1 || got.ProductVersion() != "1.1.0" {
		t.Errorf("replaced report = %+v", got)
	}

	// Rows go away with their node.
	if err := r.Nodes.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Caps.Get(ctx, id); !errors.Is(err, domain.ErrCapabilitiesNotReported) {
		t.Errorf("report survived its node: %v", err)
	}
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

	key := must(domain.ParseEnrollmentToken("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")).Key()
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

	if err := g.CompleteEnrollment(key, g.URL(), cert, t0.Add(time.Minute)); err != nil {
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

	// SaveStatus writes only the status: the runtime columns stay.
	if err := repo.SaveStatus(ctx, g.ID(), domain.StatusOnline, ""); err != nil {
		t.Fatal(err)
	}

	if rt := must(repo.Get(ctx, g.ID())).Runtime(); rt.Status != domain.StatusOnline || rt.StatusHint != "" || rt.BootID != boot || rt.CPUCores != 4 {
		t.Errorf("runtime after SaveStatus = %+v", rt)
	}

	// Save never writes the status: an admin change from a stale copy keeps
	// the status recorded meanwhile.
	stale := must(repo.Get(ctx, g.ID()))
	sv := stale.Version()

	if err := repo.SaveStatus(ctx, g.ID(), domain.StatusDegraded, "clock_offset"); err != nil {
		t.Fatal(err)
	}

	disabled := true
	if err := stale.Update(domain.NodePatch{Disabled: &disabled}, sv, t0); err != nil {
		t.Fatal(err)
	}

	stale.SetStatus(domain.StatusOffline, "")

	if err := repo.Save(ctx, stale, sv); err != nil {
		t.Fatal(err)
	}

	if got := must(repo.Get(ctx, g.ID())); !got.Disabled() || got.Runtime().Status != domain.StatusDegraded || got.Runtime().StatusHint != "clock_offset" {
		t.Errorf("after an admin save: disabled %v, status %s %q", got.Disabled(), got.Runtime().Status, got.Runtime().StatusHint)
	}

	if err := repo.SaveStatus(ctx, domain.MustNodeID("nope"), domain.StatusOnline, ""); !errors.Is(err, domain.ErrNodeNotFound) {
		t.Errorf("status of an unknown node = %v", err)
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
