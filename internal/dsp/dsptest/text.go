// Package dsptest synthesises the test signals of the native text decoders
// (DEC-006 to DEC-012, MAR-002, MAR-003): BPSK with Varicode, RTTY with
// ITA2 (Baudot), SITOR-B with CCIR 476 and FEC repetition, DSC with CCIR
// 493 and time diversity, and Morse code. The signals are
// complex baseband IQ with the signal at an offset, as the selector of a
// USB demodulator delivers it. Tests and the dev fake connector use them.
package dsptest

import (
	"math"
	"math/cmplx"
	"strings"
)

// varicode is the PSK31 alphabet (G3PLX), ASCII 32 to 126.
var varicode = map[byte]string{
	' ': "1", '!': "111111111", '"': "101011111", '#': "111110101", '$': "111011011", '%': "1011010101",
	'&': "1010111011", '\'': "101111111", '(': "11111011", ')': "11110111", '*': "101101111", '+': "111011111",
	',': "1110101", '-': "110101", '.': "1010111", '/': "110101111", '0': "10110111", '1': "10111101",
	'2': "11101101", '3': "11111111", '4': "101110111", '5': "101011011", '6': "101101011", '7': "110101101",
	'8': "110101011", '9': "110110111", ':': "11110101", ';': "110111101", '<': "111101101", '=': "1010101",
	'>': "111010111", '?': "1010101111", '@': "1010111101", 'A': "1111101", 'B': "11101011", 'C': "10101101",
	'D': "10110101", 'E': "1110111", 'F': "11011011", 'G': "11111101", 'H': "101010101", 'I': "1111111",
	'J': "111111101", 'K': "101111101", 'L': "11010111", 'M': "10111011", 'N': "11011101", 'O': "10101011",
	'P': "11010101", 'Q': "111011101", 'R': "10101111", 'S': "1101111", 'T': "1101101", 'U': "101010111",
	'V': "110110101", 'W': "101011101", 'X': "101110101", 'Y': "101111011", 'Z': "1010101101", 'a': "1011",
	'b': "1011111", 'c': "101111", 'd': "101101", 'e': "11", 'f': "111101", 'g': "1011011", 'h': "101011",
	'i': "1101", 'j': "111101011", 'k': "10111111", 'l': "11011", 'm': "111011", 'n': "1111", 'o': "111",
	'p': "111111", 'q': "110111111", 'r': "10101", 's': "10111", 't': "101", 'u': "110111", 'v': "1111011",
	'w': "1101011", 'x': "11011111", 'y': "1011101", 'z': "111010101", '\n': "11101", '\r': "11111",
}

// PSK returns DBPSK with Varicode at baud (31.25 or 62.5) and offsetHz:
// an idle preamble (phase reversals), text, an idle tail. Each symbol is a
// raised cosine pulse two symbols wide, so a reversal crosses zero
// amplitude between symbols.
func PSK(text string, baud, rate, offsetHz float64) []complex64 {
	var bits strings.Builder

	bits.WriteString(strings.Repeat("0", 64))

	for i := range len(text) {
		if code, ok := varicode[text[i]]; ok {
			bits.WriteString(code)
			bits.WriteString("00")
		}
	}

	bits.WriteString(strings.Repeat("0", 64))

	// A 0 bit is a phase reversal, a 1 bit keeps the phase.
	symbols := make([]float64, 0, bits.Len()+1)
	phase := 1.0

	symbols = append(symbols, phase)

	for _, b := range bits.String() {
		if b == '0' {
			phase = -phase
		}

		symbols = append(symbols, phase)
	}

	spb := rate / baud
	n := int(float64(len(symbols)) * spb)
	out := make([]complex64, n)

	for i := range out {
		t := float64(i) / spb
		k := int(t)
		frac := t - float64(k)
		// Pulse k peaks at the middle of symbol k: the amplitude between
		// two symbol centres is a cosine crossfade.
		var a float64

		if frac < 0.5 {
			prev := symbols[max(k-1, 0)]
			w := (1 - math.Cos(math.Pi*(frac+0.5))) / 2
			a = prev*(1-w) + symbols[k]*w
		} else {
			next := symbols[min(k+1, len(symbols)-1)]
			w := (1 - math.Cos(math.Pi*(frac-0.5))) / 2
			a = symbols[k]*(1-w) + next*w
		}

		out[i] = complex64(cmplx.Rect(0.5*a, 2*math.Pi*offsetHz*float64(i)/rate))
	}

	return out
}

// fsk returns continuous-phase FSK of bits (one float per bit: +1 the
// upper tone, −1 the lower) at baud with shiftHz between the tones.
func fsk(bits []float64, baud, shiftHz, rate, offsetHz float64) []complex64 {
	spb := rate / baud
	n := int(float64(len(bits)) * spb)
	out := make([]complex64, n)
	phase := 0.0

	for i := range out {
		b := bits[min(int(float64(i)/spb), len(bits)-1)]
		phase += 2 * math.Pi * (offsetHz + b*shiftHz/2) / rate
		out[i] = complex64(cmplx.Rect(0.5, phase))
	}

	return out
}

// ITA2 letters and figures shifts.
const (
	ita2LTRS = 31
	ita2FIGS = 27
)

// ita2Letters and ita2Figures are the ITA2 codes of the libcsdr++ Baudot
// tables.
var (
	ita2Letters = "\x00E\nA SIU\rDRJNFCKTZLWHYPQOBG\x00MXV\x00"
	ita2Figures = "\x003\n- '87\r$4\a,!:(5+)2#6019?&\x00./=\x00"
)

// RTTY returns ITA2 at baud with shiftHz (one start bit, five data bits
// LSB first, 1.5 stop bits; mark is the upper tone unless invert): idle
// mark, LTRS, text in capitals and figures, idle mark.
func RTTY(text string, baud, shiftHz float64, invert bool, rate, offsetHz float64) []complex64 {
	mark, space := 1.0, -1.0
	if invert {
		mark, space = -1, 1
	}

	// Half bits: the 1.5 stop bits.
	var half []float64

	char := func(code int) {
		half = append(half, space, space)
		for b := range 5 {
			v := space
			if code>>b&1 == 1 {
				v = mark
			}

			half = append(half, v, v)
		}

		half = append(half, mark, mark, mark)
	}

	for range 20 {
		half = append(half, mark)
	}

	char(ita2LTRS)

	figures := false

	for _, r := range strings.ToUpper(text) {
		if i := strings.IndexRune(ita2Letters, r); i > 0 && r != 0 {
			if figures && r != ' ' && r != '\n' && r != '\r' {
				char(ita2LTRS)
				figures = false
			}

			char(i)

			continue
		}

		if i := strings.IndexRune(ita2Figures, r); i > 0 {
			if !figures {
				char(ita2FIGS)
				figures = true
			}

			char(i)
		}
	}

	for range 40 {
		half = append(half, mark)
	}

	return fsk(half, 2*baud, shiftHz, rate, offsetHz)
}

// CCIR 476 control codes.
const (
	ccirLTRS = 90
	ccirFIGS = 54
	ccirSIA  = 15
	ccirRPT  = 102
)

// ccirLetters and ccirFigures map characters to CCIR 476 codes (the
// libcsdr++ tables).
var ccirLetters, ccirFigures = map[rune]int{
	'J': 23, 'F': 27, 'C': 29, 'K': 30, 'W': 39, 'Y': 43, 'P': 45, 'Q': 46, 'G': 53, 'M': 57, 'X': 58, 'V': 60,
	'A': 71, 'S': 75, 'I': 77, 'U': 78, 'D': 83, 'R': 85, 'E': 86, 'N': 89, ' ': 92, 'Z': 99, 'L': 101,
	'H': 105, '\n': 108, 'O': 113, 'B': 114, 'T': 116, '\r': 120,
}, map[rune]int{
	'!': 27, ':': 29, '(': 30, '2': 39, '6': 43, '0': 45, '1': 46, '&': 53, '.': 57, '/': 58, '=': 60,
	'-': 71, '\'': 75, '8': 77, '7': 78, '4': 85, '3': 86, ',': 89, '+': 99, ')': 101, '#': 105, '9': 113,
	'?': 114, '5': 116,
}

// SITORB returns SITOR-B (collective FEC) at 100 Bd with shiftHz: phasing
// (SIA in the DX positions, RPT in the RX ones), then each character in a
// DX position and again two pairs later in an RX position, 7 bits LSB
// first, the code bit 1 on the upper tone.
func SITORB(text string, shiftHz, rate, offsetHz float64) []complex64 {
	codes := []int{ccirLTRS}
	figures := false

	for _, r := range strings.ToUpper(text) {
		if c, ok := ccirLetters[r]; ok {
			if figures && r != ' ' && r != '\n' && r != '\r' {
				codes = append(codes, ccirLTRS)
				figures = false
			}

			codes = append(codes, c)

			continue
		}

		if c, ok := ccirFigures[r]; ok {
			if !figures {
				codes = append(codes, ccirFIGS)
				figures = true
			}

			codes = append(codes, c)
		}
	}

	var pairs [][2]int

	for range 16 {
		pairs = append(pairs, [2]int{ccirSIA, ccirRPT})
	}

	// DX k carries codes[k]; RX k repeats DX k−2.
	for k := range len(codes) + 2 {
		dx, rx := ccirSIA, ccirRPT
		if k < len(codes) {
			dx = codes[k]
		}

		if k >= 2 {
			rx = codes[k-2]
		}

		pairs = append(pairs, [2]int{dx, rx})
	}

	for range 8 {
		pairs = append(pairs, [2]int{ccirSIA, ccirRPT})
	}

	bits := make([]float64, 0, len(pairs)*14)

	for _, p := range pairs {
		for _, c := range p {
			for b := range 7 {
				v := -1.0
				if c>>b&1 == 1 {
					v = 1
				}

				bits = append(bits, v)
			}
		}
	}

	return fsk(bits, 100, shiftHz, rate, offsetHz)
}

// CCIR 493 phasing symbols.
const (
	dscPhaseDX  = 125
	dscPhaseRX0 = 104
	dscPhaseRX7 = 111
)

// DSC returns a DSC call (ITU-R M.493) at 100 Bd with shiftHz, sent twice
// (a decoder releases a call only once more symbols follow it): a dot
// pattern, the phasing sequence (DX 125, RX 111 down to 104), then the
// symbols in the DX positions, each repeated two DX positions later in an
// RX position. A symbol is 10 bits: 7 information bits LSB first then the
// count of their zero bits MSB first; bit 1 (B) is the lower tone.
func DSC(symbols []int, shiftHz, rate, offsetHz float64) []complex64 {
	var bits []float64

	for i := range 200 {
		bits = append(bits, float64(1-2*(i%2)))
	}

	for range 2 {
		// DX k carries dx[k]; RX k carries the RX phasing for the first 8,
		// then repeats DX k−2.
		dx := []int{dscPhaseDX, dscPhaseDX, dscPhaseDX, dscPhaseDX, dscPhaseDX, dscPhaseDX}
		dx = append(dx, symbols...)

		for k := range len(dx) + 2 {
			d, r := dscPhaseDX, dscPhaseRX7-k
			if k < len(dx) {
				d = dx[k]
			}

			if k >= dscPhaseRX7-dscPhaseRX0+1 {
				r = dx[k-2]
			}

			for _, c := range []int{d, r} {
				zeros := 0

				for b := range 7 {
					v := 1
					if c>>b&1 == 0 {
						v, zeros = 0, zeros+1
					}

					bits = append(bits, float64(1-2*v))
				}

				for b := 2; b >= 0; b-- {
					bits = append(bits, float64(1-2*(zeros>>b&1)))
				}
			}
		}
	}

	for range 100 {
		bits = append(bits, 1)
	}

	return fsk(bits, 100, shiftHz, rate, offsetHz)
}

// morse is the International Morse code of letters, digits and a few
// signs.
var morse = map[rune]string{
	'A': ".-", 'B': "-...", 'C': "-.-.", 'D': "-..", 'E': ".", 'F': "..-.", 'G': "--.", 'H': "....", 'I': "..",
	'J': ".---", 'K': "-.-", 'L': ".-..", 'M': "--", 'N': "-.", 'O': "---", 'P': ".--.", 'Q': "--.-", 'R': ".-.",
	'S': "...", 'T': "-", 'U': "..-", 'V': "...-", 'W': ".--", 'X': "-..-", 'Y': "-.--", 'Z': "--..",
	'0': "-----", '1': ".----", '2': "..---", '3': "...--", '4': "....-", '5': ".....", '6': "-....",
	'7': "--...", '8': "---..", '9': "----.", '/': "-..-.", '?': "..--..", '.': ".-.-.-", ',': "--..--",
}

// CW returns Morse code at wpm (PARIS timing) keyed on a tone at offsetHz,
// with 5 ms raised cosine edges, after a second of silence.
func CW(text string, wpm, rate, offsetHz float64) []complex64 {
	dit := 1.2 / wpm
	// Keying: on/off per dit unit.
	var units []bool

	gap := func(n int) {
		for range n {
			units = append(units, false)
		}
	}

	gap(int(1 / dit))

	for _, r := range strings.ToUpper(text) {
		if r == ' ' {
			gap(4) // a word gap is 7 units: 3 after the letter plus 4

			continue
		}

		code, ok := morse[r]
		if !ok {
			continue
		}

		for i, e := range code {
			if i > 0 {
				gap(1)
			}

			n := 1
			if e == '-' {
				n = 3
			}

			for range n {
				units = append(units, true)
			}
		}

		gap(3)
	}

	gap(int(1 / dit))

	spu := dit * rate
	n := int(float64(len(units)) * spu)
	out := make([]complex64, n)
	edge := 0.005 * rate
	env := 0.0

	for i := range out {
		on := units[min(int(float64(i)/spu), len(units)-1)]
		if on {
			env = min(1, env+1/edge)
		} else {
			env = max(0, env-1/edge)
		}

		a := 0.5 * (1 - math.Cos(math.Pi*env)) / 2
		out[i] = complex64(cmplx.Rect(a, 2*math.Pi*offsetHz*float64(i)/rate))
	}

	return out
}

// AddNoise adds white noise of amplitude sigma per component, from a
// deterministic generator.
func AddNoise(iq []complex64, sigma float64, seed uint64) {
	x := seed | 1

	next := func() float64 {
		// xorshift64*, then Box-Muller.
		x ^= x >> 12
		x ^= x << 25
		x ^= x >> 27

		return float64((x*2685821657736338717)>>11) / (1 << 53)
	}

	for i := range iq {
		u1, u2 := max(next(), 1e-12), next()
		r := sigma * math.Sqrt(-2*math.Log(u1))
		iq[i] += complex64(complex(r*math.Cos(2*math.Pi*u2), r*math.Sin(2*math.Pi*u2)))
	}
}
