package app

import (
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
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

	return *s, true
}
