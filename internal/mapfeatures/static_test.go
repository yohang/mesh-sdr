package mapfeatures

import (
	"context"
	"slices"
	"testing"
)

// SyncStatic replaces the static markers (MAP-017): permanent, without a
// device, listed to a visitor who may listen to no device.
func TestSyncStatic(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	err := e.m.SyncStatic(ctx, []Static{
		{File: "club", ID: "a", Name: "Club", Lat: 50, Lon: 3, Comment: "Thursdays", URL: "https://example.org/"},
		{File: "club", ID: "b", Name: "Beacon", Lat: -10, Lon: 179},
	})
	if err != nil {
		t.Fatal(err)
	}

	f, ok := e.get(t, "static:club:a")
	if !ok || !Public(f) || f.Subject != "club:a" || !f.ExpiresAt.IsZero() || f.Details["label"] != "Club" ||
		f.Details["comment"] != "Thursdays" || f.Details["url"] != "https://example.org/" || f.Geometry.Type != GeometryPoint {
		t.Errorf("static marker %+v", f)
	}

	if got := describe(e.take()); !slices.Equal(got, []string{"upsert static:club:a", "upsert static:club:b"}) {
		t.Errorf("published %v", got)
	}

	if err := e.m.SyncStatic(ctx, []Static{{File: "club", ID: "b", Name: "Beacon", Lat: -10, Lon: 179}}); err != nil {
		t.Fatal(err)
	}

	if _, ok := e.get(t, "static:club:a"); ok {
		t.Error("a marker gone from the files is kept")
	}

	if got := describe(e.take()); !slices.Equal(got, []string{"upsert static:club:b", "remove static:club:a deleted "}) {
		t.Errorf("published %v", got)
	}

	e.visible = nil

	list, _, err := e.m.Features(ctx)
	if err != nil || len(list) != 1 || list[0].Key != "static:club:b" {
		t.Errorf("features without a device %+v, %v", list, err)
	}
}
