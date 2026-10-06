// Package rxv1 is the wire codec of Protocol v1 (TECHNICAL_SPEC §6): the
// rx.v1 / rx-ctl.v1 WebSocket subprotocols.
//
// It covers, independently of any WebSocket library:
//
//   - the JSON envelope {v, type, id, ts, payload} (§6.2), with strict
//     decoding of the envelope itself and an opt-in strict payload decoder;
//   - the ack and error frames, the error code catalogue and the mapping of
//     error codes to close codes (§6.2);
//   - the application close codes (§6.2);
//   - the 24-byte little-endian binary media frame header (§6.7) and the
//     fixed payload prefixes of the FFT u8 dB and IMA ADPCM codecs.
//
// The package is pure: no I/O, no goroutines, no logging. Transport concerns
// (subprotocol negotiation, read limits, send queues, back-pressure) live in
// the adapters that use it.
package rxv1

// Subprotocol names (§6.1, §4.4).
const (
	// Subprotocol is the Sec-WebSocket-Protocol token of the hub events WS
	// and of the node media WS.
	Subprotocol = "rx.v1"
	// ControlSubprotocol is the token of the hub → node control channel.
	ControlSubprotocol = "rx-ctl.v1"
)

// Version is the protocol major version carried in every envelope ("v").
const Version = 1

// Size limits (§6.9).
const (
	// MaxInboundTextBytes is the inbound text message limit of the media and
	// hub events WS. The receiver closes with 1009 above it.
	MaxInboundTextBytes = 16 << 10
	// MaxInboundControlTextBytes is the inbound text message limit of the
	// rx-ctl.v1 control channel.
	MaxInboundControlTextBytes = 64 << 10
	// MaxFrameBytes is the maximum size of one outbound binary frame,
	// header included.
	MaxFrameBytes = 64 << 10
	// MaxCorrelationIDLen is the maximum length, in characters, of an
	// envelope "id".
	MaxCorrelationIDLen = 64
)
