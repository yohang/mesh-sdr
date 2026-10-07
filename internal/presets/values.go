package presets

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

// Slugify derives a slug from a name ("preset" when nothing remains).
func Slugify(name string) string {
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

	return s
}

// SlugWithSuffix returns slug followed by "-n", kept within 64 characters.
func SlugWithSuffix(slug string, n int) string {
	suffix := "-" + strconv.Itoa(n)

	if len(slug)+len(suffix) > 64 {
		slug = strings.TrimRight(slug[:64-len(suffix)], "-")
	}

	return slug + suffix
}

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

// checkTags validates up to 32 plain-text tags of 1 to 32 characters and
// returns them unique and sorted.
func checkTags(tags []string) ([]string, []shared.Violation) {
	if len(tags) > MaxTags {
		return nil, []shared.Violation{shared.NewViolation("tags", "too_many", "at most 32 tags")}
	}

	out := make([]string, 0, len(tags))

	for i, t := range tags {
		v, bad := plainText("tags."+strconv.Itoa(i), t, MaxTagLength, true)
		if bad != nil {
			return nil, []shared.Violation{*bad}
		}

		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}

	slices.Sort(out)

	return out, nil
}
