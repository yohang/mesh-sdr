package bookmarks

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

type fakeDevices []Device

func (f fakeDevices) Devices(context.Context) ([]Device, error) { return f, nil }

type fakePresets []Preset

func (f fakePresets) Presets(context.Context) ([]Preset, error) { return f, nil }

type signedInKey struct{}

// signedIn returns a context of a signed-in caller.
func signedIn() context.Context { return context.WithValue(context.Background(), signedInKey{}, true) }

var (
	presetA = shared.MustParseUUID("01900000-0000-7000-8000-00000000000a")
	presetB = shared.MustParseUUID("01900000-0000-7000-8000-00000000000b")
)

// env is a module on a migrated database with two devices: hf
// (anonymous, running preset A, analog modes) and vhf (registered, no
// mode reported).
type env struct {
	m       *Module
	records *audit.Records
	changes []Change
	region  string
}

func newEnv(t *testing.T) *env {
	t.Helper()

	e := &env{records: &audit.Records{}, region: "r1"}

	m, err := New(Deps{
		DB: dbtest.NewSQLite(t), Audit: e.records,
		Devices: fakeDevices{
			{ID: shared.MustDeviceID("hf"), Name: "HF", ListenPolicy: ListenAnonymous, ActivePreset: presetA, Modes: []string{"am", "usb", "lsb", "cw", "ft8"}},
			{ID: shared.MustDeviceID("vhf"), Name: "VHF", ListenPolicy: ListenRegistered},
		},
		Presets:  fakePresets{{ID: presetA, Name: "40 m"}, {ID: presetB, Name: "2 m"}},
		Region:   func() string { return e.region },
		SignedIn: func(ctx context.Context) bool { return ctx.Value(signedInKey{}) != nil },
		Changed:  func(_ context.Context, c Change) { e.changes = append(e.changes, c) },
		Now:      func() time.Time { return t0 },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	e.m = m

	return e
}

func (e *env) create(t *testing.T, d Draft) *Bookmark {
	t.Helper()

	b, err := e.m.Create(context.Background(), d)
	if err != nil {
		t.Fatalf("create %+v: %v", d, err)
	}

	return b
}

func onDevice(t *testing.T, id string) Scope {
	t.Helper()

	s, err := OnDevice(shared.MustDeviceID(id))
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func onPreset(t *testing.T, id shared.UUID) Scope {
	t.Helper()

	s, err := OnPreset(id)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func nameList(list []*Bookmark) []string {
	out := make([]string, len(list))
	for i, b := range list {
		out[i] = b.Name()
	}

	return out
}

// violations returns the paths of the field errors of an invalid bookmark.
func violations(err error) []string {
	var de *shared.Error
	if !errors.Is(err, ErrInvalidBookmark) || !errors.As(err, &de) {
		return nil
	}

	var out []string
	for _, v := range de.Violations() {
		out = append(out, v.Path())
	}

	return out
}

func TestValidation(t *testing.T) {
	e := newEnv(t)
	ok := Draft{Name: "Net", Frequency: 7_100_000, Modulation: "lsb", Scope: AllDevices()}

	for _, tt := range []struct {
		name string
		edit func(d *Draft)
		want []string
	}{
		{"empty name", func(d *Draft) { d.Name = "  " }, []string{"name"}},
		{"long name", func(d *Draft) { d.Name = string(make([]rune, 129)) }, []string{"name"}},
		{"zero frequency", func(d *Draft) { d.Frequency = 0 }, []string{"frequency"}},
		{"negative frequency", func(d *Draft) { d.Frequency = -5 }, []string{"frequency"}},
		{"no mode", func(d *Draft) { d.Modulation = "" }, []string{"modulation"}},
		{"unknown mode", func(d *Draft) { d.Modulation = "dmr" }, []string{"modulation"}},
		{"mode the device lacks", func(d *Draft) { d.Modulation, d.Scope = "nfm", onDevice(t, "hf") }, []string{"modulation"}},
		{"node without modes", func(d *Draft) { d.Scope = onDevice(t, "vhf") }, []string{"modulation"}},
		{"unknown device", func(d *Draft) { d.Scope = onDevice(t, "nope") }, []string{"device"}},
		{"unknown preset", func(d *Draft) { d.Scope = onPreset(t, shared.MustParseUUID("01900000-0000-7000-8000-0000000000ff")) }, []string{"preset"}},
		{"underlying of an analog mode", func(d *Draft) { d.Underlying = "usb" }, []string{"underlying"}},
		{"digital underlying", func(d *Draft) { d.Modulation, d.Underlying, d.Scope = "ft8", "ft4", onDevice(t, "hf") }, []string{"underlying"}},
		{"long description", func(d *Draft) { d.Description = string(make([]rune, 1025)) }, []string{"description"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := ok
			tt.edit(&d)

			_, err := e.m.Create(context.Background(), d)
			if got := violations(err); !slices.Equal(got, tt.want) {
				t.Errorf("violations = %v (%v), want %v", got, err, tt.want)
			}
		})
	}

	// A mode a node reports is accepted, with an underlying mode.
	e.create(t, Draft{Name: "FT8", Frequency: 7_074_000, Modulation: "FT8", Underlying: "usb", Scope: onDevice(t, "hf")})
	e.create(t, Draft{Name: "FT8 all", Frequency: 7_074_000, Modulation: "ft8", Underlying: "usb", Scope: AllDevices()})

	if len(*e.records) != 2 || len(e.changes) != 2 {
		t.Errorf("%d audit records, %d changes after the refusals", len(*e.records), len(e.changes))
	}

	if _, err := e.m.Create(context.Background(), Draft{Name: "FT8", Frequency: 7_074_000, Modulation: "ft8", Scope: AllDevices()}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate = %v", err)
	}
}

// TestHubBookmarkLifecycle: create, update, delete, audited and announced;
// optimistic concurrency; pack rows are read-only.
func TestHubBookmarkLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	b := e.create(t, Draft{Name: " Net ", Frequency: 7_100_000, Modulation: "LSB", Description: "Sunday", Scannable: true, Scope: onPreset(t, presetA)})
	if b.Name() != "Net" || b.Modulation() != "lsb" || b.Origin() != OriginDB || b.Version() != 1 {
		t.Fatalf("created = %+v", b.Snapshot())
	}

	got, err := e.m.Get(ctx, b.ID().String())
	if err != nil || got.Snapshot().Scope != b.Scope() || got.Description() != "Sunday" || !got.Scannable() {
		t.Fatalf("stored = %+v, %v", got, err)
	}

	d := Draft{Name: "Net 2", Frequency: 7_110_000, Modulation: "usb", Scope: onDevice(t, "hf")}

	if _, err := e.m.Update(ctx, b.ID().String(), 7, d); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale update = %v", err)
	}

	up, err := e.m.Update(ctx, b.ID().String(), 1, d)
	if err != nil || up.Version() != 2 || up.Name() != "Net 2" || up.Scope().Device().String() != "hf" {
		t.Fatalf("update = %+v, %v", up, err)
	}

	if err := e.m.Delete(ctx, b.ID().String(), 1); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale delete = %v", err)
	}

	if err := e.m.Delete(ctx, b.ID().String(), 2); err != nil {
		t.Fatal(err)
	}

	if _, err := e.m.Get(ctx, b.ID().String()); !errors.Is(err, ErrBookmarkNotFound) {
		t.Errorf("deleted bookmark = %v", err)
	}

	var actions []string
	for _, r := range *e.records {
		actions = append(actions, r.Action)
	}

	if !slices.Equal(actions, []string{ActionCreate, ActionUpdate, ActionDelete}) || (*e.records)[1].Before["name"] != "Net" ||
		(*e.records)[1].After["device_id"] != "hf" || (*e.records)[0].After["preset_id"] != presetA.String() {
		t.Errorf("audit = %+v", *e.records)
	}

	var ops []string
	for _, c := range e.changes {
		ops = append(ops, c.Op)
	}

	if !slices.Equal(ops, []string{OpUpsert, OpUpsert, OpDelete}) {
		t.Errorf("changes = %v", ops)
	}

	// Pack rows are read-only.
	if _, err := e.m.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	pack, err := e.m.List(ctx, Filter{Origin: OriginBuiltin})
	if err != nil || len(pack) == 0 {
		t.Fatalf("pack rows = %d, %v", len(pack), err)
	}

	if _, err := e.m.Update(ctx, pack[0].ID().String(), 1, d); !errors.Is(err, ErrReadOnly) {
		t.Errorf("update of a pack row = %v", err)
	}

	if err := e.m.Delete(ctx, pack[0].ID().String(), 1); !errors.Is(err, ErrReadOnly) {
		t.Errorf("delete of a pack row = %v", err)
	}
}

// TestSync: the packs are stored once, a second sync changes nothing,
// stale and changed pack rows are fixed, hub rows are never touched.
func TestSync(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// A hub bookmark that a pack also holds wins over the pack.
	hub := e.create(t, Draft{Name: "PMR1", Frequency: 446_006_250, Modulation: "nfm", Description: "Our PMR", Scope: AllDevices()})

	res, err := e.m.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if res != (SyncResult{Inserted: e.m.Packs().Len() - 1, Skipped: 1}) {
		t.Fatalf("first sync = %+v", res)
	}

	again, err := e.m.Sync(ctx)
	if err != nil || again != (SyncResult{Unchanged: e.m.Packs().Len() - 1, Skipped: 1}) {
		t.Fatalf("second sync = %+v, %v", again, err)
	}

	// A pack row that no pack holds is deleted; a changed one is restored.
	stale := &packEntry{name: "Gone", frequency: 1_000_000, modulation: "am", packs: []string{"old"}}
	s := stale.snapshot()
	s.CreatedAt, s.UpdatedAt = t0, t0

	b, err := Rehydrate(s)
	if err != nil {
		t.Fatal(err)
	}

	if err := e.m.repo.Create(ctx, b); err != nil {
		t.Fatal(err)
	}

	first := e.m.packs.entries[0]
	if _, err := e.m.d.DB.Writer(ctx).ExecContext(ctx, "UPDATE bookmarks SET name = name, description = 'edited' WHERE id = ?", first.id().Bytes()); err != nil {
		t.Fatal(err)
	}

	res, err = e.m.Sync(ctx)
	if err != nil || res != (SyncResult{Unchanged: e.m.Packs().Len() - 2, Updated: 1, Deleted: 1, Skipped: 1}) {
		t.Fatalf("repair sync = %+v, %v", res, err)
	}

	if _, err := e.m.repo.Get(ctx, b.ID()); !errors.Is(err, ErrBookmarkNotFound) {
		t.Errorf("stale pack row = %v", err)
	}

	fixed, err := e.m.repo.Get(ctx, first.id())
	if err != nil || fixed.Description() != "" || fixed.Version() != 2 {
		t.Errorf("repaired pack row = %+v, %v", fixed, err)
	}

	kept, err := e.m.repo.Get(ctx, hub.ID())
	if err != nil || kept.Snapshot().Description != "Our PMR" || kept.Version() != 1 || kept.Origin() != OriginDB {
		t.Errorf("hub row after the syncs = %+v, %v", kept, err)
	}

	if len(e.changes) != 1 || len(*e.records) != 1 {
		t.Errorf("the sync audited or announced changes: %d, %d", len(*e.records), len(e.changes))
	}
}

// TestRegionAndScope: the bookmarks of a device are the general pack, the
// current region's pack and the hub rows whose scope matches the device
// and its active preset; the listen policy decides who reads them.
func TestRegionAndScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if _, err := e.m.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	for _, d := range []Draft{
		{Name: "All", Frequency: 446_000_000, Modulation: "am", Scope: AllDevices()},
		{Name: "HF only", Frequency: 446_000_000, Modulation: "usb", Scope: onDevice(t, "hf")},
		{Name: "Preset A", Frequency: 446_000_000, Modulation: "lsb", Scope: onPreset(t, presetA)},
		{Name: "Preset B", Frequency: 446_000_000, Modulation: "cw", Scope: onPreset(t, presetB)},
	} {
		e.create(t, d)
	}

	rg, err := NewRange(new(int64(430_000_000)), new(int64(470_000_000)))
	if err != nil {
		t.Fatal(err)
	}

	// hf runs preset A.
	hf, err := e.m.ForDevice(ctx, "hf", rg)
	if err != nil {
		t.Fatal(err)
	}

	if got := nameList(hf); !slices.Contains(got, "All") || !slices.Contains(got, "HF only") || !slices.Contains(got, "Preset A") ||
		slices.Contains(got, "Preset B") || !slices.Contains(got, "PMR1") || !slices.Contains(got, "LPD1") || slices.Contains(got, "GMRS1") {
		t.Fatalf("hf in r1 = %v", got)
	}

	// Sorted by frequency, hub rows before pack rows on a tie.
	for i := 1; i < len(hf); i++ {
		a, b := hf[i-1], hf[i]
		if a.Frequency() > b.Frequency() || (a.Frequency() == b.Frequency() && a.ReadOnly() && !b.ReadOnly()) {
			t.Fatalf("order at %d: %s then %s", i, a.Name(), b.Name())
		}
	}

	// vhf is registered-only: refused to anonymous callers, and it runs no
	// preset.
	if _, err := e.m.ForDevice(ctx, "vhf", rg); !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("anonymous on a registered device = %v", err)
	}

	if _, err := e.m.ForDevice(ctx, "nope", rg); !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("unknown device = %v", err)
	}

	vhf, err := e.m.ForDevice(signedIn(), "vhf", rg)
	if got := nameList(vhf); err != nil || !slices.Contains(got, "All") || slices.Contains(got, "HF only") || slices.Contains(got, "Preset A") {
		t.Errorf("vhf = %v, %v", got, err)
	}

	// Region r2: its own pack, not r1's; r3 shares PMR with r1.
	e.region = "r2"

	r2, err := e.m.ForDevice(ctx, "hf", rg)
	if got := nameList(r2); err != nil || slices.Contains(got, "PMR1") || slices.Contains(got, "LPD1") || !slices.Contains(got, "GMRS1") {
		t.Errorf("hf in r2 = %v, %v", got, err)
	}

	e.region = "r3"

	r3, err := e.m.ForDevice(ctx, "hf", rg)
	if got := nameList(r3); err != nil || !slices.Contains(got, "PMR1") || slices.Contains(got, "LPD1") {
		t.Errorf("hf in r3 = %v, %v", got, err)
	}

	// The general pack shows in every region.
	all, err := e.m.ForDevice(ctx, "hf", Range{})
	if got := nameList(all); err != nil || !slices.Contains(got, "Emergency") {
		t.Errorf("general pack in r3: %v", err)
	}

	// Management filters.
	for _, tt := range []struct {
		f    Filter
		want int
	}{
		{Filter{Origin: OriginDB}, 4},
		{Filter{Scope: ScopePreset}, 2},
		{Filter{Origin: OriginDB, Device: "hf"}, 3},
		{Filter{Origin: OriginDB, Device: "vhf"}, 1},
		{Filter{Origin: OriginDB, Device: "nope"}, 0},
	} {
		got, err := e.m.List(ctx, tt.f)
		if err != nil || len(got) != tt.want {
			t.Errorf("List(%+v) = %v, %v; want %d", tt.f, nameList(got), err, tt.want)
		}
	}

	if _, err := NewRange(new(int64(10)), new(int64(5))); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("inverted range = %v", err)
	}

	if _, err := NewRange(new(int64(-1)), nil); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("negative range = %v", err)
	}
}
