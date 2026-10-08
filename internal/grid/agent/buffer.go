// Package agent is the node side of the grid: the bounded event buffer and
// the node agent that numbers events, answers the hub hello and produces
// heartbeats and capability reports (TECHNICAL_SPEC §4.4, §4.5, §4.7,
// ADR 0008). It holds no state outside RAM.
package agent

import (
	"slices"
	"sync"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// Class orders events for dropping on overflow (§2.3, §7.3): the lowest
// class is dropped first, oldest first within a class.
type Class int

// Event classes.
const (
	ClassDiagnostics Class = iota
	ClassMap
	ClassDecode
	ClassState
)

// Event is one numbered node → hub event.
type Event struct {
	Seq     int64
	Type    rxv1.MessageType
	Payload any
	// Key coalesces snapshot events: a new event with the same key replaces
	// the pending one (latest wins). Empty for plain events.
	Key   string
	Class Class
	Size  int
}

// Buffer keeps unacknowledged events in RAM, bounded by count and bytes.
type Buffer struct {
	maxEvents int
	maxBytes  int

	mu      sync.Mutex
	events  []Event
	bytes   int
	acked   int64
	dropped map[string]int64
	notify  chan struct{}
	// onAck is told every new acknowledged seq (the file outbox).
	onAck func(seq int64)
}

// OnAck registers the function told each new acknowledged seq, after the
// buffer dropped the acknowledged events (composition time only).
func (b *Buffer) OnAck(f func(seq int64)) { b.onAck = f }

// NewBuffer returns a buffer of at most maxEvents events and maxBytes bytes.
func NewBuffer(maxEvents, maxBytes int) *Buffer {
	return &Buffer{maxEvents: max(maxEvents, 1), maxBytes: max(maxBytes, 1), dropped: map[string]int64{}, notify: make(chan struct{}, 1)}
}

// Notify is signalled when events are pushed.
func (b *Buffer) Notify() <-chan struct{} { return b.notify }

// Push appends e, replacing a pending event with the same key, then drops
// events while the buffer is over its bounds.
func (b *Buffer) Push(e Event) {
	b.mu.Lock()

	if e.Key != "" {
		if i := slices.IndexFunc(b.events, func(x Event) bool { return x.Key == e.Key }); i >= 0 {
			b.bytes -= b.events[i].Size
			b.events = slices.Delete(b.events, i, i+1)
		}
	}

	b.events = append(b.events, e)
	b.bytes += e.Size

	for len(b.events) > 1 && (len(b.events) > b.maxEvents || b.bytes > b.maxBytes) {
		b.dropOne()
	}

	b.mu.Unlock()

	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// dropOne removes the oldest event of the lowest class. Caller holds mu.
func (b *Buffer) dropOne() {
	victim := 0

	for i, e := range b.events {
		if e.Class < b.events[victim].Class {
			victim = i
		}
	}

	e := b.events[victim]
	b.bytes -= e.Size
	b.events = slices.Delete(b.events, victim, victim+1)
	b.dropped[string(e.Type)]++
}

// Ack drops every event up to and including seq.
func (b *Buffer) Ack(seq int64) {
	if !b.ack(seq) || b.onAck == nil {
		return
	}

	b.onAck(seq)
}

// ack drops the events up to seq and reports whether seq is new.
func (b *Buffer) ack(seq int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if seq <= b.acked {
		return false
	}

	b.acked = seq
	n := 0

	for _, e := range b.events {
		if e.Seq > seq {
			b.events[n] = e
			n++
		} else {
			b.bytes -= e.Size
		}
	}

	clear(b.events[n:])
	b.events = b.events[:n]

	return true
}

// Acked returns the highest acknowledged seq.
func (b *Buffer) Acked() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.acked
}

// After returns the pending events with a seq greater than seq, in order.
func (b *Buffer) After(seq int64) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []Event

	for _, e := range b.events {
		if e.Seq > seq {
			out = append(out, e)
		}
	}

	return out
}

// Len returns the number of pending events.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.events)
}

// TakeDropped returns and resets the drop counters per event type.
func (b *Buffer) TakeDropped() (int64, map[string]int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var total int64
	for _, n := range b.dropped {
		total += n
	}

	kinds := b.dropped
	b.dropped = map[string]int64{}

	return total, kinds
}
