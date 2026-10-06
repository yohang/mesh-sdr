package rxv1_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

func TestDecodeEnvelopeValid(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		typ     rxv1.MessageType
		id      string
		ts      int64
		payload string
	}{
		{
			name:    "spec example",
			in:      `{ "v": 1, "type": "demod.set", "id": "c-42", "ts": 1767225600123, "payload": { "offset_hz": 14000 } }`,
			typ:     rxv1.TypeDemodSet,
			id:      "c-42",
			ts:      1767225600123,
			payload: `{ "offset_hz": 14000 }`,
		},
		{
			name:    "no id, empty payload",
			in:      `{"v":1,"type":"bye","ts":0,"payload":{}}`,
			typ:     rxv1.TypeBye,
			payload: `{}`,
		},
		{
			name:    "keys in any order, unknown top-level key ignored",
			in:      `{"payload":{"a":[1,2]},"x-trace":"abc","ts":5,"type":"session.hello","v":1}`,
			typ:     rxv1.TypeSessionHello,
			ts:      5,
			payload: `{"a":[1,2]}`,
		},
		{
			name:    "id of 64 multi-byte characters",
			in:      `{"v":1,"type":"ack","id":"` + strings.Repeat("é", 64) + `","ts":1,"payload":{}}`,
			typ:     rxv1.TypeAck,
			id:      strings.Repeat("é", 64),
			ts:      1,
			payload: `{}`,
		},
		{
			name:    "negative ts is an integer",
			in:      `{"v":1,"type":"time.sync","ts":-1,"payload":{"t0":1}}`,
			typ:     rxv1.TypeTimeSync,
			ts:      -1,
			payload: `{"t0":1}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := rxv1.DecodeEnvelope([]byte(tt.in))
			if err != nil {
				t.Fatalf("DecodeEnvelope: %v", err)
			}
			if env.Type() != tt.typ {
				t.Errorf("type = %q, want %q", env.Type(), tt.typ)
			}
			id, ok := env.ID()
			if ok != (tt.id != "") || id.String() != tt.id {
				t.Errorf("id = %q,%v, want %q", id, ok, tt.id)
			}
			if env.TS() != tt.ts {
				t.Errorf("ts = %d, want %d", env.TS(), tt.ts)
			}
			if string(env.Payload()) != tt.payload {
				t.Errorf("payload = %s, want %s", env.Payload(), tt.payload)
			}
		})
	}
}

func TestDecodeEnvelopeInvalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		code rxv1.ErrorCode
		path string
		re   string // expected correlation id carried by the error
	}{
		{"not json", `{"v":1,`, rxv1.CodeInvalidJSON, "", ""},
		{"invalid utf-8", "{\"v\":1,\"type\":\"bye\",\"ts\":1,\"payload\":{\"s\":\"\xff\"}}", rxv1.CodeInvalidJSON, "", ""},
		{"empty", ``, rxv1.CodeInvalidJSON, "", ""},
		{"array", `[1,2]`, rxv1.CodeInvalidEnvelope, "", ""},
		{"string", `"hello"`, rxv1.CodeInvalidEnvelope, "", ""},
		{"null", `null`, rxv1.CodeInvalidEnvelope, "", ""},
		{"missing v", `{"type":"bye","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "v", ""},
		{"v string", `{"v":"1","type":"bye","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "v", ""},
		{"v float", `{"v":1.0,"type":"bye","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "v", ""},
		{"v exponent", `{"v":1e0,"type":"bye","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "v", ""},
		{"v 2", `{"v":2,"type":"bye","ts":1,"payload":{}}`, rxv1.CodeUnsupportedVersion, "v", ""},
		{"v 0 keeps id", `{"v":0,"id":"r1","type":"bye","ts":1,"payload":{}}`, rxv1.CodeUnsupportedVersion, "v", "r1"},
		{"v checked before type", `{"v":2,"type":"NOT VALID","ts":1,"payload":{}}`, rxv1.CodeUnsupportedVersion, "v", ""},
		{"missing type", `{"v":1,"ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "type", ""},
		{"type number", `{"v":1,"type":3,"ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "type", ""},
		{"type uppercase", `{"v":1,"type":"Demod.set","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "type", ""},
		{"type trailing dot", `{"v":1,"type":"demod.","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "type", ""},
		{"type double dot", `{"v":1,"type":"demod..set","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "type", ""},
		{"type leading digit", `{"v":1,"type":"1demod","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "type", ""},
		{"type with dash", `{"v":1,"type":"demod-set","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "type", ""},
		{"bad type keeps id", `{"v":1,"id":"r2","type":"X","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "type", "r2"},
		{"missing ts", `{"v":1,"type":"bye","payload":{}}`, rxv1.CodeInvalidEnvelope, "ts", ""},
		{"ts float", `{"v":1,"type":"bye","ts":1.5,"payload":{}}`, rxv1.CodeInvalidEnvelope, "ts", ""},
		{"ts string", `{"v":1,"type":"bye","ts":"1","payload":{}}`, rxv1.CodeInvalidEnvelope, "ts", ""},
		{"ts overflow", `{"v":1,"type":"bye","ts":99999999999999999999,"payload":{}}`, rxv1.CodeInvalidEnvelope, "ts", ""},
		{"missing payload", `{"v":1,"type":"bye","ts":1}`, rxv1.CodeInvalidEnvelope, "payload", ""},
		{"payload null", `{"v":1,"type":"bye","ts":1,"payload":null}`, rxv1.CodeInvalidEnvelope, "payload", ""},
		{"payload array", `{"v":1,"id":"r3","type":"bye","ts":1,"payload":[]}`, rxv1.CodeInvalidEnvelope, "payload", "r3"},
		{"id number", `{"v":1,"id":42,"type":"bye","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "id", ""},
		{"id null", `{"v":1,"id":null,"type":"bye","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "id", ""},
		{"id empty", `{"v":1,"id":"","type":"bye","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "id", ""},
		{"id too long", `{"v":1,"id":"` + strings.Repeat("a", 65) + `","type":"bye","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "id", ""},
		{"duplicate type", `{"v":1,"type":"bye","type":"device.retune","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "type", ""},
		{"wrong-case key not accepted", `{"V":1,"type":"bye","ts":1,"payload":{}}`, rxv1.CodeInvalidEnvelope, "v", ""},
		{"trailing value", `{"v":1,"type":"bye","ts":1,"payload":{}} {}`, rxv1.CodeInvalidJSON, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rxv1.DecodeEnvelope([]byte(tt.in))
			var pe *rxv1.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *rxv1.Error", err)
			}
			if pe.Code != tt.code {
				t.Errorf("code = %q, want %q (%v)", pe.Code, tt.code, err)
			}
			if pe.Path != tt.path {
				t.Errorf("path = %q, want %q", pe.Path, tt.path)
			}
			if pe.ID.String() != tt.re {
				t.Errorf("id = %q, want %q", pe.ID, tt.re)
			}
			if !errors.Is(err, &rxv1.Error{Code: tt.code}) {
				t.Errorf("errors.Is does not match code %q", tt.code)
			}
		})
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	type payload struct {
		OffsetHz int64 `json:"offset_hz"`
	}
	env, err := rxv1.NewEnvelope(rxv1.TypeDemodSet, rxv1.MustCorrelationID("c-42"), 1767225600123, payload{OffsetHz: 14000})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"v":1,"type":"demod.set","id":"c-42","ts":1767225600123,"payload":{"offset_hz":14000}}`
	if string(b) != want {
		t.Fatalf("encoded = %s\nwant      %s", b, want)
	}
	got, err := rxv1.DecodeEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	var p payload
	if err := got.DecodePayload(&p, true); err != nil {
		t.Fatal(err)
	}
	if p.OffsetHz != 14000 || got.Type() != rxv1.TypeDemodSet || got.TS() != env.TS() {
		t.Fatalf("round trip mismatch: %+v %v", p, got)
	}
}

func TestNewEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		typ     rxv1.MessageType
		payload any
		want    string
		wantErr bool
	}{
		{"nil payload is {}", rxv1.TypeBye, nil, `{"v":1,"type":"bye","ts":7,"payload":{}}`, false},
		{"map payload", rxv1.TypeNotice, map[string]string{"code": "x"}, `{"v":1,"type":"notice","ts":7,"payload":{"code":"x"}}`, false},
		{"raw payload", rxv1.TypeNotice, json.RawMessage(`{"a":1}`), `{"v":1,"type":"notice","ts":7,"payload":{"a":1}}`, false},
		{"array payload rejected", rxv1.TypeNotice, []int{1}, "", true},
		{"string payload rejected", rxv1.TypeNotice, "x", "", true},
		{"invalid type rejected", rxv1.MessageType("Bad"), nil, "", true},
		{"unmarshalable payload", rxv1.TypeNotice, map[string]any{"f": func() {}}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := rxv1.NewEnvelope(tt.typ, rxv1.CorrelationID{}, 7, tt.payload)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			b, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.want {
				t.Errorf("got %s, want %s", b, tt.want)
			}
		})
	}
}

func TestZeroEnvelopeDoesNotMarshal(t *testing.T) {
	if _, err := json.Marshal(rxv1.Envelope{}); err == nil {
		t.Fatal("expected an error for the zero Envelope")
	}
}

func TestDecodePayload(t *testing.T) {
	type attach struct {
		DeviceID string `json:"device_id"`
		FFT      struct {
			FPS int `json:"fps"`
		} `json:"fft"`
	}
	tests := []struct {
		name    string
		payload string
		strict  bool
		code    rxv1.ErrorCode
		path    string
	}{
		{"valid strict", `{"device_id":"dev-hf","fft":{"fps":25}}`, true, "", ""},
		{"unknown field strict", `{"device_id":"dev-hf","extra":1}`, true, rxv1.CodeInvalidPayload, "payload"},
		{"unknown field lenient", `{"device_id":"dev-hf","extra":1}`, false, "", ""},
		{"wrong type", `{"device_id":"dev-hf","fft":{"fps":"25"}}`, true, rxv1.CodeInvalidPayload, "payload.fft.fps"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := rxv1.DecodeEnvelope([]byte(`{"v":1,"id":"q1","type":"device.attach","ts":1,"payload":` + tt.payload + `}`))
			if err != nil {
				t.Fatal(err)
			}
			var dst attach
			err = env.DecodePayload(&dst, tt.strict)
			if tt.code == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var pe *rxv1.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *rxv1.Error", err)
			}
			if pe.Code != tt.code || pe.Path != tt.path || pe.ID.String() != "q1" {
				t.Errorf("got code=%q path=%q id=%q, want %q %q q1", pe.Code, pe.Path, pe.ID, tt.code, tt.path)
			}
		})
	}
}

func TestParseMessageType(t *testing.T) {
	valid := []string{"a", "ack", "session.hello", "device.config.patch", "a_b.c_1.2", "x9"}
	invalid := []string{"", "A", "_a", "9a", "a.", ".a", "a..b", "a-b", "a b", "a.B", "é"}
	for _, s := range valid {
		if _, err := rxv1.ParseMessageType(s); err != nil {
			t.Errorf("ParseMessageType(%q) = %v, want ok", s, err)
		}
	}
	for _, s := range invalid {
		if _, err := rxv1.ParseMessageType(s); !errors.Is(err, rxv1.ErrInvalidEnvelope) {
			t.Errorf("ParseMessageType(%q) = %v, want invalid_envelope", s, err)
		}
	}
}

func TestCatalogues(t *testing.T) {
	tests := []struct {
		cat  rxv1.Catalogue
		typ  rxv1.MessageType
		want bool
	}{
		{rxv1.MediaClientToNode(), rxv1.TypeSessionHello, true},
		{rxv1.MediaClientToNode(), rxv1.TypeDeviceRetune, true},
		{rxv1.MediaClientToNode(), rxv1.TypeAck, false},
		{rxv1.MediaClientToNode(), rxv1.TypeSub, false},
		{rxv1.MediaNodeToClient(), rxv1.TypeStreamOpen, true},
		{rxv1.MediaNodeToClient(), rxv1.TypeError, true},
		{rxv1.MediaNodeToClient(), rxv1.TypeDemodSet, false},
		{rxv1.HubClientToHub(), rxv1.TypeSub, true},
		{rxv1.HubClientToHub(), rxv1.TypeAuthRefresh, false},
		{rxv1.HubHubToClient(), rxv1.TypeSessionRevoked, true},
		{rxv1.HubHubToClient(), rxv1.TypeStreamOpen, false},
	}
	for _, tt := range tests {
		if got := tt.cat.Contains(tt.typ); got != tt.want {
			t.Errorf("%s.Contains(%s) = %v, want %v", tt.cat.Name(), tt.typ, got, tt.want)
		}
		err := tt.cat.Check(tt.typ)
		if tt.want && err != nil {
			t.Errorf("%s.Check(%s) = %v", tt.cat.Name(), tt.typ, err)
		}
		if !tt.want && !errors.Is(err, rxv1.ErrUnsupportedType) {
			t.Errorf("%s.Check(%s) = %v, want unsupported_type", tt.cat.Name(), tt.typ, err)
		}
	}
}

func TestCorrelationIDJSON(t *testing.T) {
	var id rxv1.CorrelationID
	if err := json.Unmarshal([]byte(`"abc"`), &id); err != nil || id.String() != "abc" {
		t.Fatalf("unmarshal: %v %q", err, id)
	}
	if err := json.Unmarshal([]byte(`""`), &id); err == nil {
		t.Fatal("empty id must be rejected")
	}
	if err := json.Unmarshal([]byte(`1`), &id); err == nil {
		t.Fatal("number id must be rejected")
	}
	b, err := json.Marshal(rxv1.MustCorrelationID("x"))
	if err != nil || string(b) != `"x"` {
		t.Fatalf("marshal: %s %v", b, err)
	}
}
