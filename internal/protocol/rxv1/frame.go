package rxv1

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Binary frame header constants (§6.7).
const (
	HeaderSize   = 24
	Magic        = 0xA5
	FrameVersion = 1
)

// Header field offsets (§6.7).
const (
	offMagic      = 0
	offVersion    = 1
	offType       = 2
	offCodec      = 3
	offStreamID   = 4
	offFlags      = 6
	offSeq        = 8
	offTimestamp  = 12
	offPayloadLen = 20
)

// FrameType identifies the content of a binary frame.
type FrameType uint8

// Frame types (§6.7). 0x05–0xFF are reserved.
const (
	FrameFFT          FrameType = 0x01
	FrameAudio        FrameType = 0x02
	FrameSecondaryFFT FrameType = 0x03
	FrameHDAudio      FrameType = 0x04
)

// Known reports whether t is a defined (non-reserved) frame type.
func (t FrameType) Known() bool { return t >= FrameFFT && t <= FrameHDAudio }

// IsAudio reports whether t carries audio (0x02, 0x04).
func (t FrameType) IsAudio() bool { return t == FrameAudio || t == FrameHDAudio }

// IsFFT reports whether t carries a spectrum line (0x01, 0x03).
func (t FrameType) IsFFT() bool { return t == FrameFFT || t == FrameSecondaryFFT }

func (t FrameType) String() string {
	switch t {
	case FrameFFT:
		return "fft"
	case FrameAudio:
		return "audio"
	case FrameSecondaryFFT:
		return "fft2"
	case FrameHDAudio:
		return "audio_hd"
	default:
		return fmt.Sprintf("reserved(0x%02x)", uint8(t))
	}
}

// Codec identifies the payload layout of a binary frame.
type Codec uint8

// Codecs (§6.7).
const (
	CodecPCMS16LE Codec = 0x00
	CodecADPCMIMA Codec = 0x01
	CodecOpus     Codec = 0x02
	CodecFFTU8DB  Codec = 0x10
	CodecFFTF32DB Codec = 0x11
)

// Known reports whether c is a defined codec.
func (c Codec) Known() bool {
	switch c {
	case CodecPCMS16LE, CodecADPCMIMA, CodecOpus, CodecFFTU8DB, CodecFFTF32DB:
		return true
	}
	return false
}

// IsAudio reports whether c is an audio codec.
func (c Codec) IsAudio() bool { return c == CodecPCMS16LE || c == CodecADPCMIMA || c == CodecOpus }

// IsFFT reports whether c is an FFT codec.
func (c Codec) IsFFT() bool { return c == CodecFFTU8DB || c == CodecFFTF32DB }

func (c Codec) String() string {
	switch c {
	case CodecPCMS16LE:
		return "pcm-s16le"
	case CodecADPCMIMA:
		return "adpcm-ima"
	case CodecOpus:
		return "opus"
	case CodecFFTU8DB:
		return "u8-db"
	case CodecFFTF32DB:
		return "f32-db"
	default:
		return fmt.Sprintf("unknown(0x%02x)", uint8(c))
	}
}

// Flags is the 16-bit flags field.
type Flags uint16

// Flag bits (§6.7). Bits 4–15 are reserved and MUST be 0.
const (
	FlagDiscontinuity Flags = 1 << 0
	FlagReset         Flags = 1 << 1
	FlagEndOfStream   Flags = 1 << 2
	FlagSquelched     Flags = 1 << 3

	flagsDefined  = FlagDiscontinuity | FlagReset | FlagEndOfStream | FlagSquelched
	FlagsReserved = ^flagsDefined
)

// Has reports whether all bits of f2 are set in f.
func (f Flags) Has(f2 Flags) bool { return f&f2 == f2 }

// FrameHeader is the decoded 24-byte binary frame header. Magic and version
// are implicit.
type FrameHeader struct {
	Type        FrameType
	Codec       Codec
	StreamID    uint16
	Flags       Flags
	Seq         uint32
	TimestampUS uint64
	// PayloadLen is set by AppendFrame and read by ParseFrame.
	PayloadLen uint32
}

// Binary frame errors. Receivers drop the frame on any of them (§6.7).
var (
	ErrFrameTooShort           = errors.New("rxv1: binary frame shorter than header")
	ErrBadMagic                = errors.New("rxv1: bad binary frame magic")
	ErrUnsupportedFrameVersion = errors.New("rxv1: unsupported binary frame version")
	ErrPayloadLength           = errors.New("rxv1: payload_len does not match frame length")
	ErrFrameTooLarge           = errors.New("rxv1: binary frame larger than 64 KiB")
	ErrInvalidHeader           = errors.New("rxv1: invalid binary frame header")
)

// Validate checks the semantic rules a sender MUST respect: defined type and
// codec, codec family matching the frame type, reserved flag bits cleared.
// ParseFrame does not call it, so a receiver can choose how strict to be.
func (h FrameHeader) Validate() error {
	switch {
	case !h.Type.Known():
		return fmt.Errorf("%w: frame type %s", ErrInvalidHeader, h.Type)
	case !h.Codec.Known():
		return fmt.Errorf("%w: codec %s", ErrInvalidHeader, h.Codec)
	case h.Type.IsAudio() && !h.Codec.IsAudio(), h.Type.IsFFT() && !h.Codec.IsFFT():
		return fmt.Errorf("%w: codec %s on %s frame", ErrInvalidHeader, h.Codec, h.Type)
	case h.Flags&FlagsReserved != 0:
		return fmt.Errorf("%w: reserved flag bits 0x%04x set", ErrInvalidHeader, uint16(h.Flags&FlagsReserved))
	}
	return nil
}

// AppendFrame appends the header and payload to dst and returns the extended
// slice. It validates h, sets PayloadLen from payload and enforces
// MaxFrameBytes.
func AppendFrame(dst []byte, h FrameHeader, payload []byte) ([]byte, error) {
	if err := h.Validate(); err != nil {
		return dst, err
	}
	if HeaderSize+len(payload) > MaxFrameBytes {
		return dst, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, HeaderSize+len(payload))
	}
	h.PayloadLen = uint32(len(payload))
	dst = appendHeader(dst, h)
	return append(dst, payload...), nil
}

func appendHeader(dst []byte, h FrameHeader) []byte {
	dst = append(dst, Magic, FrameVersion, byte(h.Type), byte(h.Codec))
	dst = binary.LittleEndian.AppendUint16(dst, h.StreamID)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(h.Flags))
	dst = binary.LittleEndian.AppendUint32(dst, h.Seq)
	dst = binary.LittleEndian.AppendUint64(dst, h.TimestampUS)
	return binary.LittleEndian.AppendUint32(dst, h.PayloadLen)
}

// ParseFrame decodes the header of one binary frame and returns it with the
// payload (a sub-slice of frame, not a copy). It checks the structural rules
// only: length, magic, version and payload_len == len(frame) − 24.
func ParseFrame(frame []byte) (FrameHeader, []byte, error) {
	if len(frame) < HeaderSize {
		return FrameHeader{}, nil, ErrFrameTooShort
	}
	if frame[offMagic] != Magic {
		return FrameHeader{}, nil, ErrBadMagic
	}
	if frame[offVersion] != FrameVersion {
		return FrameHeader{}, nil, fmt.Errorf("%w: %d", ErrUnsupportedFrameVersion, frame[offVersion])
	}
	h := FrameHeader{
		Type:        FrameType(frame[offType]),
		Codec:       Codec(frame[offCodec]),
		StreamID:    binary.LittleEndian.Uint16(frame[offStreamID:]),
		Flags:       Flags(binary.LittleEndian.Uint16(frame[offFlags:])),
		Seq:         binary.LittleEndian.Uint32(frame[offSeq:]),
		TimestampUS: binary.LittleEndian.Uint64(frame[offTimestamp:]),
		PayloadLen:  binary.LittleEndian.Uint32(frame[offPayloadLen:]),
	}
	if uint64(h.PayloadLen) != uint64(len(frame)-HeaderSize) {
		return FrameHeader{}, nil, ErrPayloadLength
	}
	return h, frame[HeaderSize:], nil
}

// SetFlags ORs f into the flags of an encoded frame in place. Send queues use
// it to mark the next audio frame after a drop as a discontinuity without
// re-encoding.
func SetFlags(frame []byte, f Flags) error {
	if len(frame) < HeaderSize || frame[offMagic] != Magic {
		return ErrFrameTooShort
	}
	cur := binary.LittleEndian.Uint16(frame[offFlags:])
	binary.LittleEndian.PutUint16(frame[offFlags:], cur|uint16(f))
	return nil
}

// SeqGap returns the number of frames missing between prev and next on one
// stream, taking the 2³² wrap into account. Consecutive frames give 0.
func SeqGap(prev, next uint32) uint32 { return next - prev - 1 }
