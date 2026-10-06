package app

import (
	"golang.org/x/text/unicode/norm"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// normalizePassword returns the Unicode NFC form of a password: the same
// characters typed on systems that compose them differently hash the same.
// Every password is normalised before it is checked, hashed or verified.
func normalizePassword(s string) string { return norm.NFC.String(s) }

// newPassword normalises a new password and checks it against the policy.
func newPassword(s string, p domain.PasswordPolicy) (domain.Password, error) {
	return domain.NewPassword(normalizePassword(s), p)
}
