package settings

import (
	"encoding/json"
	"time"
)

// Classes of keys in the effective configuration (FEATURE_SPEC §9.1).
const (
	ClassConfig = "cfg" // config-only bootstrap key
	ClassDB     = "db"  // admin setting, lockable by config
)

// BootstrapEntry is a config-only key of the hub config.
type BootstrapEntry struct {
	Key    string
	Value  json.RawMessage // null for secrets (see Set)
	Set    bool            // secrets: whether a value is set
	Origin string          // "hub.toml", "env:VAR" or "default"
	Locked bool            // set by a file or the env
	Secret bool
}

// Bootstrap lists the config-only keys of the hub config.
type Bootstrap interface {
	BootstrapEntries() []BootstrapEntry
}

// ConfigEntry is one key of the effective configuration.
type ConfigEntry struct {
	Key    string
	Class  string
	Value  json.RawMessage // absent (nil) for secrets
	Set    bool            // secrets only
	Source Source
	Origin string
	Locked bool
	Secret bool
}

// EffectiveConfig is the effective configuration view (ADM-010): every
// config-only key and every setting, with its value (secrets masked), its
// source and its lock.
type EffectiveConfig struct {
	bootstrap Bootstrap
	store     *Store
	now       Clock
}

// NewEffectiveConfig returns the use case.
func NewEffectiveConfig(bootstrap Bootstrap, store *Store, now Clock) *EffectiveConfig {
	return &EffectiveConfig{bootstrap: bootstrap, store: store, now: now}
}

// ConfigView is the effective configuration at one time.
type ConfigView struct {
	GeneratedAt time.Time
	Revision    int64
	Entries     []ConfigEntry
}

// View returns the effective configuration: config-only keys first, then
// the settings (with their hub.toml name, settings.…).
func (c *EffectiveConfig) View() ConfigView {
	snap := c.store.Snapshot()
	v := ConfigView{GeneratedAt: c.now().UTC(), Revision: snap.Revision()}

	for _, b := range c.bootstrap.BootstrapEntries() {
		e := ConfigEntry{Key: b.Key, Class: ClassConfig, Origin: b.Origin, Locked: b.Locked, Secret: b.Secret, Set: b.Set,
			Source: SourceDefault}
		if b.Locked {
			e.Source = SourceConfig
		}

		if !b.Secret {
			e.Value = b.Value
		}

		v.Entries = append(v.Entries, e)
	}

	for _, s := range snap.All() {
		d := s.Definition()
		e := ConfigEntry{Key: ConfigKey(s.Key()), Class: ClassDB, Source: s.Source(), Origin: s.Origin(), Locked: s.Locked(), Secret: d.Secret()}

		if d.Secret() {
			e.Set = s.IsSet()
		} else {
			e.Value = s.Value().JSON()
		}

		v.Entries = append(v.Entries, e)
	}

	return v
}
