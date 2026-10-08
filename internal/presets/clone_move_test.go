package presets_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/presets"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestClone(t *testing.T) {
	ctx := context.Background()
	s, au, _, changed := newService(t)

	one, two := 1, -60
	start, step := int64(145_500_000), int64(12_500)

	src, err := s.Create(ctx, presets.Draft{
		Name: "2 m FM", Description: "Repeaters", Tags: []string{"vhf", "fm"}, CenterFreq: 145_000_000, SampRate: 2_048_000,
		StartFreq: &start, StartMod: "nfm", TuningStep: &step, InitialSquelchLevel: &two, InitialNRLevel: &one, WaterfallLevels: &[2]int{-90, -20},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Every field is copied; the identity, the name, the slug and the
	// position are new.
	c1, err := s.Clone(ctx, src.ID().String())
	if err != nil {
		t.Fatal(err)
	}

	want, got := src.Snapshot(), c1.Snapshot()

	if got.ID == want.ID || got.Name != "2 m FM (copy)" || got.Slug != "2-m-fm-copy" || got.SortOrder != 1 || got.Version != 1 {
		t.Errorf("clone identity = %+v", got)
	}

	got.ID, got.Name, got.Slug, got.SortOrder, got.CreatedAt, got.UpdatedAt = want.ID, want.Name, want.Slug, want.SortOrder, want.CreatedAt, want.UpdatedAt

	if !reflect.DeepEqual(got, want) {
		t.Errorf("clone fields = %+v, want %+v", got, want)
	}

	// A taken name gets the next number, a copy of a copy included.
	c2, err := s.Clone(ctx, src.ID().String())
	if err != nil {
		t.Fatal(err)
	}

	c3, err := s.Clone(ctx, src.ID().String())
	if err != nil {
		t.Fatal(err)
	}

	c4, err := s.Clone(ctx, c1.ID().String())
	if err != nil {
		t.Fatal(err)
	}

	names := []string{c2.Name(), c3.Name(), c4.Name()}
	if !reflect.DeepEqual(names, []string{"2 m FM (copy 2)", "2 m FM (copy 3)", "2 m FM (copy) (copy)"}) {
		t.Errorf("names = %v", names)
	}

	if c2.Slug() != "2-m-fm-copy-2" || c3.Slug() == c2.Slug() {
		t.Errorf("slugs = %s %s", c2.Slug(), c3.Slug())
	}

	// The name stays within its limit.
	long, err := s.Create(ctx, presets.Draft{Name: strings.Repeat("x", presets.MaxNameLength), CenterFreq: 1, SampRate: 1})
	if err != nil {
		t.Fatal(err)
	}

	lc, err := s.Clone(ctx, long.ID().String())
	if err != nil || len([]rune(lc.Name())) > presets.MaxNameLength || !strings.HasSuffix(lc.Name(), " (copy)") {
		t.Errorf("long clone = %q, %v", lc.Name(), err)
	}

	if _, err := s.Clone(ctx, shared.MustParseUUID("0192f2b4-0000-7000-8000-0000000000aa").String()); !errors.Is(err, presets.ErrPresetNotFound) {
		t.Errorf("unknown source: %v", err)
	}

	// Each copy sits right after its source, positions stay contiguous.
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var order []string

	for i, p := range list {
		if p.SortOrder() != i {
			t.Errorf("%s at %d, want %d", p.Name(), p.SortOrder(), i)
		}

		order = append(order, p.Name())
	}

	if wantOrder := []string{"2 m FM", "2 m FM (copy 3)", "2 m FM (copy 2)", "2 m FM (copy)", "2 m FM (copy) (copy)", long.Name(), lc.Name()}; !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("order = %v, want %v", order, wantOrder)
	}

	if au.records[1].Action != presets.ActionClone || au.records[1].After["cloned_from"] != src.ID().String() {
		t.Errorf("audit = %+v", au.records)
	}

	if *changed != 7 {
		t.Errorf("changed %d times", *changed)
	}
}

// TestMove: reordering renumbers the display positions only. It never edits
// a preset, notifies the schedules or pushes the desired state of the nodes
// (nothing is retuned).
func TestMove(t *testing.T) {
	ctx := context.Background()
	s, au, sc, changed := newService(t)

	var ids []string

	for _, n := range []string{"A", "B", "C", "D"} {
		p, err := s.Create(ctx, presets.Draft{Name: n, CenterFreq: 100_000_000, SampRate: 2_000_000})
		if err != nil {
			t.Fatal(err)
		}

		ids = append(ids, p.ID().String())
	}

	order := func() string {
		list, err := s.List(ctx)
		if err != nil {
			t.Fatal(err)
		}

		var b strings.Builder

		for i, p := range list {
			if p.SortOrder() != i || p.Version() != 1 {
				t.Errorf("%s: position %d version %d", p.Name(), p.SortOrder(), p.Version())
			}

			b.WriteString(p.Name())
		}

		return b.String()
	}

	creates, audits := *changed, len(au.records)

	for _, tt := range []struct {
		name  string
		move  func() (bool, error)
		want  string
		moved bool
	}{
		{"down", func() (bool, error) { return s.MoveBy(ctx, ids[0], 1) }, "BACD", true},
		{"up", func() (bool, error) { return s.MoveBy(ctx, ids[2], -1) }, "BCAD", true},
		{"first stays first", func() (bool, error) { return s.MoveBy(ctx, ids[1], -1) }, "BCAD", false},
		{"last stays last", func() (bool, error) { return s.MoveBy(ctx, ids[3], 1) }, "BCAD", false},
		{"to a position", func() (bool, error) { return s.MoveTo(ctx, ids[3], 0) }, "DBCA", true},
		{"beyond the end", func() (bool, error) { return s.MoveTo(ctx, ids[3], 99) }, "BCAD", true},
	} {
		moved, err := tt.move()
		if err != nil || moved != tt.moved {
			t.Errorf("%s: moved %v, %v", tt.name, moved, err)
		}

		if got := order(); got != tt.want {
			t.Errorf("%s: order %s, want %s", tt.name, got, tt.want)
		}
	}

	if _, err := s.MoveBy(ctx, "0192f2b4-0000-7000-8000-0000000000aa", 1); !errors.Is(err, presets.ErrPresetNotFound) {
		t.Errorf("unknown preset: %v", err)
	}

	// No retune: the desired state is not pushed again and the schedules
	// are not told that a preset changed.
	if *changed != creates || len(sc.replaced) != 0 {
		t.Errorf("changed %d (was %d), schedules told %v", *changed, creates, sc.replaced)
	}

	if n := len(au.records) - audits; n != 4 || au.records[len(au.records)-1].Action != presets.ActionMove {
		t.Errorf("%d move audits, last %+v", n, au.records[len(au.records)-1])
	}
}
