package rxv1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// CorrelationID is the optional envelope "id" (§6.2): a non-empty string of
// at most 64 characters. The zero value means "absent".
type CorrelationID struct{ s string }

// ParseCorrelationID validates s.
func ParseCorrelationID(s string) (CorrelationID, error) {
	if s == "" {
		return CorrelationID{}, &Error{Code: CodeInvalidEnvelope, Path: "id", Reason: "id must not be empty"}
	}
	if utf8.RuneCountInString(s) > MaxCorrelationIDLen {
		return CorrelationID{}, &Error{Code: CodeInvalidEnvelope, Path: "id", Reason: "id longer than 64 characters"}
	}
	return CorrelationID{s: s}, nil
}

// MustCorrelationID panics on an invalid id. Tests and constants only.
func MustCorrelationID(s string) CorrelationID {
	id, err := ParseCorrelationID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// IsZero reports whether the id is absent.
func (id CorrelationID) IsZero() bool { return id.s == "" }

// String returns the wire form ("" when absent).
func (id CorrelationID) String() string { return id.s }

// MarshalJSON encodes the id as a JSON string.
func (id CorrelationID) MarshalJSON() ([]byte, error) { return json.Marshal(id.s) }

// UnmarshalJSON decodes and validates a JSON string id.
func (id *CorrelationID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := ParseCorrelationID(s)
	if err != nil {
		return err
	}
	*id = v
	return nil
}

// Envelope is one JSON text frame (§6.2). Construct it with NewEnvelope or
// DecodeEnvelope; an existing Envelope is always valid.
type Envelope struct {
	typ     MessageType
	id      CorrelationID
	ts      int64
	payload json.RawMessage
}

// NewEnvelope builds an envelope. payload is marshalled and MUST encode to a
// JSON object; nil encodes to {}. id may be the zero value.
func NewEnvelope(typ MessageType, id CorrelationID, ts int64, payload any) (Envelope, error) {
	if !validMessageType(string(typ)) {
		return Envelope{}, fmt.Errorf("rxv1: invalid message type %q", typ)
	}
	raw, err := marshalObject(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("rxv1: %s payload: %w", typ, err)
	}
	return Envelope{typ: typ, id: id, ts: ts, payload: raw}, nil
}

// Type returns the message type.
func (e Envelope) Type() MessageType { return e.typ }

// ID returns the correlation id and whether it is present.
func (e Envelope) ID() (CorrelationID, bool) { return e.id, !e.id.IsZero() }

// TS returns the sender time in ms since the Unix epoch.
func (e Envelope) TS() int64 { return e.ts }

// Payload returns the raw payload object. Callers MUST NOT modify it.
func (e Envelope) Payload() json.RawMessage { return e.payload }

type wireEnvelope struct {
	V       int             `json:"v"`
	Type    MessageType     `json:"type"`
	ID      *CorrelationID  `json:"id,omitempty"`
	TS      int64           `json:"ts"`
	Payload json.RawMessage `json:"payload"`
}

// MarshalJSON encodes the envelope with fields in spec order.
func (e Envelope) MarshalJSON() ([]byte, error) {
	if e.typ == "" {
		return nil, errors.New("rxv1: zero Envelope")
	}
	w := wireEnvelope{V: Version, Type: e.typ, TS: e.ts, Payload: e.payload}
	if !e.id.IsZero() {
		id := e.id
		w.ID = &id
	}
	return json.Marshal(w)
}

// DecodePayload decodes the payload into dst. With strict, unknown fields are
// rejected (client → server direction, §6.2); without, they are ignored
// (server → client, forward compatibility). Failures are invalid_payload
// errors carrying the request id.
//
// Note: encoding/json (v1) matches field names case-insensitively.
func (e Envelope) DecodePayload(dst any, strict bool) error {
	dec := json.NewDecoder(bytes.NewReader(e.payload))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		perr := &Error{Code: CodeInvalidPayload, Path: "payload", Reason: "payload does not match the message schema", ID: e.id, Err: err}
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			perr.Path = "payload." + te.Field
		}
		return perr
	}
	return nil
}

// DecodeEnvelope parses and validates one text frame.
//
// Rules: the frame MUST be valid UTF-8 JSON (else invalid_json); it MUST be a
// single object with exactly-cased, non-duplicated keys v, type, ts, payload
// and optionally id (else invalid_envelope with Path set); an integer v other
// than 1 yields unsupported_version. Unknown top-level keys are ignored.
// The message type is checked syntactically only: membership in a channel
// catalogue is checked with Catalogue.Check.
func DecodeEnvelope(b []byte) (Envelope, error) {
	if !utf8.Valid(b) || !json.Valid(b) {
		return Envelope{}, &Error{Code: CodeInvalidJSON, Reason: "text frame is not valid UTF-8 JSON"}
	}

	var f struct {
		v, typ, id, ts, payload json.RawMessage
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Reason: "envelope must be a JSON object"}
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return Envelope{}, &Error{Code: CodeInvalidEnvelope, Err: err}
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: key, Err: err}
		}
		var slot *json.RawMessage
		switch key {
		case "v":
			slot = &f.v
		case "type":
			slot = &f.typ
		case "id":
			slot = &f.id
		case "ts":
			slot = &f.ts
		case "payload":
			slot = &f.payload
		default:
			continue
		}
		if *slot != nil {
			return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: key, Reason: "duplicate key"}
		}
		*slot = raw
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Err: err}
	}
	if _, err := dec.Token(); err != io.EOF {
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Reason: "trailing data after envelope"}
	}

	// Read id first so that every later error can be correlated.
	var env Envelope
	if f.id != nil {
		var s string
		if err := json.Unmarshal(f.id, &s); err != nil {
			return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: "id", Reason: "id must be a string"}
		}
		id, err := ParseCorrelationID(s)
		if err != nil {
			return Envelope{}, err
		}
		env.id = id
	}

	if f.v == nil {
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: "v", Reason: "missing v", ID: env.id}
	}
	v, ok := parseInt(f.v)
	if !ok {
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: "v", Reason: "v must be an integer", ID: env.id}
	}
	if v != Version {
		return Envelope{}, &Error{Code: CodeUnsupportedVersion, Path: "v", Reason: fmt.Sprintf("protocol version %d is not supported", v), ID: env.id}
	}

	if f.typ == nil {
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: "type", Reason: "missing type", ID: env.id}
	}
	var ts string
	if err := json.Unmarshal(f.typ, &ts); err != nil {
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: "type", Reason: "type must be a string", ID: env.id}
	}
	typ, err := ParseMessageType(ts)
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) {
			pe.ID = env.id
		}
		return Envelope{}, err
	}
	env.typ = typ

	if f.ts == nil {
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: "ts", Reason: "missing ts", ID: env.id}
	}
	if env.ts, ok = parseInt(f.ts); !ok {
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: "ts", Reason: "ts must be an integer", ID: env.id}
	}

	if f.payload == nil {
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: "payload", Reason: "missing payload", ID: env.id}
	}
	if !isObject(f.payload) {
		return Envelope{}, &Error{Code: CodeInvalidEnvelope, Path: "payload", Reason: "payload must be an object", ID: env.id}
	}
	env.payload = f.payload
	return env, nil
}

// parseInt accepts a JSON number written as an integer (no fraction, no
// exponent) that fits in int64.
func parseInt(raw json.RawMessage) (int64, bool) {
	s := string(raw)
	if s == "" || bytes.ContainsAny(raw, ".eE") || s[0] == '"' {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func isObject(raw json.RawMessage) bool {
	t := bytes.TrimLeft(raw, " \t\r\n")
	return len(t) > 0 && t[0] == '{'
}

func marshalObject(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("{}"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if !isObject(b) {
		return nil, errors.New("payload must encode to a JSON object")
	}
	return b, nil
}
