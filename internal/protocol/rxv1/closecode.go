package rxv1

import "strconv"

// CloseCode is a WebSocket close status code used by Protocol v1 (§6.2).
// It is converted to the library-specific type at the transport boundary.
type CloseCode uint16

// Close codes (§6.2 close code table).
const (
	CloseNormal             CloseCode = 1000
	CloseGoingAway          CloseCode = 1001
	CloseUnsupportedData    CloseCode = 1003
	ClosePolicyViolation    CloseCode = 1008
	CloseMessageTooBig      CloseCode = 1009
	CloseProtocolViolations CloseCode = 4400
	CloseUnauthenticated    CloseCode = 4401
	CloseForbidden          CloseCode = 4403
	CloseHandshakeTimeout   CloseCode = 4408
	CloseSlowConsumer       CloseCode = 4413
	CloseUnsupportedVersion CloseCode = 4426
	CloseRateLimited        CloseCode = 4429
	CloseUnavailable        CloseCode = 4503
)

var closeCodeNames = map[CloseCode]string{
	CloseNormal:             "normal",
	CloseGoingAway:          "going_away",
	CloseUnsupportedData:    "unsupported_data",
	ClosePolicyViolation:    "policy_violation",
	CloseMessageTooBig:      "message_too_big",
	CloseProtocolViolations: "protocol_violations",
	CloseUnauthenticated:    "unauthenticated",
	CloseForbidden:          "forbidden",
	CloseHandshakeTimeout:   "handshake_timeout",
	CloseSlowConsumer:       "slow_consumer",
	CloseUnsupportedVersion: "unsupported_version",
	CloseRateLimited:        "rate_limited",
	CloseUnavailable:        "unavailable",
}

// Known reports whether c is one of the §6.2 close codes.
func (c CloseCode) Known() bool {
	_, ok := closeCodeNames[c]
	return ok
}

// String returns a stable snake_case name, usable as a log attribute or a
// short close reason, or the number for unknown codes.
func (c CloseCode) String() string {
	if n, ok := closeCodeNames[c]; ok {
		return n
	}
	return strconv.Itoa(int(c))
}

// ShouldReconnect reports whether the §6.2 client behaviour for c is to
// reconnect automatically (possibly after fetching a token, backing off or
// lowering the FFT rate). False means: do not retry automatically.
func (c CloseCode) ShouldReconnect() bool {
	switch c {
	case CloseGoingAway, CloseUnauthenticated, CloseHandshakeTimeout,
		CloseSlowConsumer, CloseRateLimited, CloseUnavailable:
		return true
	default:
		return false
	}
}
