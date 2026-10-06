package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestPresenceFromNodeEvents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := enrolledNode(t, e)
	c, tr := newControl(e, "1.0.0")
	repo := sqlite.NewConnectionRepository(e.db)
	p := app.NewPresence(repo, tr, app.DefaultTimings(), e.clock.now, discard)

	for _, typ := range []rxv1.MessageType{rxv1.TypeConnectionOpened, rxv1.TypeConnectionHeart, rxv1.TypeConnectionClosed} {
		c.Handle(typ, p.Handler())
	}

	c.OnBoot(p.NodeRestarted)

	boot := welcome(t, c, n.ID(), "1.0.0")
	cid1, _ := shared.NewUUIDv7(e.clock.now())
	cid2, _ := shared.NewUUIDv7(e.clock.now().Add(time.Millisecond))

	event := func(seq int64, typ rxv1.MessageType, cid shared.UUID, reason string) app.Event {
		raw, _ := json.Marshal(ctl.Connection{Seq: seq, CID: cid.String(), DeviceID: "hf", Demod: "usb", Reason: reason})

		return app.Event{Seq: seq, Type: typ, Payload: raw}
	}

	_, err := c.Apply(ctx, n.ID(), boot, false, []app.Event{
		event(1, rxv1.TypeConnectionOpened, cid1, ""),
		event(2, rxv1.TypeConnectionOpened, cid2, ""),
		event(3, rxv1.TypeConnectionClosed, cid2, "client"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if count, _ := p.Count(ctx); count != 1 {
		t.Fatalf("open = %d, want 1", count)
	}

	open, _ := p.List(ctx)
	if i := open[0].Info(); i.ID != cid1 || i.Kind != domain.ConnectionMedia || i.NodeID != "attic" || i.DeviceID != "hf" || i.Mode != "usb" {
		t.Errorf("connection = %+v", i)
	}

	// Liveness: a connection.heartbeat keeps the row alive; silence closes it.
	e.clock.advance(40 * time.Second)

	if _, err := c.Apply(ctx, n.ID(), boot, false, []app.Event{event(4, rxv1.TypeConnectionHeart, cid1, "")}); err != nil {
		t.Fatal(err)
	}

	e.clock.advance(40 * time.Second)
	p.Reap(ctx)

	if count, _ := p.Count(ctx); count != 1 {
		t.Fatalf("open after a heartbeat = %d, want 1", count)
	}

	e.clock.advance(10 * time.Second)
	p.Reap(ctx)

	got, _ := repo.Get(ctx, cid1)
	if at, reason, closed := got.Closed(); !closed || reason != domain.CloseHeartbeatTimeout || !at.Equal(got.LastHeartbeat()) {
		t.Errorf("stale row = %v %s %v", at, reason, closed)
	}
}

func TestPresenceLifecycleClosures(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := enrolledNode(t, e)
	c, tr := newControl(e, "1.0.0")
	repo := sqlite.NewConnectionRepository(e.db)
	p := app.NewPresence(repo, tr, app.DefaultTimings(), e.clock.now, discard)
	c.OnBoot(p.NodeRestarted)

	open := func(node string) shared.UUID {
		t.Helper()

		id, _ := shared.NewUUIDv7(time.Now())
		if err := p.Open(ctx, domain.ConnectionInfo{ID: id, Kind: domain.ConnectionMedia, IP: "::ffff:192.0.2.1", NodeID: node}); err != nil {
			t.Fatal(err)
		}

		return id
	}

	reasonOf := func(id shared.UUID) domain.CloseReason {
		got, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		_, r, _ := got.Closed()

		return r
	}

	// A node restart closes the rows of its previous boot.
	welcome(t, c, n.ID(), "1.0.0")

	a := open("attic")

	if got, _ := repo.Get(ctx, a); got.Info().IP != "192.0.2.1" {
		t.Errorf("ip = %q, want the unmapped IPv4", got.Info().IP)
	}

	welcome(t, c, n.ID(), "1.0.0")

	if r := reasonOf(a); r != domain.CloseNodeRestart {
		t.Errorf("after restart = %q", r)
	}

	// A node whose channel stays lost past the stale delay loses its rows.
	b := open("attic")

	c.Disconnected(ctx, n.ID(), errors.New("gone"))
	e.clock.advance(46 * time.Second)

	// The row's own heartbeat is fresh enough not to be stale... at first.
	if err := p.Heartbeat(ctx, []shared.UUID{b}); err != nil {
		t.Fatal(err)
	}

	p.Reap(ctx)

	if r := reasonOf(b); r != domain.CloseNodeLost {
		t.Errorf("after node lost = %q", r)
	}

	// Hub start closes everything left open.
	d := open("")

	if err := p.CloseAtStart(ctx); err != nil {
		t.Fatal(err)
	}

	if r := reasonOf(d); r != domain.CloseHubRestart {
		t.Errorf("after hub start = %q", r)
	}

	// Retention deletes old closed rows.
	e.clock.advance(31 * 24 * time.Hour)
	p.Reap(ctx)

	if _, err := repo.Get(ctx, d); !errors.Is(err, domain.ErrConnectionNotFound) {
		t.Errorf("retention kept the row: %v", err)
	}
}
