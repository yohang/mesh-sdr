// Package commonpw is the bundled list of common passwords that new
// passwords must not match (ACC-011, SR-04). The list is the SecLists
// "xato-net-10-million-passwords-100000" file (MIT, see LICENSE.SecLists),
// reduced by `make vendor-passwords` to the lower-cased entries of at least
// domain.PasswordMinLengthFloor characters: shorter ones already fail the
// length rule.
package commonpw

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// File is the name of the embedded list, written by the vendor tool.
const File = "common-passwords.txt.gz"

//go:embed common-passwords.txt.gz
var embedded []byte

// List is a sorted list of lower-cased passwords.
type List struct{ entries []string }

// Load decodes the embedded list.
func Load() (*List, error) { return Parse(embedded) }

// Parse decodes a gzip-compressed list with one password per line.
func Parse(gz []byte) (*List, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("common password list: %w", err)
	}

	var entries []string

	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			entries = append(entries, strings.ToLower(line))
		}
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("common password list: %w", err)
	}

	if err := zr.Close(); err != nil {
		return nil, fmt.Errorf("common password list: %w", err)
	}

	slices.Sort(entries)

	return &List{entries: slices.Compact(entries)}, nil
}

// Len returns the number of entries.
func (l *List) Len() int { return len(l.entries) }

// Contains reports whether password is in the list (implements
// domain.CommonPasswords). The lookup ignores case, compatibility forms
// (NFKC: full-width letters, ligatures…) and surrounding spaces.
func (l *List) Contains(password string) bool {
	_, ok := slices.BinarySearch(l.entries, strings.ToLower(strings.TrimSpace(norm.NFKC.String(password))))

	return ok
}
