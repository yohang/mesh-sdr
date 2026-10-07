package shell_test

import (
	"context"
	"slices"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell"
)

type adminKey struct{}

func TestNavigationGates(t *testing.T) {
	admin := shell.GateFunc(func(ctx context.Context) bool { return ctx.Value(adminKey{}) != nil })

	nav := shell.NewNavigation(map[shell.Section]shell.Gate{
		shell.SectionReceiver: shell.Everyone,
		shell.SectionMap:      shell.Everyone,
		shell.SectionFiles:    shell.Everyone,
		shell.SectionAdmin:    admin,
		shell.SectionDecodes:  nil, // no gate: never shown
	})

	ids := func(ss []shell.Section) (out []string) {
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
