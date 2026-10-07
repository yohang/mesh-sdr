package dsp

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// FrameDuration is the audio frame length (§2.3: 20 ms frames; ADPCM
// framing stays within its 10 ms budget per frame boundary).
const FrameDuration = 20 * time.Millisecond

// AudioFrame is one encoded audio frame.
type AudioFrame struct {
	Codec   rxv1.Codec
	Payload []byte
	Samples int
	// Time is the time of the first IQ sample of the frame.
	Time time.Time
	// Squelched is set when the frame is silence from a closed squelch.
	Squelched bool
	// Reset is set on the first frame after a codec (re)configuration.
	Reset bool
}

// Duration returns the audio duration of the frame at rate.
func (f AudioFrame) Duration(rate int) time.Duration {
	return time.Duration(f.Samples) * time.Second / time.Duration(rate)
}

// Framer cuts float audio into 20 ms frames encoded as PCM s16le or IMA
// ADPCM (§6.7). Opus is not provided by this node (ADR 0019).
type Framer struct {
	rate      int
	codec     rxv1.Codec
	frameLen  int
	buf       []int16
	start     time.Time
	squelched bool
	reset     bool
	enc       ADPCMEncoder
}

// NewFramer returns a framer for codec at rate.
func NewFramer(codec rxv1.Codec, rate int) (*Framer, error) {
	if codec != rxv1.CodecPCMS16LE && codec != rxv1.CodecADPCMIMA {
		return nil, fmt.Errorf("%w: audio codec %s", ErrChain, codec)
	}

	n := rate * int(FrameDuration/time.Millisecond) / 1000
	n += n % 2

	return &Framer{rate: rate, codec: codec, frameLen: n, buf: make([]int16, 0, n), reset: true}, nil
}

// Codec returns the codec.
func (f *Framer) Codec() rxv1.Codec { return f.codec }

// Rate returns the sample rate.
func (f *Framer) Rate() int { return f.rate }

// Push appends audio whose first sample is at t; emit receives every
// completed frame. A frame is marked squelched when any of its samples
// came from a closed squelch.
func (f *Framer) Push(audio []float32, t time.Time, squelched bool, emit func(AudioFrame)) {
	for i, v := range audio {
		if len(f.buf) == 0 {
			f.start = t.Add(time.Duration(i) * time.Second / time.Duration(f.rate))
			f.squelched = false
		}

		f.squelched = f.squelched || squelched
		f.buf = append(f.buf, toS16(v))

		if len(f.buf) == f.frameLen {
			emit(f.encode())
			f.buf = f.buf[:0]
		}
	}
}

func (f *Framer) encode() AudioFrame {
	fr := AudioFrame{Codec: f.codec, Samples: len(f.buf), Time: f.start, Squelched: f.squelched, Reset: f.reset}
	f.reset = false

	switch f.codec {
	case rxv1.CodecADPCMIMA:
		pred, idx := f.enc.State()
		p := rxv1.AppendADPCM(make([]byte, 0, rxv1.ADPCMPrefixSize+len(f.buf)/2), rxv1.ADPCMState{Predictor: pred, StepIndex: idx}, nil)
		fr.Payload = f.enc.Encode(p, f.buf)
	default:
		p := make([]byte, 0, 2*len(f.buf))
		for _, s := range f.buf {
			p = binary.LittleEndian.AppendUint16(p, uint16(s))
		}

		fr.Payload = p
	}

	return fr
}

func toS16(v float32) int16 {
	x := math.Round(float64(v) * 32767)

	return int16(min(max(x, -32768), 32767))
}
