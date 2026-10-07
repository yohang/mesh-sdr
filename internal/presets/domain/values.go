// Package domain models presets (TECHNICAL_SPEC §7.1 `presets`, ADR 0020):
// device-independent tuning data, validated against a device's reported
// limits when it is applied.
package domain

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Limits of the preset fields.
const (
	MaxNameLength        = 128
	MaxDescriptionLength = 1024
	MaxTags              = 32
	MaxTagLength         = 32
	// MaxFrequency bounds frequencies (300 GHz).
	MaxFrequency   = 300_000_000_000
	maxInt32       = 2147483647
	MinSquelch     = -150
	MaxSquelch     = 0
	MinNRLevel     = -20
	MaxNRLevel     = 20
	MinWaterfallDB = -200
	MaxWaterfallDB = 50
)

var (
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	modePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,23}$`)
)

// Slug is the unique, URL-safe handle of a preset.
type Slug struct{ value string }

// NewSlug validates s.
func NewSlug(s string) (Slug, error) {
	if !slugPattern.MatchString(s) {
		return Slug{}, ErrInvalidPreset.WithViolations(shared.NewViolation("slug", "invalid_slug",
			"lower-case letters, digits and hyphens, 1 to 64 characters, starting with a letter or a digit"))
	}

	return Slug{value: s}, nil
}

// Slugify derives a slug from a name ("preset" when nothing remains).
func Slugify(name string) Slug {
	var b strings.Builder

	dash := false

	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)

			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')

			dash = true
		}
	}

	s := strings.Trim(b.String(), "-")
	if len(s) > 56 {
		s = strings.Trim(s[:56], "-")
	}

	if s == "" {
		s = "preset"
	}

	return Slug{value: s}
}

// WithSuffix returns the slug followed by "-n", kept within 64 characters.
func (s Slug) WithSuffix(n int) Slug {
	suffix := "-" + strconv.Itoa(n)
	base := s.value

	if len(base)+len(suffix) > 64 {
		base = strings.TrimRight(base[:64-len(suffix)], "-")
	}

	return Slug{value: base + suffix}
}

// String returns the slug.
func (s Slug) String() string { return s.value }

// plainText checks a plain-text field: valid UTF-8, no control characters,
// at most max characters.
func plainText(path, s string, maxLen int, required bool) (string, *shared.Violation) {
	s = strings.TrimSpace(s)

	switch {
	case required && s == "":
		v := shared.NewViolation(path, "required", "required")

		return "", &v
	case !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		v := shared.NewViolation(path, "invalid_text", "plain text without control characters")

		return "", &v
	case utf8.RuneCountInString(s) > maxLen:
		v := shared.NewViolation(path, "too_long", "at most "+strconv.Itoa(maxLen)+" characters")

		return "", &v
	}

	return s, nil
}

// Name is the display name of a preset.
type Name struct{ value string }

// NewName validates a plain-text name of 1 to 128 characters.
func NewName(s string) (Name, error) {
	v, bad := plainText("name", s, MaxNameLength, true)
	if bad != nil {
		return Name{}, ErrInvalidPreset.WithViolations(*bad)
	}

	return Name{value: v}, nil
}

// String returns the name.
func (n Name) String() string { return n.value }

// ModeID is a mode of the mode catalogue (§7.1 presets.start_mod). Only its
// syntax is checked: the catalogue comes with the demodulation epics (ADR
// 0020 Q9).
type ModeID struct{ value string }

// NewModeID validates a mode id.
func NewModeID(s string) (ModeID, error) {
	if !modePattern.MatchString(s) {
		return ModeID{}, ErrInvalidPreset.WithViolations(shared.NewViolation("start_mod", "invalid_mode",
			"a mode id: lower-case letters, digits, '_' and '-', 1 to 24 characters"))
	}

	return ModeID{value: s}, nil
}

// String returns the mode id.
func (m ModeID) String() string { return m.value }

// WaterfallLevels are the waterfall levels of a preset, in dB.
type WaterfallLevels struct{ min, max int }

// NewWaterfallLevels validates min < max, both within −200..50 dB.
func NewWaterfallLevels(minDB, maxDB int) (WaterfallLevels, error) {
	if minDB < MinWaterfallDB || maxDB > MaxWaterfallDB || minDB >= maxDB {
		return WaterfallLevels{}, ErrInvalidPreset.WithViolations(shared.NewViolation("waterfall_levels", "out_of_range",
			"min must be below max, both between -200 and 50 dB"))
	}

	return WaterfallLevels{min: minDB, max: maxDB}, nil
}

// Min returns the lower level.
func (w WaterfallLevels) Min() int { return w.min }

// Max returns the upper level.
func (w WaterfallLevels) Max() int { return w.max }

// Tags are the filter tags of a preset: plain text, unique, sorted.
type Tags struct{ values []string }

// NewTags validates up to 32 tags of 1 to 32 characters.
func NewTags(tags []string) (Tags, error) {
	if len(tags) > MaxTags {
		return Tags{}, ErrInvalidPreset.WithViolations(shared.NewViolation("tags", "too_many", "at most 32 tags"))
	}

	out := make([]string, 0, len(tags))

	for i, t := range tags {
		v, bad := plainText("tags."+strconv.Itoa(i), t, MaxTagLength, true)
		if bad != nil {
			return Tags{}, ErrInvalidPreset.WithViolations(*bad)
		}

		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}

	slices.Sort(out)

	return Tags{values: out}, nil
}

// Values returns the tags.
func (t Tags) Values() []string { return slices.Clone(t.values) }
