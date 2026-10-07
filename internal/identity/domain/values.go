package domain

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{2,64}$`)

// Username is a unique, case-insensitive account name (TECHNICAL_SPEC §7.1).
type Username struct{ v string }

// NewUsername validates a username.
func NewUsername(s string) (Username, error) {
	if !usernamePattern.MatchString(s) {
		return Username{}, ErrInvalidUsername
	}

	return Username{v: s}, nil
}

// String returns the username as chosen.
func (u Username) String() string { return u.v }

// Key returns the case-folded username used for uniqueness and lookups.
func (u Username) Key() string { return strings.ToLower(u.v) }

// Equal reports whether both usernames are the same account name.
func (u Username) Equal(o Username) bool { return u.Key() == o.Key() }

// IsZero reports whether u is the zero value.
func (u Username) IsZero() bool { return u.v == "" }

// Email is an e-mail address (unique, case-insensitive).
type Email struct{ v string }

// NewEmail validates a bare e-mail address (no display name).
func NewEmail(s string) (Email, error) {
	if s == "" || len(s) > 254 || strings.TrimSpace(s) != s {
		return Email{}, ErrInvalidEmail
	}

	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return Email{}, ErrInvalidEmail
	}

	return Email{v: s}, nil
}

// String returns the address as given.
func (e Email) String() string { return e.v }

// Key returns the case-folded address used for uniqueness and lookups.
func (e Email) Key() string { return strings.ToLower(e.v) }

// IsZero reports whether e is the zero value (no e-mail).
func (e Email) IsZero() bool { return e.v == "" }

// DisplayName is a free display name, rendered as plain text.
type DisplayName struct{ v string }

// NewDisplayName validates a display name: 1 to 64 characters, valid UTF-8,
// no control characters, not only spaces.
func NewDisplayName(s string) (DisplayName, error) {
	if !utf8.ValidString(s) || strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > 64 {
		return DisplayName{}, ErrInvalidDisplayName
	}

	for _, r := range s {
		if unicode.IsControl(r) {
			return DisplayName{}, ErrInvalidDisplayName
		}
	}

	return DisplayName{v: s}, nil
}

// String returns the display name.
func (d DisplayName) String() string { return d.v }

// IsZero reports whether d is the zero value (no display name).
func (d DisplayName) IsZero() bool { return d.v == "" }

// Login is what a user types to sign in: a username or an e-mail address.
type Login struct{ v string }

// NewLogin normalises a typed login (surrounding spaces removed).
func NewLogin(s string) (Login, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 254 || !utf8.ValidString(s) {
		return Login{}, ErrInvalidLogin
	}

	return Login{v: s}, nil
}

// String returns the login as typed (trimmed).
func (l Login) String() string { return l.v }

// IsEmail reports whether the login is an e-mail address: usernames cannot
// contain '@'.
func (l Login) IsEmail() bool { return strings.Contains(l.v, "@") }

// Key is the normalised identifier that per-account throttling is keyed by
// (SR-05: identifier as typed, normalised).
func (l Login) Key() string { return strings.ToLower(l.v) }

// CommonPasswords is a list of common and breached passwords that new
// passwords must not match (ACC-011, SR-04). The match ignores case.
type CommonPasswords interface {
	Contains(password string) bool
}

// PasswordPolicy is the local password policy (ACC-011, SR-04): a minimum
// and a maximum length in characters, no composition rules, and no common
// password. It applies to every new password: CLI, password change, reset,
// invitation acceptance and setup.
type PasswordPolicy struct {
	minLength int
	maxLength int
	common    CommonPasswords
}

// Password length bounds.
const (
	DefaultPasswordMinLength = 10
	PasswordMinLengthFloor   = 8
	PasswordMaxLength        = 256
)

// Reasons a new password is refused: the code of the "password" violation
// of ErrInvalidPassword.
const (
	PasswordTooShort      shared.Code = "too_short"
	PasswordTooLong       shared.Code = "too_long"
	PasswordNotUTF8       shared.Code = "invalid_utf8"
	PasswordCommon        shared.Code = "common"
	PasswordSameAsCurrent shared.Code = "same_as_current"
)

// NewPasswordPolicy returns a policy with the given minimum length (never
// below PasswordMinLengthFloor) and a maximum of PasswordMaxLength, without
// a common-password list.
func NewPasswordPolicy(minLength int) PasswordPolicy {
	return PasswordPolicy{minLength: max(minLength, PasswordMinLengthFloor), maxLength: PasswordMaxLength}
}

// DefaultPasswordPolicy returns the policy with the default minimum length.
func DefaultPasswordPolicy() PasswordPolicy { return NewPasswordPolicy(DefaultPasswordMinLength) }

// WithCommonPasswords returns a copy of p that also refuses the passwords of
// list.
func (p PasswordPolicy) WithCommonPasswords(list CommonPasswords) PasswordPolicy {
	p.common = list

	return p
}

// MinLength returns the minimum length in characters.
func (p PasswordPolicy) MinLength() int { return p.minLength }

// MaxLength returns the maximum length in characters.
func (p PasswordPolicy) MaxLength() int { return p.maxLength }

// Password is a new cleartext password that follows the policy. It never
// prints its value.
type Password struct{ v string }

// PasswordRefused returns ErrInvalidPassword with the reason as the code of
// its "password" violation.
func PasswordRefused(reason shared.Code, message string) error {
	return ErrInvalidPassword.WithDetail(message).WithViolations(shared.NewViolation("password", reason, message))
}

// NewPassword checks a new password against the policy. Any Unicode
// character is accepted; length is counted in characters.
func NewPassword(s string, p PasswordPolicy) (Password, error) {
	if p.minLength == 0 {
		p = DefaultPasswordPolicy().WithCommonPasswords(p.common)
	}

	if !utf8.ValidString(s) {
		return Password{}, PasswordRefused(PasswordNotUTF8, "the password is not valid UTF-8")
	}

	switch n := utf8.RuneCountInString(s); {
	case n < p.minLength:
		return Password{}, PasswordRefused(PasswordTooShort, fmt.Sprintf("the password must have at least %d characters", p.minLength))
	case n > p.maxLength:
		return Password{}, PasswordRefused(PasswordTooLong, fmt.Sprintf("the password must have at most %d characters", p.maxLength))
	}

	if p.common != nil && p.common.Contains(s) {
		return Password{}, PasswordRefused(PasswordCommon, "this password is too common, choose another one")
	}

	return Password{v: s}, nil
}

// Reveal returns the cleartext, for hashing only.
func (p Password) Reveal() string { return p.v }

// String redacts the password.
func (p Password) String() string { return "<redacted>" }

// PasswordHash is a stored password hash in PHC string format. Its content
// is checked by the hasher at verification time: a malformed hash makes
// only that account unusable (AUTH-017).
type PasswordHash struct{ v string }

// NewPasswordHash wraps a PHC string.
func NewPasswordHash(s string) (PasswordHash, error) {
	if !strings.HasPrefix(s, "$") || len(s) > 1024 {
		return PasswordHash{}, ErrInvalidHash
	}

	return PasswordHash{v: s}, nil
}

// String returns the PHC string.
func (h PasswordHash) String() string { return h.v }

// IsZero reports whether h is the zero value (no local password).
func (h PasswordHash) IsZero() bool { return h.v == "" }
