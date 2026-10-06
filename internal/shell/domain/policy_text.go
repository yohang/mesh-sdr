package domain

import (
	"strconv"
	"strings"
	"unicode/utf8"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// MaxPolicyTextLength is the maximum length of a usage policy, in characters.
const MaxPolicyTextLength = 20000

// ErrInvalidPolicyText is returned for an empty or too long usage policy.
var ErrInvalidPolicyText = shared.NewError(shared.KindInvalid, "invalid_usage_policy",
	"usage policy must be 1 to "+strconv.Itoa(MaxPolicyTextLength)+" characters")

// PolicyText is the usage policy shown at /policy (UI-003): Markdown source,
// rendered without raw HTML.
type PolicyText struct {
	markdown string
}

// NewPolicyText validates s and returns it as a PolicyText. Surrounding
// whitespace is trimmed.
func NewPolicyText(s string) (PolicyText, error) {
	s = strings.TrimSpace(s)

	if n := utf8.RuneCountInString(s); n == 0 || n > MaxPolicyTextLength || !utf8.ValidString(s) {
		return PolicyText{}, ErrInvalidPolicyText
	}

	return PolicyText{markdown: s}, nil
}

// MustPolicyText is NewPolicyText that panics on error. Tests and constants only.
func MustPolicyText(s string) PolicyText {
	p, err := NewPolicyText(s)
	if err != nil {
		panic(err)
	}

	return p
}

// Markdown returns the Markdown source.
func (p PolicyText) Markdown() string { return p.markdown }
