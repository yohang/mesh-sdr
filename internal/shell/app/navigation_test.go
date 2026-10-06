package app_test

import (
	"context"
	"slices"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell/app"
	"github.com/yohang/mesh-sdr/internal/shell/domain"
)

type adminKey struct{}

func TestNavigation(t *testing.T) {
	admin := app.GateFunc(func(ctx context.Context) bool { return ctx.Value(adminKey{}) != nil })

	nav := app.NewNavigation(map[domain.Section]app.Gate{
		domain.SectionReceiver: app.Everyone,
		domain.SectionMap:      app.Everyone,
		domain.SectionFiles:    app.Everyone,
		domain.SectionAdmin:    admin,
		domain.SectionDecodes:  nil, // no gate: never shown
	})

	ids := func(ss []domain.Section) (out []string) {
		for _, s := range ss {
			out = append(out, s.ID())
		}

		return out
	}

	got := ids(nav.Sections(context.Background()))
	if want := []string{"receiver", "map", "files"}; !slices.Equal(got, want) {
		t.Errorf("visitor sections = %v, want %v", got, want)
	}

	got = ids(nav.Sections(context.WithValue(context.Background(), adminKey{}, true)))
	if want := []string{"receiver", "map", "files", "admin"}; !slices.Equal(got, want) {
		t.Errorf("admin sections = %v, want %v", got, want)
	}
}
