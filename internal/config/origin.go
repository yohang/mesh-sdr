package config

import "sort"

// OriginKind tells where an effective config value comes from.
type OriginKind int

// Origin kinds, by increasing precedence.
const (
	OriginDefault OriginKind = iota
	OriginFile
	OriginEnv
)

// Origin is the source of one config key: "default", the file name
// ("hub.toml", "hub.d/10-site.toml") or "env:MESHSDR_DB__DSN". Keys set by a
// file or env var are locked.
type Origin struct {
	kind OriginKind
	file string // path relative to the config dir
	env  string
}

// Kind returns the origin kind.
func (o Origin) Kind() OriginKind { return o.kind }

// Locked reports whether the key is set by a config file or an env var.
func (o Origin) Locked() bool { return o.kind != OriginDefault }

// String formats the origin.
func (o Origin) String() string {
	switch o.kind {
	case OriginFile:
		return o.file
	case OriginEnv:
		return "env:" + o.env
	default:
		return "default"
	}
}

// Origins maps every config key (dotted) to its origin.
type Origins map[string]Origin

// Of returns the origin of key ("default" when unknown).
func (o Origins) Of(key string) Origin { return o[key] }

// Keys returns the keys in lexical order.
func (o Origins) Keys() []string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// Locked returns the number of keys set by a file or an env var.
func (o Origins) Locked() int {
	n := 0

	for _, v := range o {
		if v.Locked() {
			n++
		}
	}

	return n
}
