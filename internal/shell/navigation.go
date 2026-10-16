package shell

import (
	"context"

	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// Gate decides whether the visitor of a request may open a section. Gates
// are supplied at wiring by the modules that own the sections' policies
// (identity for Admin, files for Files).
type Gate interface {
	Allows(ctx context.Context) bool
}

// GateFunc adapts a function to a Gate.
type GateFunc func(ctx context.Context) bool

// Allows implements Gate.
func (f GateFunc) Allows(ctx context.Context) bool { return f(ctx) }

// Everyone is the gate of a section open to every visitor.
var Everyone Gate = GateFunc(func(context.Context) bool { return true })

// sections are the top-level sections of the navigation (UI-006,
// FEATURE_SPEC §10.2), in order.
var sections = []layout.Link{
	{Section: layout.SectionReceiver, Label: "Receiver", Href: "/"},
	{Section: layout.SectionMap, Label: "Map", Href: "/map"},
	{Section: layout.SectionDecodes, Label: "Decodes", Href: "/decodes"},
	{Section: layout.SectionFiles, Label: "Files", Href: "/files"},
	{Section: layout.SectionAdmin, Label: "Admin", Href: "/admin"},
}

// nav returns the sections the visitor of ctx may open, in order. A section
// without a gate is never shown, so the navigation fails closed.
func nav(ctx context.Context, gates map[string]Gate) []layout.Link {
	var out []layout.Link

	for _, l := range sections {
		if g := gates[l.Section]; g != nil && g.Allows(ctx) {
			out = append(out, l)
		}
	}

	return out
}
