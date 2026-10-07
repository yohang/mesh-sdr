package settings

import (
	"slices"
	"strconv"
)

// Source is the layer an effective value comes from (TECHNICAL_SPEC §6.10).
type Source string

// Sources, by decreasing precedence.
const (
	SourceConfig  Source = "cfg"
	SourceDB      Source = "db"
	SourceDefault Source = "default"
)

// Configured is a value set in the hub config: a file or an env var. It
// locks its key.
type Configured struct {
	Value  Value
	Origin string // "hub.toml" or "env:MESHSDR_SETTINGS__…"
}

// Effective is the effective value of one key, with where it comes from.
type Effective struct {
	def      Definition
	value    Value
	source   Source
	origin   string
	version  int64
	shadowed *Value
}

// Resolve applies the precedence config (locked) > DB > default. stored is
// the valid DB row of the key, or nil. A DB value under a config value is
// kept as the shadowed value ("overridden by config").
func Resolve(def Definition, cfg *Configured, stored *Setting) Effective {
	e := Effective{def: def, value: def.Default(), source: SourceDefault, origin: string(SourceDefault)}

	if stored != nil {
		e.version = stored.Version()
	}

	switch {
	case cfg != nil:
		e.value, e.source, e.origin = cfg.Value, SourceConfig, cfg.Origin

		if stored != nil {
			v := stored.Value()
			e.shadowed = &v
		}
	case stored != nil:
		e.value, e.source, e.origin = stored.Value(), SourceDB, string(SourceDB)
	}

	return e
}

// ResolveIgnoring resolves a key whose DB row is ignored (its value is
// invalid): the value comes from the config or the default, but the
// version is the row's, so a write or a reset from the UI can replace the
// row (the writer sends back the version it read).
func ResolveIgnoring(def Definition, cfg *Configured, ignored *Setting) Effective {
	e := Resolve(def, cfg, nil)
	if ignored != nil {
		e.version = ignored.Version()
	}

	return e
}

// Definition returns the key definition.
func (e Effective) Definition() Definition { return e.def }

// Key returns the key.
func (e Effective) Key() string { return e.def.Key() }

// Value returns the effective value. Callers that expose it must mask
// secrets (Definition.Secret).
func (e Effective) Value() Value { return e.value }

// Source returns the layer of the value.
func (e Effective) Source() Source { return e.source }

// Origin returns "hub.toml", "env:VAR", "db" or "default".
func (e Effective) Origin() string { return e.origin }

// Locked reports whether the hub config sets the key.
func (e Effective) Locked() bool { return e.source == SourceConfig }

// Version returns the version of the DB row (0 when there is none). Writes
// send it back to detect concurrent changes.
func (e Effective) Version() int64 { return e.version }

// Shadowed returns the DB value hidden by a config value, if any.
func (e Effective) Shadowed() (Value, bool) {
	if e.shadowed == nil {
		return Value{}, false
	}

	return *e.shadowed, true
}

// IsSet reports whether a non-null value is in effect (for secrets: "set").
func (e Effective) IsSet() bool { return !e.value.IsNull() }

// Change is one key of a write: a new value, or a reset to the default
// (nil value), with the version the writer last saw (0: no DB row).
type Change struct {
	key      string
	value    *Value
	expected int64
}

// SetTo returns a change writing value.
func SetTo(key string, value Value, expected int64) Change {
	return Change{key: key, value: &value, expected: expected}
}

// ResetTo returns a change deleting the DB value of key.
func ResetTo(key string, expected int64) Change { return Change{key: key, expected: expected} }

// Key returns the key.
func (c Change) Key() string { return c.key }

// Value returns the new value; ok is false for a reset.
func (c Change) Value() (Value, bool) {
	if c.value == nil {
		return Value{}, false
	}

	return *c.value, true
}

// IsReset reports whether the change resets the key.
func (c Change) IsReset() bool { return c.value == nil }

// Expected returns the version the writer last saw.
func (c Change) Expected() int64 { return c.expected }

// ChangeSet is a non-empty write of distinct keys, applied atomically.
type ChangeSet struct{ changes []Change }

// NewChangeSet validates a write.
func NewChangeSet(changes ...Change) (ChangeSet, error) {
	if len(changes) == 0 {
		return ChangeSet{}, ErrInvalidSetting.WithDetail("no setting to write")
	}

	seen := map[string]bool{}

	for _, c := range changes {
		if c.key == "" || c.expected < 0 {
			return ChangeSet{}, ErrInvalidSetting.WithDetail("invalid change")
		}

		if seen[c.key] {
			return ChangeSet{}, ErrInvalidSetting.WithDetail("duplicate key " + strconv.Quote(c.key))
		}

		seen[c.key] = true
	}

	return ChangeSet{changes: slices.Clone(changes)}, nil
}

// Changes returns the changes in order.
func (s ChangeSet) Changes() []Change { return slices.Clone(s.changes) }
