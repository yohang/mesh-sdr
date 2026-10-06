package rxv1_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

func TestErrorCodeCloseMapping(t *testing.T) {
	tests := []struct {
		code       rxv1.ErrorCode
		immediate  rxv1.CloseCode // 0 = none
		escalation rxv1.CloseCode // 0 = none
	}{
		{rxv1.CodeInvalidJSON, 0, rxv1.CloseProtocolViolations},
		{rxv1.CodeInvalidEnvelope, 0, rxv1.CloseProtocolViolations},
		{rxv1.CodeInvalidPayload, 0, rxv1.CloseProtocolViolations},
		{rxv1.CodeUnsupportedType, 0, 0},
		{rxv1.CodeUnsupportedVersion, rxv1.CloseUnsupportedVersion, 0},
		{rxv1.CodeUnauthenticated, rxv1.CloseUnauthenticated, 0},
		{rxv1.CodeTokenExpired, rxv1.CloseUnauthenticated, 0},
		{rxv1.CodeTokenInvalid, rxv1.CloseUnauthenticated, 0},
		{rxv1.CodeForbidden, 0, rxv1.CloseForbidden},
		{rxv1.CodeNotFound, 0, 0},
		{rxv1.CodeConflict, 0, 0},
		{rxv1.CodeCapacityExceeded, 0, 0},
		{rxv1.CodeRateLimited, 0, rxv1.CloseRateLimited},
		{rxv1.CodeOutOfRange, 0, 0},
		{rxv1.CodePresetIncompatible, 0, 0},
		{rxv1.CodeDeviceUnavailable, 0, 0},
		{rxv1.CodeDemodError, 0, 0},
		{rxv1.CodeNodeUnavailable, rxv1.CloseUnavailable, 0},
		{rxv1.CodeHubUnavailable, rxv1.CloseUnavailable, 0},
		{rxv1.CodeClockSkew, rxv1.CloseUnavailable, 0},
		{rxv1.CodeInternal, 0, 0},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			if !tt.code.Known() {
				t.Fatal("code not known")
			}
			got, ok := tt.code.CloseCode()
			if ok != (tt.immediate != 0) || got != tt.immediate {
				t.Errorf("CloseCode() = %d,%v, want %d", got, ok, tt.immediate)
			}
			got, ok = tt.code.EscalationCloseCode()
			if ok != (tt.escalation != 0) || got != tt.escalation {
				t.Errorf("EscalationCloseCode() = %d,%v, want %d", got, ok, tt.escalation)
			}
		})
	}
	if rxv1.ErrorCode("stream_error").Known() {
		t.Error("stream_error is not in the §6.2 catalogue")
	}
}

func TestErrorPayloadFrom(t *testing.T) {
	t.Run("protocol error keeps code, path and re", func(t *testing.T) {
		_, err := rxv1.DecodeEnvelope([]byte(`{"v":1,"id":"c-43","type":"bye","ts":"x","payload":{}}`))
		p := rxv1.ErrorPayloadFrom(err, "inc-1")
		if p.Code != rxv1.CodeInvalidEnvelope || p.Re == nil || p.Re.String() != "c-43" || p.Details["path"] != "ts" {
			t.Fatalf("unexpected payload %+v", p)
		}
	})
	t.Run("wrapped protocol error", func(t *testing.T) {
		err := fmt.Errorf("handle frame: %w", &rxv1.Error{Code: rxv1.CodeForbidden, Reason: "Retune is not allowed on this device"})
		p := rxv1.ErrorPayloadFrom(err, "inc-1")
		if p.Code != rxv1.CodeForbidden || p.Message != "Retune is not allowed on this device" || p.Re != nil || p.Details != nil {
			t.Fatalf("unexpected payload %+v", p)
		}
	})
	t.Run("foreign error becomes internal without leaking the cause", func(t *testing.T) {
		p := rxv1.ErrorPayloadFrom(errors.New("db: secret path /data/x"), "inc-9")
		if p.Code != rxv1.CodeInternal || p.Details["incident_id"] != "inc-9" || p.Message != "Internal error" {
			t.Fatalf("unexpected payload %+v", p)
		}
	})
}

func TestNewErrorEnvelope(t *testing.T) {
	re := rxv1.MustCorrelationID("c-43")
	env, err := rxv1.NewErrorEnvelope(1767225600131, rxv1.ErrorPayload{
		Re:      &re,
		Code:    rxv1.CodeForbidden,
		Message: "Retune is not allowed on this device",
		Details: map[string]any{"device_id": "dev-hf"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"v":1,"type":"error","ts":1767225600131,"payload":{"re":"c-43","code":"forbidden","message":"Retune is not allowed on this device","retryable":false,"retry_after_ms":null,"details":{"device_id":"dev-hf"}}}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}

	// Fire-and-forget: re is null.
	env, err = rxv1.NewErrorEnvelope(1, rxv1.ErrorPayload{Code: rxv1.CodeNotFound, Message: "x"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(env)
	if want := `{"v":1,"type":"error","ts":1,"payload":{"re":null,"code":"not_found","message":"x","retryable":false,"retry_after_ms":null}}`; string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}

	if _, err := rxv1.NewErrorEnvelope(1, rxv1.ErrorPayload{Code: "nope"}); err == nil {
		t.Error("unknown code must be rejected")
	}
	if _, err := rxv1.NewErrorEnvelope(1, rxv1.ErrorPayload{Code: rxv1.CodeRateLimited}); err == nil {
		t.Error("rate_limited without retry_after_ms must be rejected")
	}
	ms := int64(500)
	if _, err := rxv1.NewErrorEnvelope(1, rxv1.ErrorPayload{Code: rxv1.CodeRateLimited, Retryable: true, RetryAfterMS: &ms}); err != nil {
		t.Errorf("rate_limited with retry_after_ms: %v", err)
	}
}

func TestNewAckEnvelope(t *testing.T) {
	env, err := rxv1.NewAckEnvelope(1767225600130, rxv1.MustCorrelationID("c-42"),
		map[string]any{"applied": map[string]int{"offset_hz": 14000}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	const want = `{"v":1,"type":"ack","ts":1767225600130,"payload":{"re":"c-42","result":{"applied":{"offset_hz":14000}}}}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}

	env, err = rxv1.NewAckEnvelope(1, rxv1.MustCorrelationID("a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(env)
	if want := `{"v":1,"type":"ack","ts":1,"payload":{"re":"a","result":{}}}`; string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}

	if _, err := rxv1.NewAckEnvelope(1, rxv1.CorrelationID{}, nil); err == nil {
		t.Error("ack without id must be rejected")
	}
	if _, err := rxv1.NewAckEnvelope(1, rxv1.MustCorrelationID("a"), []int{1}); err == nil {
		t.Error("non-object result must be rejected")
	}
}

func TestErrorString(t *testing.T) {
	cause := errors.New("boom")
	err := &rxv1.Error{Code: rxv1.CodeInvalidPayload, Path: "payload.x", Reason: "bad", Err: cause}
	if got, want := err.Error(), "invalid_payload at payload.x: bad: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Error("cause not unwrapped")
	}
	if errors.Is(err, rxv1.ErrInvalidEnvelope) {
		t.Error("must not match another code")
	}
}
