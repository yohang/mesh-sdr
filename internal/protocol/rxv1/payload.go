package rxv1

import (
	"encoding/binary"
	"errors"
	"math"
)

// FFT u8 dB codec (0x10, §6.7): {db_min:f32, db_step:f32} then size × u8.

// FFTU8PrefixSize is the size of the {db_min, db_step} prefix.
const FFTU8PrefixSize = 8

// FFTU8Scale is the quantisation scale of a u8 dB spectrum line.
type FFTU8Scale struct {
	DBMin  float32
	DBStep float32
}

// DefaultFFTU8Scale is db_min = −150, db_step = 0.5 (§6.7).
func DefaultFFTU8Scale() FFTU8Scale { return FFTU8Scale{DBMin: -150, DBStep: 0.5} }

// DB returns the level in dB of quantised value q.
func (s FFTU8Scale) DB(q uint8) float32 { return s.DBMin + float32(q)*s.DBStep }

// Quantise maps a level in dB to q, clamping to [0, 255].
func (s FFTU8Scale) Quantise(db float32) uint8 {
	q := math.Round(float64((db - s.DBMin) / s.DBStep))
	switch {
	case math.IsNaN(q), q <= 0:
		return 0
	case q >= 255:
		return 255
	}
	return uint8(q)
}

// ErrPayloadTooShort is returned when a payload is shorter than its fixed prefix.
var ErrPayloadTooShort = errors.New("rxv1: payload shorter than codec prefix")

// ErrInvalidScale is returned for a non-finite or non-positive db_step.
var ErrInvalidScale = errors.New("rxv1: invalid FFT u8 scale")

// AppendFFTU8 appends an FFT u8 dB payload to dst.
func AppendFFTU8(dst []byte, s FFTU8Scale, bins []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, math.Float32bits(s.DBMin))
	dst = binary.LittleEndian.AppendUint32(dst, math.Float32bits(s.DBStep))
	return append(dst, bins...)
}

// ParseFFTU8 splits an FFT u8 dB payload into its scale and bins (a
// sub-slice of payload). The bin count is checked against fft.size by the
// caller, which knows it from stream.open.
func ParseFFTU8(payload []byte) (FFTU8Scale, []byte, error) {
	if len(payload) < FFTU8PrefixSize {
		return FFTU8Scale{}, nil, ErrPayloadTooShort
	}
	s := FFTU8Scale{
		DBMin:  math.Float32frombits(binary.LittleEndian.Uint32(payload[0:])),
		DBStep: math.Float32frombits(binary.LittleEndian.Uint32(payload[4:])),
	}
	if !finite(s.DBMin) || !finite(s.DBStep) || s.DBStep <= 0 {
		return FFTU8Scale{}, nil, ErrInvalidScale
	}
	return s, payload[FFTU8PrefixSize:], nil
}

func finite(f float32) bool { return !math.IsNaN(float64(f)) && !math.IsInf(float64(f), 0) }

// IMA ADPCM codec (0x01, §6.7): 4-byte state prefix then nibbles, low nibble
// first.

// ADPCMPrefixSize is the size of the IMA ADPCM state prefix.
const ADPCMPrefixSize = 4

// MaxADPCMStepIndex is the last index of the standard IMA step table.
const MaxADPCMStepIndex = 88

// ADPCMState is the decoder state carried at the start of each frame, which
// makes every frame self-contained.
type ADPCMState struct {
	Predictor int16
	StepIndex uint8
}

// ErrInvalidADPCMState is returned for a step index outside the IMA table.
var ErrInvalidADPCMState = errors.New("rxv1: IMA ADPCM step index out of range")

// AppendADPCM appends an IMA ADPCM payload to dst.
func AppendADPCM(dst []byte, st ADPCMState, nibbles []byte) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, uint16(st.Predictor))
	dst = append(dst, st.StepIndex, 0)
	return append(dst, nibbles...)
}

// ParseADPCM splits an IMA ADPCM payload into its state and nibble data (a
// sub-slice of payload).
func ParseADPCM(payload []byte) (ADPCMState, []byte, error) {
	if len(payload) < ADPCMPrefixSize {
		return ADPCMState{}, nil, ErrPayloadTooShort
	}
	st := ADPCMState{
		Predictor: int16(binary.LittleEndian.Uint16(payload[0:])),
		StepIndex: payload[2],
	}
	if st.StepIndex > MaxADPCMStepIndex {
		return ADPCMState{}, nil, ErrInvalidADPCMState
	}
	return st, payload[ADPCMPrefixSize:], nil
}
