package config

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
)

// Unit formats (TECHNICAL_SPEC §7.4 "Format" rule 4).
const (
	durationPattern  = `^([0-9]+(ms|s|m|h|d|w))+$`
	sizePattern      = `^[0-9]+(B|kB|MB|GB|TB|KiB|MiB|GiB|TiB)?$`
	frequencyPattern = `^[0-9]+(\.[0-9]+)?(Hz|kHz|MHz|GHz)$`
)

var (
	durationPartRe = regexp.MustCompile(`([0-9]+)(ms|s|m|h|d|w)`)
	durationRe     = regexp.MustCompile(durationPattern)
	sizeRe         = regexp.MustCompile(`^([0-9]+)(B|kB|MB|GB|TB|KiB|MiB|GiB|TiB)?$`)
	frequencyRe    = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?(Hz|kHz|MHz|GHz)$`)

	errOverflow = errors.New("value out of range")
)

// Duration is a non-negative duration written with units: "15s", "7d", "1h30m".
// Units: ms, s, m, h, d (24 h), w (7 d).
type Duration struct {
	d time.Duration
}

// ParseDuration parses a duration string.
func ParseDuration(s string) (Duration, error) {
	if !durationRe.MatchString(s) {
		return Duration{}, fmt.Errorf("invalid duration %q: want <int><unit>[...] with units ms, s, m, h, d, w (for example \"15s\", \"7d\", \"1h30m\")", s)
	}

	units := map[string]time.Duration{
		"ms": time.Millisecond, "s": time.Second, "m": time.Minute,
		"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
	}

	var total time.Duration

	for _, m := range durationPartRe.FindAllStringSubmatch(s, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return Duration{}, fmt.Errorf("invalid duration %q: %w", s, errOverflow)
		}

		u := units[m[2]]
		if n > int64(math.MaxInt64/u) || total > math.MaxInt64-time.Duration(n)*u {
			return Duration{}, fmt.Errorf("invalid duration %q: %w", s, errOverflow)
		}

		total += time.Duration(n) * u
	}

	return Duration{d: total}, nil
}

// MustDuration is ParseDuration that panics on error. Defaults and tests only.
func MustDuration(s string) Duration {
	d, err := ParseDuration(s)
	if err != nil {
		panic(err)
	}

	return d
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return d.d }

// String formats the duration with the largest exact units.
func (d Duration) String() string {
	if d.d == 0 {
		return "0s"
	}

	var b strings.Builder

	// Weeks only for a whole number of weeks: "30d" rather than "4w2d".
	const week = 7 * 24 * time.Hour
	if d.d%week == 0 {
		return strconv.FormatInt(int64(d.d/week), 10) + "w"
	}

	rest := d.d
	for _, u := range []struct {
		name string
		d    time.Duration
	}{
		{"d", 24 * time.Hour}, {"h", time.Hour},
		{"m", time.Minute}, {"s", time.Second}, {"ms", time.Millisecond},
	} {
		if n := rest / u.d; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.name)
			rest -= n * u.d
		}
	}

	return b.String()
}

// UnmarshalText implements encoding.TextUnmarshaler (TOML strings and env vars).
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := ParseDuration(string(text))
	if err != nil {
		return err
	}

	*d = v

	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// JSONSchema describes the type in the generated config schema.
func (Duration) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: durationPattern, Examples: []any{"15s", "7d", "1h30m"}}
}

// Size is a byte size: an integer (bytes) or a string with a unit, IEC
// (KiB, MiB, GiB, TiB) or SI (kB, MB, GB, TB): "32MiB".
type Size struct {
	bytes int64
}

// ParseSize parses a size string.
func ParseSize(s string) (Size, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return Size{}, fmt.Errorf("invalid size %q: want <int>[unit] with units B, kB, MB, GB, TB, KiB, MiB, GiB, TiB (for example \"32MiB\")", s)
	}

	units := map[string]int64{
		"": 1, "B": 1,
		"kB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
	}

	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return Size{}, fmt.Errorf("invalid size %q: %w", s, errOverflow)
	}

	u := units[m[2]]
	if n > math.MaxInt64/u {
		return Size{}, fmt.Errorf("invalid size %q: %w", s, errOverflow)
	}

	return Size{bytes: n * u}, nil
}

// MustSize is ParseSize that panics on error. Defaults and tests only.
func MustSize(s string) Size {
	v, err := ParseSize(s)
	if err != nil {
		panic(err)
	}

	return v
}

// Bytes returns the size in bytes.
func (s Size) Bytes() int64 { return s.bytes }

// String formats the size with the largest exact IEC unit.
func (s Size) String() string {
	for _, u := range []struct {
		name string
		n    int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if s.bytes != 0 && s.bytes%u.n == 0 {
			return strconv.FormatInt(s.bytes/u.n, 10) + u.name
		}
	}

	return strconv.FormatInt(s.bytes, 10) + "B"
}

// UnmarshalText implements encoding.TextUnmarshaler (env vars).
func (s *Size) UnmarshalText(text []byte) error {
	v, err := ParseSize(string(text))
	if err != nil {
		return err
	}

	*s = v

	return nil
}

// UnmarshalTOML implements toml.Unmarshaler: an integer (bytes) or a string.
func (s *Size) UnmarshalTOML(data any) error {
	switch v := data.(type) {
	case int64:
		if v < 0 {
			return fmt.Errorf("invalid size %d: must not be negative", v)
		}

		s.bytes = v

		return nil
	case string:
		return s.UnmarshalText([]byte(v))
	default:
		return fmt.Errorf("invalid size %v: want an integer or a string", data)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (s Size) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// JSONSchema describes the type in the generated config schema.
func (Size) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "integer", Minimum: "0"},
			{Type: "string", Pattern: sizePattern},
		},
		Examples: []any{"32MiB", 1048576},
	}
}

// Frequency is a frequency in Hz: an integer (Hz) or a string with a unit
// (Hz, kHz, MHz, GHz): "145.800MHz". The value must be a whole number of Hz.
type Frequency struct {
	hz int64
}

// ParseFrequency parses a frequency string. A bare integer is in Hz.
func ParseFrequency(s string) (Frequency, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 {
			return Frequency{}, fmt.Errorf("invalid frequency %q: must not be negative", s)
		}

		return Frequency{hz: n}, nil
	}

	m := frequencyRe.FindStringSubmatch(s)
	if m == nil {
		return Frequency{}, fmt.Errorf("invalid frequency %q: want an integer in Hz or <number><unit> with units Hz, kHz, MHz, GHz (for example \"145.800MHz\")", s)
	}

	exp := map[string]int{"Hz": 0, "kHz": 3, "MHz": 6, "GHz": 9}[m[3]]

	frac := m[2]
	if len(frac) > exp {
		if strings.Trim(frac[exp:], "0") != "" {
			return Frequency{}, fmt.Errorf("invalid frequency %q: not a whole number of Hz", s)
		}

		frac = frac[:exp]
	}

	digits := m[1] + frac + strings.Repeat("0", exp-len(frac))

	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return Frequency{}, fmt.Errorf("invalid frequency %q: %w", s, errOverflow)
	}

	return Frequency{hz: n}, nil
}

// MustFrequency is ParseFrequency that panics on error. Defaults and tests only.
func MustFrequency(s string) Frequency {
	v, err := ParseFrequency(s)
	if err != nil {
		panic(err)
	}

	return v
}

// Hz returns the frequency in Hz.
func (f Frequency) Hz() int64 { return f.hz }

// String returns the frequency in Hz.
func (f Frequency) String() string { return strconv.FormatInt(f.hz, 10) }

// UnmarshalText implements encoding.TextUnmarshaler (env vars).
func (f *Frequency) UnmarshalText(text []byte) error {
	v, err := ParseFrequency(string(text))
	if err != nil {
		return err
	}

	*f = v

	return nil
}

// UnmarshalTOML implements toml.Unmarshaler: an integer (Hz) or a string.
func (f *Frequency) UnmarshalTOML(data any) error {
	switch v := data.(type) {
	case int64:
		return f.UnmarshalText([]byte(strconv.FormatInt(v, 10)))
	case string:
		return f.UnmarshalText([]byte(v))
	default:
		return fmt.Errorf("invalid frequency %v: want an integer or a string", data)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (f Frequency) MarshalText() ([]byte, error) { return []byte(f.String()), nil }

// JSONSchema describes the type in the generated config schema.
func (Frequency) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "integer", Minimum: "0"},
			{Type: "string", Pattern: frequencyPattern},
		},
		Examples: []any{"145.800MHz", 7074000},
	}
}
