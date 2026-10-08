package settings

import (
	"slices"
	"time"
)

// Snapshot is an immutable view of every effective setting at one settings
// revision, with the values decoded to their Go types. Consumers read it on
// every use, so a saved change applies at once (ADR 0010 "Live apply").
type Snapshot struct {
	revision int64
	entries  []Effective
	index    map[string]int
	typed    map[string]any
}

func newSnapshot(revision int64, entries []Effective, typed map[string]any) *Snapshot {
	s := &Snapshot{revision: revision, entries: entries, index: make(map[string]int, len(entries)), typed: typed}
	for i, e := range entries {
		s.index[e.Key()] = i
	}

	return s
}

// Revision returns the settings revision of the snapshot.
func (s *Snapshot) Revision() int64 { return s.revision }

// All returns every effective setting in schema order.
func (s *Snapshot) All() []Effective { return append([]Effective(nil), s.entries...) }

// Get returns the effective setting of key.
func (s *Snapshot) Get(key string) (Effective, bool) {
	i, ok := s.index[key]
	if !ok {
		return Effective{}, false
	}

	return s.entries[i], true
}

// Typed returns the Go value of key (nil when unknown).
func (s *Snapshot) Typed(key string) any { return s.typed[key] }

// String returns a string setting ("" when unknown).
func (s *Snapshot) String(key string) string {
	v, _ := s.typed[key].(string)

	return v
}

// Bool returns a boolean setting (false when unknown).
func (s *Snapshot) Bool(key string) bool {
	v, _ := s.typed[key].(bool)

	return v
}

// Strings returns the value of a list setting (a copy; nil when unset).
func (s *Snapshot) Strings(key string) []string {
	v, _ := s.typed[key].([]string)

	return slices.Clone(v)
}

// Int returns an integer setting (0 when unknown).
func (s *Snapshot) Int(key string) int {
	v, _ := s.typed[key].(int)

	return v
}

// Duration returns a duration setting (0 when unknown).
func (s *Snapshot) Duration(key string) time.Duration {
	v, ok := s.typed[key].(interface{ Duration() time.Duration })
	if !ok {
		return 0
	}

	return v.Duration()
}

// Rate returns a rate setting: count events per window (zero when
// unknown).
func (s *Snapshot) Rate(key string) (int, time.Duration) {
	v, ok := s.typed[key].(interface {
		Count() int
		Window() time.Duration
	})
	if !ok {
		return 0, 0
	}

	return v.Count(), v.Window()
}

// Geo returns a position setting; ok is false when it is unset.
func (s *Snapshot) Geo(key string) (lat, lon float64, ok bool) {
	v, isGeo := s.typed[key].(interface {
		IsSet() bool
		Lat() float64
		Lon() float64
	})
	if !isGeo || !v.IsSet() {
		return 0, 0, false
	}

	return v.Lat(), v.Lon(), true
}

// Values reads typed settings from the current snapshot of a store on
// every call: consumers hold it to see changes at once. *Store implements
// it.
type Values interface {
	String(key string) string
	Bool(key string) bool
	Int(key string) int
	Duration(key string) time.Duration
	Rate(key string) (int, time.Duration)
}

var _ Values = (*Store)(nil)

// String reads a string setting of the current snapshot.
func (s *Store) String(key string) string { return s.Snapshot().String(key) }

// Bool reads a boolean setting of the current snapshot.
func (s *Store) Bool(key string) bool { return s.Snapshot().Bool(key) }

// Int reads an integer setting of the current snapshot.
func (s *Store) Int(key string) int { return s.Snapshot().Int(key) }

// Duration reads a duration setting of the current snapshot.
func (s *Store) Duration(key string) time.Duration { return s.Snapshot().Duration(key) }

// Strings reads a list setting of the current snapshot.
func (s *Store) Strings(key string) []string { return s.Snapshot().Strings(key) }

// Rate reads a rate setting of the current snapshot.
func (s *Store) Rate(key string) (int, time.Duration) { return s.Snapshot().Rate(key) }
