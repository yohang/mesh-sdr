package decoder

import (
	"strings"
	"unicode"
)

// maxText caps the text of a record (§8.4 "Typed framed output" rule 4):
// sessionBase.decode cuts it on a rune boundary.
const maxText = 4096

// cleanRF turns RF-derived bytes into text (§8.4 rule 4): invalid UTF-8
// replaced, control characters other than tab and newline removed.
func cleanRF(s string) string {
	s = strings.ToValidUTF8(s, "�")

	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || !unicode.IsControl(r) {
			return r
		}

		return -1
	}, s)
}
