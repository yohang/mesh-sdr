// Package domain is the shared kernel: value objects and the domain error type
// used by every module.
package domain

import "strings"

// Kind classifies a domain error. Boundaries map kinds to transport responses
// (for example an HTTP status) without knowing every error code.
type Kind int

// Error kinds.
const (
	KindInvalid Kind = iota + 1
	KindNotFound
	KindConflict
	KindForbidden
	KindUnauthenticated
	KindUnavailable
	// KindRateLimited means the caller must wait before retrying.
	KindRateLimited
)

// String returns the snake_case name of the kind.
func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid"
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindForbidden:
		return "forbidden"
	case KindUnauthenticated:
		return "unauthenticated"
	case KindUnavailable:
		return "unavailable"
	case KindRateLimited:
		return "rate_limited"
	default:
		return "unknown"
	}
}

// Code is a stable, snake_case error code (for example "invalid_node_id").
// Codes are part of the public API: never rename one.
type Code string

// Violation describes one invalid field of an input.
type Violation struct {
	path    string
	code    Code
	message string
}

// NewViolation returns a violation of the field at path.
func NewViolation(path string, code Code, message string) Violation {
	return Violation{path: path, code: code, message: message}
}

// Path returns the dotted path of the invalid field.
func (v Violation) Path() string { return v.path }

// Code returns the violation code.
func (v Violation) Code() Code { return v.code }

// Message returns the human-readable message.
func (v Violation) Message() string { return v.message }

// Error is a typed domain error with a stable code.
//
// Declare sentinels with NewError in the module's domain package and check
// them with errors.Is: two errors match when their codes are equal, so a
// sentinel refined with WithDetail or WithViolations still matches it.
type Error struct {
	kind       Kind
	code       Code
	message    string
	violations []Violation
}

// NewError returns a domain error.
func NewError(kind Kind, code Code, message string) *Error {
	return &Error{kind: kind, code: code, message: message}
}

// WithDetail returns a copy of e with a more specific message.
func (e *Error) WithDetail(message string) *Error {
	c := *e
	c.message = message

	return &c
}

// WithViolations returns a copy of e carrying the given field violations.
func (e *Error) WithViolations(violations ...Violation) *Error {
	c := *e
	c.violations = append([]Violation(nil), violations...)

	return &c
}

// Kind returns the error kind.
func (e *Error) Kind() Kind { return e.kind }

// Code returns the stable error code.
func (e *Error) Code() Code { return e.code }

// Message returns the human-readable message.
func (e *Error) Message() string { return e.message }

// Violations returns the field violations, if any.
func (e *Error) Violations() []Violation {
	return append([]Violation(nil), e.violations...)
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder

	b.WriteString(string(e.code))
	if e.message != "" {
		b.WriteString(": ")
		b.WriteString(e.message)
	}

	for _, v := range e.violations {
		b.WriteString("; ")
		b.WriteString(v.path)
		b.WriteString(": ")
		b.WriteString(v.message)
	}

	return b.String()
}

// Is reports whether target is a domain error with the same code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)

	return ok && t.code == e.code
}
