// Package audit is the audit port shared by the modules (TECHNICAL_SPEC
// §7.1 audit_log): one record shape and one Appender, implemented once over
// identity's audit_log by the composition root.
package audit

import "context"

// Actor is who performed an audited action.
type Actor string

// Actors. Caller (the zero value) is the caller of the request in ctx: the
// signed-in user, or an anonymous client, with its address and request id.
const (
	Caller Actor = ""
	System Actor = "system"
	CLI    Actor = "cli"
)

// Result is the outcome of an audited action.
type Result string

// Results. The zero value is ResultOK.
const (
	ResultOK     Result = "ok"
	ResultDenied Result = "denied"
	ResultError  Result = "error"
)

// Record is one audited action. Values in Before and After must never hold
// secrets (mask them first).
type Record struct {
	Actor      Actor
	Action     string // dotted verb, such as "preset.update"
	Result     Result
	TargetType string
	TargetID   string
	Before     map[string]string
	After      map[string]string
}

// Appender appends audit records, inside the caller's transaction when ctx
// carries one.
type Appender interface {
	Append(ctx context.Context, r Record) error
}

// Records is an in-memory Appender, for tests.
type Records []Record

// Append implements Appender.
func (r *Records) Append(_ context.Context, rec Record) error {
	*r = append(*r, rec)

	return nil
}
