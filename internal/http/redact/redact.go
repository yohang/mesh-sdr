// Package redact keeps the single-use tokens of setup, invitation, password
// reset and e-mail confirmation links out of logs and redirects (SR-07).
package redact

import "strings"

// TokenPrefixes are the path prefixes followed by a single-use token.
var TokenPrefixes = []string{
	"/setup/",
	"/invite/",
	"/password/reset/",
	"/account/email/verify/",
}

// Path returns p with everything after a token prefix replaced by
// "{token}". The prefix is searched anywhere in the path and regardless of
// case, so that unrouted variants (//setup/…, /SETUP/…, /x/setup/…) are
// redacted too.
func Path(p string) string {
	lower := strings.ToLower(p)
	cut := -1

	for _, prefix := range TokenPrefixes {
		if i := strings.Index(lower, prefix); i >= 0 && (cut < 0 || i+len(prefix) < cut) {
			cut = i + len(prefix)
		}
	}

	if cut < 0 || cut == len(p) {
		return p
	}

	return p[:cut] + "{token}"
}

// HasToken reports whether p carries a single-use token.
func HasToken(p string) bool { return Path(p) != p }
