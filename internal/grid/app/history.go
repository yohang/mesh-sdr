package app

import (
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

// LoadSample is one heartbeat kept for the node load history.
type LoadSample struct {
	At                time.Time
	CPU               float64
	Load1             float64
	TempC             *float64
	MemAvailableBytes uint64
	MemTotalBytes     uint64
}

// HistorySize is the number of samples kept per node (1 h at 10 s).
const HistorySize = 360

// History is the RAM ring of recent heartbeats per node (ADR 0008 Q21).
type History struct {
	mu    sync.Mutex
	nodes map[domain.NodeID][]LoadSample
}

// NewHistory returns an empty history.
func NewHistory() *History { return &History{nodes: map[domain.NodeID][]LoadSample{}} }

// Add appends a sample, dropping the oldest beyond HistorySize.
func (h *History) Add(id domain.NodeID, s LoadSample) {
	h.mu.Lock()
	defer h.mu.Unlock()

	ring := append(h.nodes[id], s)
	if len(ring) > HistorySize {
		ring = append(ring[:0:0], ring[len(ring)-HistorySize:]...)
	}

	h.nodes[id] = ring
}

// Samples returns a copy of the samples of id, oldest first.
func (h *History) Samples(id domain.NodeID) []LoadSample {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]LoadSample(nil), h.nodes[id]...)
}

// Forget drops the samples of id.
func (h *History) Forget(id domain.NodeID) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.nodes, id)
}
