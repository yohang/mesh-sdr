package decoder

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxText caps the text of a record (§8.4 "Typed framed output" rule 4).
const MaxText = 4096

// cleanRF turns RF-derived bytes into text (§8.4 rule 4): invalid UTF-8
// replaced, control characters other than tab and newline removed, cut to
// MaxText bytes on a rune boundary.
func cleanRF(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || !unicode.IsControl(r) {
			return r
		}

		return -1
	}, s)

	return capBytes(s, MaxText)
}

// capBytes cuts s to at most n bytes on a rune boundary.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
