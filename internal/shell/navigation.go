package shell

import (
	"context"
)

// Gate decides whether the visitor of a request may open a section. Gates
// are supplied at wiring by the modules that own the sections' policies
// (identity for Admin; the files, decodes and map modules later).
type Gate interface {
	Allows(ctx context.Context) bool
}

// GateFunc adapts a function to a Gate.
type GateFunc func(ctx context.Context) bool

// Allows implements Gate.
func (f GateFunc) Allows(ctx context.Context) bool { return f(ctx) }

// Everyone is the gate of a section open to every visitor.
var Everyone Gate = GateFunc(func(context.Context) bool { return true })

// Navigation lists the sections a visitor may open (UI-006). A section
// without a gate is never shown, so the navigation fails closed.
type Navigation struct {
	gates map[Section]Gate
}

// NewNavigation returns the use case. gates holds the gate of each section.
func NewNavigation(gates map[Section]Gate) *Navigation {
	g := make(map[Section]Gate, len(gates))
	for s, gate := range gates {
		if gate != nil {
			g[s] = gate
		}
	}

	return &Navigation{gates: g}
}

// Sections returns the sections the visitor of ctx may open, in navigation
// order.
func (n *Navigation) Sections(ctx context.Context) []Section {
	var out []Section

	for _, s := range Sections() {
		if g, ok := n.gates[s]; ok && g.Allows(ctx) {
			out = append(out, s)
		}
	}

	return out
}
