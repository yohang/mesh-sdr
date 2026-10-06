package rxv1_test

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

func TestCloseCodes(t *testing.T) {
	tests := []struct {
		code      rxv1.CloseCode
		value     uint16
		name      string
		reconnect bool
	}{
		{rxv1.CloseNormal, 1000, "normal", false},
		{rxv1.CloseGoingAway, 1001, "going_away", true},
		{rxv1.CloseUnsupportedData, 1003, "unsupported_data", false},
		{rxv1.ClosePolicyViolation, 1008, "policy_violation", false},
		{rxv1.CloseMessageTooBig, 1009, "message_too_big", false},
		{rxv1.CloseProtocolViolations, 4400, "protocol_violations", false},
		{rxv1.CloseUnauthenticated, 4401, "unauthenticated", true},
		{rxv1.CloseForbidden, 4403, "forbidden", false},
		{rxv1.CloseHandshakeTimeout, 4408, "handshake_timeout", true},
		{rxv1.CloseSlowConsumer, 4413, "slow_consumer", true},
		{rxv1.CloseUnsupportedVersion, 4426, "unsupported_version", false},
		{rxv1.CloseRateLimited, 4429, "rate_limited", true},
		{rxv1.CloseUnavailable, 4503, "unavailable", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if uint16(tt.code) != tt.value {
				t.Errorf("value = %d, want %d", tt.code, tt.value)
			}
			if !tt.code.Known() {
				t.Error("not known")
			}
			if tt.code.String() != tt.name {
				t.Errorf("String() = %q, want %q", tt.code, tt.name)
			}
			if tt.code.ShouldReconnect() != tt.reconnect {
				t.Errorf("ShouldReconnect() = %v, want %v", tt.code.ShouldReconnect(), tt.reconnect)
			}
		})
	}
	if c := rxv1.CloseCode(4999); c.Known() || c.String() != "4999" || c.ShouldReconnect() {
		t.Errorf("unknown close code handling: %v %q", c.Known(), c)
	}
}
