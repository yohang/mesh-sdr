package config

import (
	"encoding/json"
	"strings"
)

// Entry is one config-only key of a loaded configuration, for the
// effective configuration view (ADM-010): its value (secrets masked), and
// where it comes from.
type Entry struct {
	Key    string
	Value  json.RawMessage // null for an unset or secret value
	Origin Origin
	Secret bool
}

// BootstrapEntries lists the config-only keys of a loaded hub config (every
// key outside [settings]) in field order, with their origin. Secret values
// are never copied: only whether they are set.
func BootstrapEntries(cfg Hub, origins Origins) []Entry {
	var out []Entry

	for _, lf := range leaves(&cfg) {
		if isFileMetaKey(lf.key) || strings.HasPrefix(lf.key, settingsPrefix) {
			continue
		}

		e := Entry{Key: lf.key, Origin: origins.Of(lf.key), Value: json.RawMessage("null")}

		if s, ok := lf.value.Interface().(Secret); ok {
			e.Secret = true
			if s.IsSet() {
				e.Value = json.RawMessage("true")
			}
		} else if b, err := json.Marshal(lf.value.Interface()); err == nil {
			e.Value = b
		}

		out = append(out, e)
	}

	return out
}
