package domain

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// MaxKeyLength bounds a key (`settings.key` is STRING(160)).
const MaxKeyLength = 160

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// ValidKey reports whether s is a setting key: the dotted path of the
// settings schema without the `settings.` prefix of hub.toml
// ("ui.theme_mode", "listen_policy"), at most MaxKeyLength characters.
func ValidKey(s string) bool { return len(s) <= MaxKeyLength && keyPattern.MatchString(s) }

// ConfigKey returns the key in hub.toml ("settings.ui.theme_mode").
func ConfigKey(key string) string { return "settings." + key }

// Value is a setting value: compact JSON. It holds the value as written
// (durations keep their units, "30d"), not a re-encoding of the Go type.
type Value struct{ raw string }

// NewValue validates and compacts JSON. Null is a valid value (the default
// of an optional key); writes use a nil *Value to reset a key instead.
func NewValue(raw []byte) (Value, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, bytes.TrimSpace(raw)); err != nil || buf.Len() == 0 {
		return Value{}, ErrInvalidValue
	}

	return Value{raw: buf.String()}, nil
}

// MustValue is NewValue that panics. Constants and tests only.
func MustValue(raw string) Value {
	v, err := NewValue([]byte(raw))
	if err != nil {
		panic(err)
	}

	return v
}

// ValueOf encodes a Go value.
func ValueOf(v any) (Value, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Value{}, ErrInvalidValue.WithDetail(err.Error())
	}

	return NewValue(b)
}

// JSON returns the value as JSON.
func (v Value) JSON() json.RawMessage {
	if v.raw == "" {
		return json.RawMessage("null")
	}

	return json.RawMessage(v.raw)
}

// String returns the JSON text.
func (v Value) String() string { return string(v.JSON()) }

// IsNull reports whether the value is JSON null (or the zero Value).
func (v Value) IsNull() bool { return v.raw == "" || v.raw == "null" }

// Equal reports whether two values have the same JSON text.
func (v Value) Equal(o Value) bool { return v.String() == o.String() }
