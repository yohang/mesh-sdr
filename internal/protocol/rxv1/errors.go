package rxv1

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrorCode is the stable "code" of an error frame (§6.2).
type ErrorCode string

// Error codes (§6.2 error frame table).
const (
	CodeInvalidJSON        ErrorCode = "invalid_json"
	CodeInvalidEnvelope    ErrorCode = "invalid_envelope"
	CodeInvalidPayload     ErrorCode = "invalid_payload"
	CodeUnsupportedType    ErrorCode = "unsupported_type"
	CodeUnsupportedVersion ErrorCode = "unsupported_version"
	CodeUnauthenticated    ErrorCode = "unauthenticated"
	CodeTokenExpired       ErrorCode = "token_expired"
	CodeTokenInvalid       ErrorCode = "token_invalid"
	CodeForbidden          ErrorCode = "forbidden"
	CodeNotFound           ErrorCode = "not_found"
	CodeConflict           ErrorCode = "conflict"
	CodeCapacityExceeded   ErrorCode = "capacity_exceeded"
	CodeRateLimited        ErrorCode = "rate_limited"
	CodeOutOfRange         ErrorCode = "out_of_range"
	CodePresetIncompatible ErrorCode = "preset_incompatible"
	CodeDeviceUnavailable  ErrorCode = "device_unavailable"
	CodeDemodError         ErrorCode = "demod_error"
	CodeNodeUnavailable    ErrorCode = "node_unavailable"
	CodeHubUnavailable     ErrorCode = "hub_unavailable"
	CodeClockSkew          ErrorCode = "clock_skew"
	CodeInternal           ErrorCode = "internal"
)

var knownErrorCodes = map[ErrorCode]struct{}{
	CodeInvalidJSON: {}, CodeInvalidEnvelope: {}, CodeInvalidPayload: {},
	CodeUnsupportedType: {}, CodeUnsupportedVersion: {},
	CodeUnauthenticated: {}, CodeTokenExpired: {}, CodeTokenInvalid: {},
	CodeForbidden: {}, CodeNotFound: {}, CodeConflict: {}, CodeCapacityExceeded: {},
	CodeRateLimited: {}, CodeOutOfRange: {}, CodePresetIncompatible: {},
	CodeDeviceUnavailable: {}, CodeDemodError: {},
	CodeNodeUnavailable: {}, CodeHubUnavailable: {}, CodeClockSkew: {},
	CodeInternal: {},
}

// Known reports whether c is in the §6.2 catalogue.
func (c ErrorCode) Known() bool {
	_, ok := knownErrorCodes[c]
	return ok
}

// String returns the wire form of c.
func (c ErrorCode) String() string { return string(c) }

// CloseCode returns the close code the sender MUST close with right after
// sending an error with this code, if any (§6.2 "Close?" column, immediate
// cases).
func (c ErrorCode) CloseCode() (CloseCode, bool) {
	switch c {
	case CodeUnsupportedVersion:
		return CloseUnsupportedVersion, true
	case CodeUnauthenticated, CodeTokenExpired, CodeTokenInvalid:
		return CloseUnauthenticated, true
	case CodeNodeUnavailable, CodeHubUnavailable, CodeClockSkew:
		return CloseUnavailable, true
	default:
		return 0, false
	}
}

// EscalationCloseCode returns the close code used once errors with this code
// repeat beyond the policy threshold (§6.2 "After 10 in 60 s", "Repeated").
// The threshold itself is a policy of the caller.
func (c ErrorCode) EscalationCloseCode() (CloseCode, bool) {
	switch c {
	case CodeInvalidJSON, CodeInvalidEnvelope, CodeInvalidPayload:
		return CloseProtocolViolations, true
	case CodeForbidden:
		return CloseForbidden, true
	case CodeRateLimited:
		return CloseRateLimited, true
	default:
		return 0, false
	}
}

// Error is a protocol-level failure carrying a §6.2 error code. Decoders
// return it; transports turn it into an error frame (see ErrorPayloadFrom).
//
// errors.Is(err, ErrInvalidEnvelope) matches any *Error with the same Code.
type Error struct {
	Code ErrorCode
	// Path names the offending field (details.path), "" when not applicable.
	Path string
	// Reason is plain English text, no HTML. It may echo truncated input.
	Reason string
	// ID is the request correlation id when it could be read before the
	// failure, so that the error frame can reference it ("re").
	ID CorrelationID
	// Err is the underlying cause, if any.
	Err error
}

// Sentinels for errors.Is. They match on Code only.
var (
	ErrInvalidJSON        = &Error{Code: CodeInvalidJSON}
	ErrInvalidEnvelope    = &Error{Code: CodeInvalidEnvelope}
	ErrInvalidPayload     = &Error{Code: CodeInvalidPayload}
	ErrUnsupportedType    = &Error{Code: CodeUnsupportedType}
	ErrUnsupportedVersion = &Error{Code: CodeUnsupportedVersion}
)

func (e *Error) Error() string {
	msg := string(e.Code)
	if e.Path != "" {
		msg += " at " + e.Path
	}
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// Is matches any *Error with the same Code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// ErrorPayload is the payload of an "error" frame (§6.2).
type ErrorPayload struct {
	// Re is the request id, nil for fire-and-forget requests (encoded null).
	Re           *CorrelationID `json:"re"`
	Code         ErrorCode      `json:"code"`
	Message      string         `json:"message"`
	Retryable    bool           `json:"retryable"`
	RetryAfterMS *int64         `json:"retry_after_ms"`
	Details      map[string]any `json:"details,omitempty"`
}

// ErrorPayloadFrom maps err to an error frame payload. A *Error keeps its
// code, path and correlation id; anything else becomes "internal" with the
// given incident id (which MUST match a server log line) and no detail of
// the cause.
func ErrorPayloadFrom(err error, incidentID string) ErrorPayload {
	var pe *Error
	if !errors.As(err, &pe) {
		return ErrorPayload{
			Code:    CodeInternal,
			Message: "Internal error",
			Details: map[string]any{"incident_id": incidentID},
		}
	}
	p := ErrorPayload{Code: pe.Code, Message: pe.Reason}
	if p.Message == "" {
		p.Message = string(pe.Code)
	}
	if !pe.ID.IsZero() {
		id := pe.ID
		p.Re = &id
	}
	if pe.Path != "" {
		p.Details = map[string]any{"path": pe.Path}
	}
	return p
}

// NewErrorEnvelope builds an "error" envelope.
func NewErrorEnvelope(ts int64, p ErrorPayload) (Envelope, error) {
	if !p.Code.Known() {
		return Envelope{}, fmt.Errorf("rxv1: unknown error code %q", p.Code)
	}
	if p.Code == CodeRateLimited && p.RetryAfterMS == nil {
		return Envelope{}, errors.New("rxv1: rate_limited requires retry_after_ms")
	}
	return NewEnvelope(TypeError, CorrelationID{}, ts, p)
}

// AckPayload is the payload of an "ack" frame (§6.2).
type AckPayload struct {
	Re     CorrelationID   `json:"re"`
	Result json.RawMessage `json:"result"`
}

// NewAckEnvelope builds an "ack" for request re. result is marshalled to a
// JSON object; nil means {}.
func NewAckEnvelope(ts int64, re CorrelationID, result any) (Envelope, error) {
	if re.IsZero() {
		return Envelope{}, errors.New("rxv1: ack requires a correlation id")
	}
	raw, err := marshalObject(result)
	if err != nil {
		return Envelope{}, fmt.Errorf("rxv1: ack result: %w", err)
	}
	return NewEnvelope(TypeAck, CorrelationID{}, ts, AckPayload{Re: re, Result: raw})
}
