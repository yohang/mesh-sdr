package app

import (
	"slices"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// LinkState is the in-memory state of one node's control channel.
type LinkState struct {
	Connected      bool
	EverWelcomed   bool
	Boot           shared.UUID
	Compat         domain.Compatibility
	WelcomedAt     time.Time
	DisconnectedAt time.Time
	LastHeartbeat  time.Time
	ClockOffsetMS  int64
	NTPSynced      bool
	DialFailures   int
	// StateRevision is the desired-state revision pushed on the current
	// channel (0: none yet); AppliedRevision the last one the node
	// answered, with the devices it refused (ADR 0020).
	StateRevision   int64
	AppliedRevision int64
	StateErrors     []ctl.StateError
}

// Tracker holds the link state of every node (RAM only; rebuilt from the
// channels after a hub restart).
type Tracker struct {
	mu    sync.Mutex
	links map[domain.NodeID]*LinkState
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker { return &Tracker{links: map[domain.NodeID]*LinkState{}} }

func (t *Tracker) get(id domain.NodeID) *LinkState {
	s, ok := t.links[id]
	if !ok {
		s = &LinkState{NTPSynced: true}
		t.links[id] = s
	}

	return s
}

// Welcomed records an accepted ctl.welcome.
func (t *Tracker) Welcomed(id domain.NodeID, boot shared.UUID, compat domain.Compatibility, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.get(id)
	s.Connected, s.EverWelcomed, s.Boot, s.Compat, s.WelcomedAt, s.DialFailures = true, true, boot, compat, now, 0
	s.StateRevision, s.AppliedRevision, s.StateErrors = 0, 0, nil
}

// StatePushed records the desired-state revision sent on the channel.
func (t *Tracker) StatePushed(id domain.NodeID, revision int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.get(id).StateRevision = revision
}

// StateApplied records the node's answer to a desired state.
func (t *Tracker) StateApplied(id domain.NodeID, revision int64, errs []ctl.StateError) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.get(id)
	s.AppliedRevision, s.StateErrors = revision, slices.Clone(errs)
}

// Connected returns the nodes whose channel is up.
func (t *Tracker) Connected() []domain.NodeID {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]domain.NodeID, 0, len(t.links))

	for id, s := range t.links {
		if s.Connected {
			out = append(out, id)
		}
	}

	return out
}

// Heartbeat records a heartbeat applied at now.
func (t *Tracker) Heartbeat(id domain.NodeID, now time.Time, clockOffsetMS int64, ntpSynced bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.get(id)
	s.LastHeartbeat, s.ClockOffsetMS, s.NTPSynced = now, clockOffsetMS, ntpSynced
}

// Disconnected records the end of a channel.
func (t *Tracker) Disconnected(id domain.NodeID, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.get(id)
	if s.Connected {
		s.DisconnectedAt = now
	}

	s.Connected = false
}

// DialFailed records a failed dial.
func (t *Tracker) DialFailed(id domain.NodeID, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.get(id)
	s.DialFailures++

	if s.DisconnectedAt.IsZero() {
		s.DisconnectedAt = now
	}
}

// Forget drops the state of a removed node.
func (t *Tracker) Forget(id domain.NodeID) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.links, id)
}

// State returns a copy of the state of id.
func (t *Tracker) State(id domain.NodeID) (LinkState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, ok := t.links[id]
	if !ok {
		return LinkState{}, false
	}

	c := *s
	c.StateErrors = slices.Clone(s.StateErrors)

	return c, true
}
