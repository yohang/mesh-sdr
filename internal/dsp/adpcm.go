package dsp

// IMA ADPCM in the rx.v1 layout (§6.7, codec 0x01): every frame is
// self-contained, with a 4-byte state prefix and the nibbles low nibble
// first; no sync words (ADR 0014: written in Go, not csdr's encoder).

var adpcmSteps = [89]int{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230,
	253, 279, 307, 337, 371, 408, 449, 494, 544, 598, 658, 724, 796, 876, 963,
	1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024, 3327,
	3660, 4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493, 10442,
	11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794,
	32767,
}

var adpcmIndex = [16]int{-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8}

// ADPCMEncoder is a stateful IMA ADPCM encoder; its state carries over
// from frame to frame and is written in each frame's prefix.
type ADPCMEncoder struct {
	pred int
	idx  int
}

// State returns the predictor and step index for the next frame prefix.
func (e *ADPCMEncoder) State() (int16, uint8) { return int16(e.pred), uint8(e.idx) }

// Reset returns the encoder to its initial state.
func (e *ADPCMEncoder) Reset() { e.pred, e.idx = 0, 0 }

// Encode appends the nibbles of an even number of samples to dst.
func (e *ADPCMEncoder) Encode(dst []byte, samples []int16) []byte {
	for i := 0; i+1 < len(samples); i += 2 {
		lo := e.encode(samples[i])
		hi := e.encode(samples[i+1])
		dst = append(dst, lo|hi<<4)
	}

	return dst
}

func (e *ADPCMEncoder) encode(s int16) byte {
	step := adpcmSteps[e.idx]
	diff := int(s) - e.pred
	code := 0

	if diff < 0 {
		code = 8
		diff = -diff
	}

	delta := step >> 3

	if diff >= step {
		code |= 4
		diff -= step
		delta += step
	}

	step >>= 1
	if diff >= step {
		code |= 2
		diff -= step
		delta += step
	}

	step >>= 1
	if diff >= step {
		code |= 1
		delta += step
	}

	if code&8 != 0 {
		e.pred -= delta
	} else {
		e.pred += delta
	}

	e.pred = min(max(e.pred, -32768), 32767)
	e.idx = min(max(e.idx+adpcmIndex[code], 0), 88)

	return byte(code)
}

// DecodeADPCM decodes nibbles from the state prefix values (reference
// decoder for tests and clients).
func DecodeADPCM(pred int16, idx uint8, data []byte) []int16 {
	p, ix := int(pred), min(int(idx), 88)
	out := make([]int16, 0, len(data)*2)

	dec := func(code int) {
		step := adpcmSteps[ix]
		diff := step >> 3

		if code&4 != 0 {
			diff += step
		}

		if code&2 != 0 {
			diff += step >> 1
		}

		if code&1 != 0 {
			diff += step >> 2
		}

		if code&8 != 0 {
			p -= diff
		} else {
			p += diff
		}

		p = min(max(p, -32768), 32767)
		ix = min(max(ix+adpcmIndex[code], 0), 88)
		out = append(out, int16(p))
	}

	for _, b := range data {
		dec(int(b & 0x0f))
		dec(int(b >> 4))
	}

	return out
}
