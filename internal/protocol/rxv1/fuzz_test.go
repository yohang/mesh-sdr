package rxv1_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// FuzzParseFrame checks that the header decoder never panics, only accepts
// structurally valid frames, and that every accepted frame re-encodes to the
// exact same bytes.
func FuzzParseFrame(f *testing.F) {
	seed := func(h rxv1.FrameHeader, payload []byte) {
		b, err := rxv1.AppendFrame(nil, h, payload)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	seed(rxv1.FrameHeader{Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB, Seq: 1}, rxv1.AppendFFTU8(nil, rxv1.DefaultFFTU8Scale(), []byte{0, 128, 255}))
	seed(rxv1.FrameHeader{Type: rxv1.FrameAudio, Codec: rxv1.CodecADPCMIMA, Flags: rxv1.FlagDiscontinuity}, rxv1.AppendADPCM(nil, rxv1.ADPCMState{Predictor: -5, StepIndex: 88}, []byte{0x12}))
	seed(rxv1.FrameHeader{Type: rxv1.FrameHDAudio, Codec: rxv1.CodecOpus, Seq: ^uint32(0), TimestampUS: ^uint64(0)}, nil)
	f.Add([]byte{})
	f.Add(make([]byte, 23))
	f.Add(bytes.Repeat([]byte{0xA5}, 24))
	f.Add([]byte{0xA5, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, frame []byte) {
		h, payload, err := rxv1.ParseFrame(frame)
		if err != nil {
			if !errors.Is(err, rxv1.ErrFrameTooShort) && !errors.Is(err, rxv1.ErrBadMagic) &&
				!errors.Is(err, rxv1.ErrUnsupportedFrameVersion) && !errors.Is(err, rxv1.ErrPayloadLength) {
				t.Fatalf("unexpected error kind: %v", err)
			}
			return
		}
		if len(frame) < rxv1.HeaderSize || frame[0] != rxv1.Magic || frame[1] != rxv1.FrameVersion {
			t.Fatalf("accepted structurally invalid frame %x", frame)
		}
		if int(h.PayloadLen) != len(payload) || len(payload) != len(frame)-rxv1.HeaderSize {
			t.Fatalf("payload_len %d, payload %d, frame %d", h.PayloadLen, len(payload), len(frame))
		}
		// Semantically valid headers must round-trip byte for byte.
		if h.Validate() == nil && len(frame) <= rxv1.MaxFrameBytes {
			out, err := rxv1.AppendFrame(nil, h, payload)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if !bytes.Equal(out, frame) {
				t.Fatalf("round trip mismatch\n in  %x\n out %x", frame, out)
			}
		}
		// Payload prefix parsers must not panic either.
		switch h.Codec {
		case rxv1.CodecFFTU8DB:
			_, _, _ = rxv1.ParseFFTU8(payload)
		case rxv1.CodecADPCMIMA:
			_, _, _ = rxv1.ParseADPCM(payload)
		}
	})
}

// FuzzDecodeEnvelope checks that the envelope decoder never panics, always
// returns a *rxv1.Error on failure, and that accepted envelopes survive an
// encode/decode round trip.
func FuzzDecodeEnvelope(f *testing.F) {
	f.Add([]byte(`{"v":1,"type":"demod.set","id":"c-42","ts":1767225600123,"payload":{"offset_hz":14000}}`))
	f.Add([]byte(`{"v":1,"type":"bye","ts":0,"payload":{}}`))
	f.Add([]byte(`{"v":2,"type":"x","ts":0,"payload":{}}`))
	f.Add([]byte(`{"v":1,"type":"a","type":"b","ts":0,"payload":{}}`))
	f.Add([]byte(`[]`))
	f.Add([]byte("\xff"))

	f.Fuzz(func(t *testing.T, in []byte) {
		env, err := rxv1.DecodeEnvelope(in)
		if err != nil {
			var pe *rxv1.Error
			if !errors.As(err, &pe) || !pe.Code.Known() {
				t.Fatalf("non-protocol error %T: %v", err, err)
			}
			return
		}
		b, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		again, err := rxv1.DecodeEnvelope(b)
		if err != nil {
			t.Fatalf("decode re-encoded %s: %v", b, err)
		}
		id1, _ := env.ID()
		id2, _ := again.ID()
		if again.Type() != env.Type() || again.TS() != env.TS() || id1 != id2 {
			t.Fatalf("round trip mismatch: %s", b)
		}
	})
}
