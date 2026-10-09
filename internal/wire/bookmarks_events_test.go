package wire

import (
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/bookmarks"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// TestBookmarkChangedEvent: bookmark.changed goes on the devices topic; a
// bookmark scoped to a registered-only device does not reach anonymous
// viewers.
func TestBookmarkChangedEvent(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	now := time.Now()
	nodes, devices := gridsqlite.NewNodeRepository(a), gridsqlite.NewDeviceRepository(a)

	if err := nodes.Create(ctx, domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("Attic"), domain.MustNodeURL("https://attic:8074"), now)); err != nil {
		t.Fatal(err)
	}

	d, err := domain.NewReportedDevice(domain.MustNodeID("attic"), domain.DeviceSpec{
		ID: shared.MustDeviceID("members"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2, SampleRates: []int64{1},
		ListenPolicy: "registered",
	}, 0, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := devices.Save(ctx, d); err != nil {
		t.Fatal(err)
	}

	rec := &recordedEvents{}
	cache := gridapp.NewListenPolicies(devices, &fixedGlobal{v: "anonymous"}, quiet)
	publish := bookmarkChanged(rec, cache, quiet)

	scoped, err := bookmarks.OnDevice(shared.MustDeviceID("members"))
	if err != nil {
		t.Fatal(err)
	}

	id := shared.MustParseUUID("01900000-0000-7000-8000-000000000001")

	for _, scope := range []bookmarks.Scope{bookmarks.AllDevices(), scoped} {
		b, err := bookmarks.NewBookmark(id, bookmarks.Draft{Name: "Net", Frequency: 7_100_000, Modulation: "lsb", Scope: scope}, shared.UUID{}, now)
		if err != nil {
			t.Fatal(err)
		}

		publish(ctx, bookmarks.Change{Op: bookmarks.OpUpsert, Bookmark: b})
	}

	got := rec.take()
	if len(got) != 2 {
		t.Fatalf("%d events", len(got))
	}

	anon, user := events.Viewer{}, events.Viewer{UserID: "u"}
	sees := func(ev events.Event, v events.Viewer) bool { return ev.Audience == nil || ev.Audience(v) }

	for i, ev := range got {
		p, ok := ev.Payload.(bookmarkChangedEvent)
		if ev.Topic != topicDevices || ev.Type != rxv1.TypeBookmarkChanged.String() || !ok || p.Op != "upsert" || p.Bookmark.Name != "Net" {
			t.Fatalf("event %d = %+v", i, ev)
		}

		if !sees(ev, user) || sees(ev, anon) != (i == 0) {
			t.Errorf("event %d (scope %s): anonymous %v, user %v", i, p.Bookmark.Scope.Kind, sees(ev, anon), sees(ev, user))
		}
	}
}
