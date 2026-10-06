package rxv1_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"testing"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

func TestAppendFrameGoldenBytes(t *testing.T) {
	h := rxv1.FrameHeader{
		Type:        rxv1.FrameAudio,
		Codec:       rxv1.CodecOpus,
		StreamID:    0x0102,
		Flags:       rxv1.FlagDiscontinuity | rxv1.FlagSquelched,
		Seq:         0x03040506,
		TimestampUS: 0x0708090A0B0C0D0E,
	}
	got, err := rxv1.AppendFrame(nil, h, []byte{0xDE, 0xAD})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString("" +
		"a5" + // magic
		"01" + // version
		"02" + // type audio
		"02" + // codec opus
		"0201" + // stream_id LE
		"0900" + // flags LE: discontinuity|squelched
		"06050403" + // seq LE
		"0e0d0c0b0a090807" + // timestamp_us LE
		"02000000" + // payload_len LE
		"dead")
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}

	parsed, payload, err := rxv1.ParseFrame(got)
	if err != nil {
		t.Fatal(err)
	}
	h.PayloadLen = 2
	if parsed != h || !bytes.Equal(payload, []byte{0xDE, 0xAD}) {
		t.Fatalf("parsed %+v %x", parsed, payload)
	}
}

func TestAppendFrameAppendsToDst(t *testing.T) {
	dst := []byte("prefix")
	out, err := rxv1.AppendFrame(dst, rxv1.FrameHeader{Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("prefix")) || len(out) != len("prefix")+rxv1.HeaderSize {
		t.Fatalf("unexpected output %x", out)
	}
}

func TestAppendFrameValidation(t *testing.T) {
	ok := rxv1.FrameHeader{Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB}
	tests := []struct {
		name    string
		h       rxv1.FrameHeader
		payload int
		want    error
	}{
		{"valid fft", ok, 100, nil},
		{"valid secondary fft f32", rxv1.FrameHeader{Type: rxv1.FrameSecondaryFFT, Codec: rxv1.CodecFFTF32DB}, 0, nil},
		{"valid hd audio pcm", rxv1.FrameHeader{Type: rxv1.FrameHDAudio, Codec: rxv1.CodecPCMS16LE}, 0, nil},
		{"valid all defined flags", rxv1.FrameHeader{Type: rxv1.FrameAudio, Codec: rxv1.CodecADPCMIMA, Flags: 0x000F}, 0, nil},
		{"max size", ok, rxv1.MaxFrameBytes - rxv1.HeaderSize, nil},
		{"too large", ok, rxv1.MaxFrameBytes - rxv1.HeaderSize + 1, rxv1.ErrFrameTooLarge},
		{"type 0", rxv1.FrameHeader{Type: 0, Codec: rxv1.CodecOpus}, 0, rxv1.ErrInvalidHeader},
		{"reserved type", rxv1.FrameHeader{Type: 0x05, Codec: rxv1.CodecOpus}, 0, rxv1.ErrInvalidHeader},
		{"unknown codec", rxv1.FrameHeader{Type: rxv1.FrameAudio, Codec: 0x03}, 0, rxv1.ErrInvalidHeader},
		{"fft codec on audio", rxv1.FrameHeader{Type: rxv1.FrameAudio, Codec: rxv1.CodecFFTU8DB}, 0, rxv1.ErrInvalidHeader},
		{"audio codec on fft", rxv1.FrameHeader{Type: rxv1.FrameFFT, Codec: rxv1.CodecADPCMIMA}, 0, rxv1.ErrInvalidHeader},
		{"reserved flag bit 4", rxv1.FrameHeader{Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB, Flags: 1 << 4}, 0, rxv1.ErrInvalidHeader},
		{"reserved flag bit 15", rxv1.FrameHeader{Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB, Flags: 1 << 15}, 0, rxv1.ErrInvalidHeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := rxv1.AppendFrame(nil, tt.h, make([]byte, tt.payload))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if err == nil && len(out) != rxv1.HeaderSize+tt.payload {
				t.Fatalf("len = %d", len(out))
			}
			if err != nil && len(out) != 0 {
				t.Fatalf("dst modified on error: %x", out)
			}
		})
	}
}

func TestParseFrameErrors(t *testing.T) {
	valid, err := rxv1.AppendFrame(nil, rxv1.FrameHeader{Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB, Seq: 7}, []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(f func([]byte) []byte) []byte {
		b := append([]byte(nil), valid...)
		return f(b)
	}
	tests := []struct {
		name  string
		frame []byte
		want  error
	}{
		{"nil", nil, rxv1.ErrFrameTooShort},
		{"23 bytes", valid[:23], rxv1.ErrFrameTooShort},
		{"bad magic", mutate(func(b []byte) []byte { b[0] = 0x5A; return b }), rxv1.ErrBadMagic},
		{"version 0", mutate(func(b []byte) []byte { b[1] = 0; return b }), rxv1.ErrUnsupportedFrameVersion},
		{"version 2", mutate(func(b []byte) []byte { b[1] = 2; return b }), rxv1.ErrUnsupportedFrameVersion},
		{"truncated payload", valid[:len(valid)-1], rxv1.ErrPayloadLength},
		{"extra byte", append(append([]byte(nil), valid...), 0), rxv1.ErrPayloadLength},
		{"payload_len huge", mutate(func(b []byte) []byte { b[20], b[21], b[22], b[23] = 0xFF, 0xFF, 0xFF, 0xFF; return b }), rxv1.ErrPayloadLength},
		{"header only claiming payload", mutate(func(b []byte) []byte { return b[:24] }), rxv1.ErrPayloadLength},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := rxv1.ParseFrame(tt.frame); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestParseFrameIsStructuralOnly(t *testing.T) {
	// A reserved type and reserved flags parse; Validate reports them.
	frame := []byte{0xA5, 1, 0x7F, 0x99, 0, 0, 0xF0, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	h, payload, err := rxv1.ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 0 || h.Type != 0x7F || h.Flags != 0xFFF0 {
		t.Fatalf("unexpected %+v", h)
	}
	if err := h.Validate(); !errors.Is(err, rxv1.ErrInvalidHeader) {
		t.Fatalf("Validate = %v", err)
	}
}

func TestParseFramePayloadAliasesInput(t *testing.T) {
	frame, _ := rxv1.AppendFrame(nil, rxv1.FrameHeader{Type: rxv1.FrameAudio, Codec: rxv1.CodecOpus}, []byte{9})
	_, payload, _ := rxv1.ParseFrame(frame)
	payload[0] = 1
	if frame[rxv1.HeaderSize] != 1 {
		t.Fatal("payload should alias the frame (zero-copy)")
	}
}

func TestSetFlags(t *testing.T) {
	frame, _ := rxv1.AppendFrame(nil, rxv1.FrameHeader{Type: rxv1.FrameAudio, Codec: rxv1.CodecOpus, Flags: rxv1.FlagReset}, []byte{1})
	if err := rxv1.SetFlags(frame, rxv1.FlagDiscontinuity); err != nil {
		t.Fatal(err)
	}
	h, _, err := rxv1.ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Flags.Has(rxv1.FlagDiscontinuity|rxv1.FlagReset) || h.Flags.Has(rxv1.FlagEndOfStream) {
		t.Fatalf("flags = %04x", uint16(h.Flags))
	}
	if err := rxv1.SetFlags(frame[:10], rxv1.FlagReset); err == nil {
		t.Fatal("short frame must be rejected")
	}
}

func TestSeqGap(t *testing.T) {
	tests := []struct {
		prev, next, want uint32
	}{
		{1, 2, 0},
		{1, 3, 1},
		{1, 11, 9},
		{math.MaxUint32, 0, 0},
		{math.MaxUint32 - 1, 1, 2},
		{5, 5, math.MaxUint32}, // duplicate: maximal gap, caller treats as reorder/dup
	}
	for _, tt := range tests {
		if got := rxv1.SeqGap(tt.prev, tt.next); got != tt.want {
			t.Errorf("SeqGap(%d,%d) = %d, want %d", tt.prev, tt.next, got, tt.want)
		}
	}
}

func TestFrameTypeAndCodecPredicates(t *testing.T) {
	for ft := 0; ft < 256; ft++ {
		f := rxv1.FrameType(ft)
		if f.Known() != (ft >= 1 && ft <= 4) {
			t.Errorf("FrameType(%d).Known", ft)
		}
		if f.IsAudio() && f.IsFFT() {
			t.Errorf("FrameType(%d) both audio and fft", ft)
		}
		if f.String() == "" {
			t.Errorf("FrameType(%d) empty name", ft)
		}
	}
	for c := 0; c < 256; c++ {
		cd := rxv1.Codec(c)
		if cd.Known() != (cd.IsAudio() || cd.IsFFT()) {
			t.Errorf("Codec(%d) Known inconsistent", c)
		}
		if cd.String() == "" {
			t.Errorf("Codec(%d) empty name", c)
		}
	}
}

func BenchmarkAppendFrame(b *testing.B) {
	payload := make([]byte, 2048+8)
	buf := make([]byte, 0, rxv1.HeaderSize+len(payload))
	h := rxv1.FrameHeader{Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		h.Seq = uint32(i)
		var err error
		if buf, err = rxv1.AppendFrame(buf[:0], h, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseFrame(b *testing.B) {
	frame, _ := rxv1.AppendFrame(nil, rxv1.FrameHeader{Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB}, make([]byte, 2056))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := rxv1.ParseFrame(frame); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeEnvelope(b *testing.B) {
	msg := []byte(`{"v":1,"type":"demod.set","id":"c-42","ts":1767225600123,"payload":{"offset_hz":14000}}`)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := rxv1.DecodeEnvelope(msg); err != nil {
			b.Fatal(err)
		}
	}
}
