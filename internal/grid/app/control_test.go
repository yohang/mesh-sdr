package app_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func enrolledNode(t *testing.T, e *env) *domain.Node {
	t.Helper()

	tok, _ := domain.NewEnrollmentToken()
	n := domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("Attic"), domain.MustNodeURL("https://x:1"), e.clock.now())
	n.IssueEnrollmentKey(tok.Key(), time.Time{}, e.clock.now())

	cert, _ := domain.NewCertInfo(make([]byte, 32), "0A", e.clock.now().Add(90*24*time.Hour))
	if err := n.CompleteEnrollment(tok.Key(), n.URL(), cert, e.clock.now()); err != nil {
		t.Fatal(err)
	}

	if err := e.nodes.Create(context.Background(), n); err != nil {
		t.Fatal(err)
	}

	return n
}

func newControl(e *env, hubVersion string) (*app.Control, *app.Tracker) {
	tr := app.NewTracker()

	return app.NewControl(e.nodes, e.revs, sqlite.NewCursorRepository(e.db), e.db, e.audit, tr, hubVersion, e.clock.now, discard), tr
}

func TestControlIdempotentIngestion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := enrolledNode(t, e)
	c, tr := newControl(e, "1.4.0")

	var applied []int64

	c.Handle(rxv1.TypeConnectionOpened, func(_ context.Context, _ *domain.Node, ev app.Event, _ time.Time) error {
		applied = append(applied, ev.Seq)

		return nil
	})

	boot, _ := shared.NewUUIDv7(e.clock.now())

	d, err := c.Welcome(ctx, n.ID(), ctl.Welcome{NodeID: "attic", BootID: boot.String(), Version: "1.4.1", Protocols: []string{"rx-ctl.v1"}})
	if err != nil || d.Restricted || d.Compat.Level != domain.CompatOK {
		t.Fatalf("welcome = %+v, %v", d, err)
	}

	if s, _ := tr.State(n.ID()); !s.Connected || s.Boot != boot {
		t.Errorf("tracker = %+v", s)
	}

	ev := func(seq int64) app.Event {
		return app.Event{Seq: seq, Type: rxv1.TypeConnectionOpened, Payload: json.RawMessage(`{"seq":1}`)}
	}

	upto, err := c.Apply(ctx, n.ID(), boot, false, []app.Event{ev(1), ev(2), ev(3)})
	if err != nil || upto != 3 {
		t.Fatalf("apply = %d, %v", upto, err)
	}

	// A replay after a reconnect is acknowledged but not applied twice.
	upto, err = c.Apply(ctx, n.ID(), boot, false, []app.Event{ev(2), ev(3), ev(4)})
	if err != nil || upto != 4 {
		t.Fatalf("replay = %d, %v", upto, err)
	}

	if len(applied) != 4 || applied[3] != 4 {
		t.Errorf("applied = %v, want [1 2 3 4]", applied)
	}

	// Unknown catalogued events are acknowledged and ignored.
	if upto, err := c.Apply(ctx, n.ID(), boot, false, []app.Event{{Seq: 5, Type: rxv1.TypeDecodeBatch, Payload: json.RawMessage(`{}`)}}); err != nil || upto != 5 {
		t.Errorf("unknown = %d, %v", upto, err)
	}
}

func TestControlIncompatibleNodeIsRestricted(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := enrolledNode(t, e)
	c, _ := newControl(e, "2.0.0")

	called := false

	c.Handle(rxv1.TypeConnectionOpened, func(context.Context, *domain.Node, app.Event, time.Time) error {
		called = true

		return nil
	})

	boot, _ := shared.NewUUIDv7(e.clock.now())

	d, err := c.Welcome(ctx, n.ID(), ctl.Welcome{NodeID: "attic", BootID: boot.String(), Version: "1.9.0", Protocols: []string{"rx-ctl.v1"}})
	if err != nil || !d.Restricted || d.Compat.Hint != domain.HintNodeIncompatible {
		t.Fatalf("welcome = %+v, %v", d, err)
	}

	if _, err := c.Apply(ctx, n.ID(), boot, d.Restricted, []app.Event{{Seq: 1, Type: rxv1.TypeConnectionOpened, Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}

	if called {
		t.Error("an incompatible node's connection event was applied")
	}
}

func TestControlNewBootRunsBootHandlers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := enrolledNode(t, e)
	c, _ := newControl(e, "1.0.0")

	boots := 0

	c.OnBoot(func(context.Context, domain.NodeID, time.Time) error {
		boots++

		return nil
	})

	b1, _ := shared.NewUUIDv7(e.clock.now())
	b2, _ := shared.NewUUIDv7(e.clock.now().Add(time.Second))

	for _, b := range []shared.UUID{b1, b1, b2} {
		if _, err := c.Welcome(ctx, n.ID(), ctl.Welcome{NodeID: "attic", BootID: b.String(), Version: "1.0.0", Protocols: []string{"rx-ctl.v1"}}); err != nil {
			t.Fatal(err)
		}
	}

	if boots != 2 {
		t.Errorf("boot handlers ran %d times, want 2 (first boot, then the restart)", boots)
	}
}

func welcome(t *testing.T, c *app.Control, id domain.NodeID, version string) shared.UUID {
	t.Helper()

	boot, _ := shared.NewUUIDv7(time.Now())

	if _, err := c.Welcome(context.Background(), id, ctl.Welcome{NodeID: id.String(), BootID: boot.String(), Version: version, Protocols: []string{"rx-ctl.v1"}}); err != nil {
		t.Fatal(err)
	}

	return boot
}

// A renewed certificate is accepted as soon as it is proposed, and the old
// one is revoked when it is promoted.
func TestCertificateRenewalPin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := enrolledNode(t, e)
	c, _ := newControl(e, "1.0.0")

	old := n.Certificate()
	fp := [32]byte{1}
	renewed, _ := domain.NewCertInfo(fp[:], "0B", e.clock.now().Add(90*24*time.Hour))

	if err := c.ProposeRenewal(ctx, n.ID(), renewed); err != nil {
		t.Fatal(err)
	}

	got, _ := e.nodes.Get(ctx, n.ID())
	if !got.AcceptsFingerprint(old.Fingerprint()) || !got.AcceptsFingerprint(fp) || got.AcceptsFingerprint([32]byte{2}) {
		t.Fatalf("pin during renewal: %+v", got.Snapshot())
	}

	if err := c.RecordRenewal(ctx, n.ID(), "0C"); err == nil {
		t.Error("promotion of an unknown serial accepted")
	}

	if err := c.RecordRenewal(ctx, n.ID(), "0B"); err != nil {
		t.Fatal(err)
	}

	got, _ = e.nodes.Get(ctx, n.ID())
	if got.Certificate() != renewed || !got.PendingCertificate().IsZero() || got.AcceptsFingerprint(old.Fingerprint()) {
		t.Errorf("after promotion: %+v", got.Snapshot())
	}

	if revoked, _ := e.revs.List(ctx, e.clock.now()); len(revoked) != 1 || revoked[0].Serial() != old.Serial() || revoked[0].Reason() != "renewed" {
		t.Errorf("revocations = %+v", revoked)
	}
}
