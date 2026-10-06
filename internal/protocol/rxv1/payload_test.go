package rxv1_test

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

func TestFFTU8RoundTrip(t *testing.T) {
	s := rxv1.DefaultFFTU8Scale()
	bins := []byte{0, 1, 254, 255}
	payload := rxv1.AppendFFTU8(nil, s, bins)
	if len(payload) != rxv1.FFTU8PrefixSize+len(bins) {
		t.Fatalf("len = %d", len(payload))
	}
	// db_min −150 = 0xC3160000, db_step 0.5 = 0x3F000000, little-endian.
	if want := []byte{0x00, 0x00, 0x16, 0xC3, 0x00, 0x00, 0x00, 0x3F}; !bytes.Equal(payload[:8], want) {
		t.Fatalf("prefix = %x, want %x", payload[:8], want)
	}
	got, gotBins, err := rxv1.ParseFFTU8(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got != s || !bytes.Equal(gotBins, bins) {
		t.Fatalf("got %+v %x", got, gotBins)
	}
}

func TestFFTU8Scale(t *testing.T) {
	s := rxv1.DefaultFFTU8Scale()
	tests := []struct {
		db   float32
		want uint8
	}{
		{-200, 0},
		{-150, 0},
		{-149.5, 1},
		{-149.76, 0}, // rounds to nearest
		{-149.74, 1},
		{-22.5, 255},
		{0, 255},
		{float32(math.NaN()), 0},
		{float32(math.Inf(1)), 255},
		{float32(math.Inf(-1)), 0},
	}
	for _, tt := range tests {
		if got := s.Quantise(tt.db); got != tt.want {
			t.Errorf("Quantise(%v) = %d, want %d", tt.db, got, tt.want)
		}
	}
	if got := s.DB(255); got != -22.5 {
		t.Errorf("DB(255) = %v", got)
	}
	if got := s.DB(0); got != -150 {
		t.Errorf("DB(0) = %v", got)
	}
}

func TestParseFFTU8Errors(t *testing.T) {
	nan := rxv1.AppendFFTU8(nil, rxv1.FFTU8Scale{DBMin: float32(math.NaN()), DBStep: 0.5}, nil)
	zero := rxv1.AppendFFTU8(nil, rxv1.FFTU8Scale{DBMin: -150, DBStep: 0}, nil)
	neg := rxv1.AppendFFTU8(nil, rxv1.FFTU8Scale{DBMin: -150, DBStep: -1}, nil)
	inf := rxv1.AppendFFTU8(nil, rxv1.FFTU8Scale{DBMin: -150, DBStep: float32(math.Inf(1))}, nil)
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, rxv1.ErrPayloadTooShort},
		{"7 bytes", make([]byte, 7), rxv1.ErrPayloadTooShort},
		{"nan db_min", nan, rxv1.ErrInvalidScale},
		{"zero step", zero, rxv1.ErrInvalidScale},
		{"negative step", neg, rxv1.ErrInvalidScale},
		{"inf step", inf, rxv1.ErrInvalidScale},
	}
	for _, tt := range tests {
		if _, _, err := rxv1.ParseFFTU8(tt.in); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestADPCMRoundTrip(t *testing.T) {
	tests := []rxv1.ADPCMState{
		{Predictor: 0, StepIndex: 0},
		{Predictor: -32768, StepIndex: 88},
		{Predictor: 32767, StepIndex: 42},
	}
	for _, st := range tests {
		payload := rxv1.AppendADPCM(nil, st, []byte{0x21, 0x43})
		if payload[3] != 0 {
			t.Errorf("reserved byte = %d", payload[3])
		}
		got, data, err := rxv1.ParseADPCM(payload)
		if err != nil {
			t.Fatal(err)
		}
		if got != st || !bytes.Equal(data, []byte{0x21, 0x43}) {
			t.Errorf("got %+v %x, want %+v", got, data, st)
		}
	}
	// −2 as int16 LE is FE FF.
	if p := rxv1.AppendADPCM(nil, rxv1.ADPCMState{Predictor: -2, StepIndex: 3}, nil); !bytes.Equal(p, []byte{0xFE, 0xFF, 3, 0}) {
		t.Errorf("encoding = %x", p)
	}
}

func TestParseADPCMErrors(t *testing.T) {
	if _, _, err := rxv1.ParseADPCM([]byte{1, 2, 3}); !errors.Is(err, rxv1.ErrPayloadTooShort) {
		t.Errorf("short: %v", err)
	}
	if _, _, err := rxv1.ParseADPCM([]byte{0, 0, 89, 0}); !errors.Is(err, rxv1.ErrInvalidADPCMState) {
		t.Errorf("step 89: %v", err)
	}
}
