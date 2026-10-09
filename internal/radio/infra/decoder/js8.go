package decoder

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/yohang/mesh-sdr/internal/radio/app"
)

// The JS8 frame parser is a port of js8py (Jakob Ketterl, GPL-3.0), itself
// a port of the JS8Call 2.2 varicode (Jordan Sherer, GPL-3.0), DEC-029.

// js8Profiles returns the JS8 slot decoders of the enabled speeds.
func js8Profiles(s Settings) []profile {
	depth := s.JS8Depth
	if depth <= 0 {
		depth = 3
	}

	var out []profile

	for _, name := range js8SpeedOrder {
		sp, ok := js8Speeds[name]
		if !ok || !slices.Contains(s.JS8Profiles, name) {
			continue
		}

		out = append(out, profile{
			mode: "js8", period: sp.period, submode: sp.submode, tool: "js8", job: sp.period * 8 / 10, parse: parseJS8,
			args: func(file, dir string) []string {
				return []string{"--js8", "-b", sp.submode, "-a", dir, "-t", dir, "-d", strconv.Itoa(depth), file}
			},
		})
	}

	return out
}

// JS8Schema names the js8.v1 payload.
const JS8Schema = "js8.v1"

// JS8 frame types.
const (
	JS8Heartbeat        = "heartbeat"
	JS8Compound         = "compound"
	JS8CompoundDirected = "compound_directed"
	JS8Directed         = "directed"
	JS8Data             = "data"
	JS8DataCompressed   = "data_compressed"
)

// JS8Record is the js8.v1 payload (DEC-029): the columns of a js8 decode
// and its frame. Heartbeat and compound frames keep the sender's locator
// here until the map (M3, ADR 0028). ThreadType holds the frame's first
// (bit 1) and last (bit 2) flags of a multi-frame message (DEC-030).
type JS8Record struct {
	DB float64 `json:"db"`
	DT float64 `json:"dt"`
	// DF is the audio frequency of the signal (Hz above the dial).
	DF int64 `json:"df"`
	// Submode is A (normal), B (fast), C (turbo) or E (slow).
	Submode    string `json:"submode"`
	ThreadType int    `json:"thread_type"`
	Frame      string `json:"frame"`
	// Callsign is the sender; To the station or group it calls, Cmd the
	// directed command, SNR its report.
	Callsign string `json:"callsign,omitempty"`
	To       string `json:"to,omitempty"`
	Cmd      string `json:"cmd,omitempty"`
	SNR      *int   `json:"snr,omitempty"`
	Locator  string `json:"locator,omitempty"`
	// PartialSlot: the slot lost more than 10 % of its audio (DEC-026).
	PartialSlot bool `json:"partial_slot,omitempty"`
}

// parseJS8 parses a js8 decode:
//
//	122600 -13  0.3  697 A  yHYCHYCG++++         2
//
// time, dB, DT, audio frequency, submode, the 12-character frame and its
// thread type.
func parseJS8(_ profile, line string, partial bool) (app.DecodeRecord, bool) {
	if noise.MatchString(line) || len(line) < 46 {
		return app.DecodeRecord{}, false
	}

	db, err1 := strconv.ParseFloat(field(line, 7, 10), 64)
	dt, err2 := strconv.ParseFloat(field(line, 11, 15), 64)
	df, err3 := strconv.ParseInt(field(line, 16, 20), 10, 64)
	thread, err4 := strconv.Atoi(line[45:46])

	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return app.DecodeRecord{}, false
	}

	rec := JS8Record{DB: db, DT: dt, DF: df, Submode: line[21:22], ThreadType: thread, PartialSlot: partial}

	text, err := parseJS8Frame(field(line, 24, 45), &rec)
	if err != nil {
		return app.DecodeRecord{}, false
	}

	payload, err := json.Marshal(rec)
	if err != nil {
		return app.DecodeRecord{}, false
	}

	return app.DecodeRecord{Schema: JS8Schema, Text: text, Payload: payload, AudioHz: df}, true
}

const (
	js8Alphanumeric = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ /@"
	js8Alphabet72   = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-+/?."

	js8NBaseCall = 37 * 36 * 10 * 27 * 27 * 27
	js8NBaseGrid = 180 * 180
	js8NUserGrid = js8NBaseGrid + 10
	js8NMaxGrid  = 1<<15 - 1
)

var errJS8Frame = errors.New("invalid js8 frame")

// js8Bits unpacks the 12 characters of a frame into its 72 bits.
func js8Bits(payload string) ([]byte, error) {
	if len(payload) != 12 {
		return nil, errJS8Frame
	}

	bits := make([]byte, 0, 72)

	for i := range len(payload) {
		v := strings.IndexByte(js8Alphabet72, payload[i])
		if v < 0 {
			return nil, errJS8Frame
		}

		for b := 5; b >= 0; b-- {
			bits = append(bits, byte(v>>b&1))
		}
	}

	return bits, nil
}

func bitsInt(bits []byte) int {
	n := 0
	for _, b := range bits {
		n = n<<1 | int(b)
	}

	return n
}

// parseJS8Frame decodes a frame into rec and returns its text.
func parseJS8Frame(payload string, rec *JS8Record) (string, error) {
	bits, err := js8Bits(payload)
	if err != nil {
		return "", err
	}

	switch {
	case bits[0] == 1 && bits[1] == 1:
		rec.Frame = JS8DataCompressed

		return jscDecompress(bits[2:])
	case bits[0] == 1:
		rec.Frame = JS8Data

		return huffmanDecode(bits[2:])
	case bits[1] == 1 && bits[2] == 1:
		rec.Frame = JS8Directed

		return js8Directed(bits, rec), nil
	case bits[1] == 1:
		rec.Frame = JS8CompoundDirected

		return js8Compound(bits, rec, true), nil
	case bits[2] == 1:
		rec.Frame = JS8Compound

		return js8Compound(bits, rec, false), nil
	default:
		rec.Frame = JS8Heartbeat

		return js8Heartbeat(bits, rec), nil
	}
}

// js8Commands are the directed commands by value, in js8py's order (the
// first name of a value wins).
var js8Commands = []struct {
	name  string
	value int
}{
	{" HB", -1}, {" SNR?", 0}, {"?", 0}, {" DIT DIT", 1}, {" NACK", 2}, {" HEARING?", 3}, {" GRID?", 4}, {">", 5},
	{" STATUS?", 6}, {" STATUS", 7}, {" HEARING", 8}, {" MSG", 9}, {" MSG TO:", 10}, {" QUERY", 11}, {" QUERY MSGS", 12},
	{" QUERY MSGS?", 12}, {" QUERY CALL", 13}, {" GRID", 15}, {" INFO?", 16}, {" INFO", 17}, {" FB", 18}, {" HW CPY?", 19},
	{" SK", 20}, {" RR", 21}, {" QSL?", 22}, {" QSL", 23}, {" CMD", 24}, {" SNR", 25}, {" NO", 26}, {" YES", 27}, {" 73", 28},
	{" ACK", 29}, {" AGN?", 30}, {"  ", 31}, {" ", 31},
}

// js8Groups are the group calls (@ALLCALL…) by their value above the base
// calls.
var js8Groups = []string{
	"<....>", "@ALLCALL", "@JS8NET", "@DX/NA", "@DX/SA", "@DX/EU", "@DX/AS", "@DX/AF", "@DX/OC", "@DX/AN",
	"@REGION/1", "@REGION/2", "@REGION/3", "@GROUP/0", "@GROUP/1", "@GROUP/2", "@GROUP/3", "@GROUP/4", "@GROUP/5",
	"@GROUP/6", "@GROUP/7", "@GROUP/8", "@GROUP/9", "@COMMAND", "@CONTROL", "@NET", "@NTS", "@RESERVE/0", "@RESERVE/1",
	"@RESERVE/2", "@RESERVE/3", "@RESERVE/4", "@APRSIS", "@RAGCHEW", "@JS8", "@EMCOMM", "@ARES", "@MARS", "@AMRRON",
	"@RACES", "@RAYNET", "@RADAR", "@SKYWARN",
}

var (
	js8CQs = []string{"CQ CQ CQ", "CQ DX", "CQ QRP", "CQ CONTEST", "CQ FIELD", "CQ FD", "CQ CQ", "CQ"}
	js8HBs = []string{"HB", "HB AUTO", "HB AUTO RELAY", "HB AUTO RELAY SPOT", "HB RELAY", "HB RELAY SPOT", "HB SPOT", "HB AUTO SPOT"}
)

// js8Directed: "FROM: TO CMD [SNR] ".
func js8Directed(bits []byte, rec *JS8Record) string {
	rec.Callsign = js8Callsign(bitsInt(bits[3:31]), bits[64] == 1)
	rec.To = js8Callsign(bitsInt(bits[31:59]), bits[65] == 1)

	cmd := bitsInt(bits[59:64])
	for _, c := range js8Commands {
		if c.value == cmd%32 {
			rec.Cmd = c.name

			break
		}
	}

	if extra := bitsInt(bits[66:72]); extra != 0 && (cmd == 25 || cmd == 29) {
		snr := extra - 31
		rec.SNR = &snr
	}

	text := rec.Callsign + ": " + rec.To + rec.Cmd + " "
	if rec.SNR != nil {
		text += signed(*rec.SNR) + " "
	}

	rec.Cmd = strings.TrimSpace(rec.Cmd)

	return text
}

// signed formats a report as Python's "{:0=+3}": -4 → "-04", 0 → "+00".
func signed(n int) string {
	if n < 0 {
		return fmt.Sprintf("-%02d", -n)
	}

	return fmt.Sprintf("+%02d", n)
}

// js8Callsign unpacks a 28-bit call (or group).
func js8Callsign(value int, portable bool) string {
	if i := value - js8NBaseCall - 1; i >= 0 && i < len(js8Groups) {
		return js8Groups[i]
	}

	word := make([]byte, 6)
	for i, mod := range []int{27, 27, 27, 10, 36} {
		pos := 5 - i
		v := value % mod
		value /= mod

		if mod == 27 {
			v += 10
		}

		word[pos] = js8Alphanumeric[v]
	}

	if value >= len(js8Alphanumeric) {
		return ""
	}

	word[0] = js8Alphanumeric[value]
	call := string(word)

	if strings.HasPrefix(call, "3D0") {
		call = "3DA0" + call[3:]
	}

	if len(call) > 1 && call[0] == 'Q' && call[1] >= 'A' && call[1] <= 'Z' {
		call = "3X" + call[1:]
	}

	call = strings.TrimSpace(call)
	if portable {
		call += "/P"
	}

	return call
}

// js8Alphanumeric50 unpacks the 50-bit call of compound and heartbeat
// frames.
func js8Alphanumeric50(packed int) string {
	word := make([]byte, 11)

	for pos := 10; pos >= 0; pos-- {
		switch pos {
		case 7, 3:
			if packed%2 == 1 {
				word[pos] = '/'
			} else {
				word[pos] = ' '
			}

			packed /= 2
		case 0:
			word[pos] = js8Alphanumeric[packed%39]
		default:
			word[pos] = js8Alphanumeric[packed%38]
			packed /= 38
		}
	}

	return strings.ReplaceAll(string(word), " ", "")
}

// js8Grid unpacks a 4-character Maidenhead locator ("" when none).
func js8Grid(value int) string {
	if value > js8NBaseGrid {
		return ""
	}

	dlat := float64(value%180 - 90)
	dlong := float64(value)/180*2 - 180

	if dlong < -180 {
		dlong += 360
	}

	if dlong > 180 {
		dlong -= 360
	}

	nlong := int(60 * (180 - dlong) / 5)
	nlat := int(60 * (dlat + 90) / 2.5)

	return string([]byte{byte('A' + nlong/240), byte('A' + nlat/240), byte('0' + nlong%240/24), byte('0' + nlat%240/24)})
}

// js8Compound: "CALL:" (compound) or "CALL [GRID|CMD SNR]" (compound
// directed).
func js8Compound(bits []byte, rec *JS8Record, directed bool) string {
	rec.Callsign = js8Alphanumeric50(bitsInt(bits[3:53]))

	extra := bitsInt(bits[53:69])

	switch {
	case extra <= js8NBaseGrid:
		rec.Locator = js8Grid(extra)
	case extra >= js8NUserGrid && extra < js8NMaxGrid:
		extra -= js8NUserGrid
		if extra&(1<<7) != 0 {
			rec.Cmd = "SNR"
			if extra&(1<<6) != 0 {
				rec.Cmd = "ACK"
			}

			snr := extra&(1<<6-1) - 31
			rec.SNR = &snr
		}
	}

	if !directed {
		return rec.Callsign + ":"
	}

	switch {
	case rec.Locator != "":
		return rec.Callsign + " " + rec.Locator
	case rec.Cmd != "" && rec.SNR != nil && *rec.SNR != 0:
		return rec.Callsign + " " + rec.Cmd + " " + signed(*rec.SNR)
	}

	return rec.Callsign
}

// js8Heartbeat: "CALL: CQ|HB … GRID".
func js8Heartbeat(bits []byte, rec *JS8Record) string {
	rec.Callsign = js8Alphanumeric50(bitsInt(bits[3:53]))
	rec.Locator = js8Grid(bitsInt(bits[54:69]))

	msgs := js8HBs
	if bits[53] == 1 {
		msgs = js8CQs
	}

	rec.Cmd = msgs[bitsInt(bits[69:72])]

	return rec.Callsign + ": " + rec.Cmd + " " + rec.Locator
}

// js8Huffman is the JS8Call Huffman table of uncompressed data frames.
var js8Huffman = map[string]string{
	"01": " ", "100": "E", "1101": "T", "0011": "A", "11111": "O", "11100": "I", "10111": "N", "10100": "S", "00011": "H",
	"00000": "R", "111011": "D", "110011": "L", "110001": "C", "101101": "U", "101011": "M", "001011": "W", "001001": "F",
	"000101": "G", "000011": "Y", "1111011": "P", "1111001": "B", "1110100": ".", "1100101": "V", "1100100": "K", "1100001": "-",
	"1100000": "+", "1011001": "?", "1011000": "!", "1010101": "\"", "1010100": "X", "0010101": "0", "0010100": "J",
	"0010001": "1", "0010000": "Q", "0001001": "2", "0001000": "Z", "0000101": "3", "0000100": "5", "11110101": "4",
	"11110100": "9", "11110001": "8", "11110000": "6", "11101011": "7", "11101010": "/",
}

// huffmanDecode decodes a data frame: the bits up to the last 0 (the
// padding is 0 then ones).
func huffmanDecode(bits []byte) (string, error) {
	last := bytes.LastIndexByte(bits, 0)
	if last < 0 {
		return "", errJS8Frame
	}

	rem := make([]byte, last)
	for i, b := range bits[:last] {
		rem[i] = '0' + b
	}

	var out strings.Builder

	// As js8py: codes are read while more than 2 bits are left.
	for len(rem) > 2 {
		found := false

		for n := 2; n <= 8 && n <= len(rem); n++ {
			if c, ok := js8Huffman[string(rem[:n])]; ok {
				out.WriteString(c)
				rem, found = rem[n:], true

				break
			}
		}

		if !found {
			break
		}
	}

	return out.String(), nil
}

//go:embed jsc_map.bin.gz
var jscMapGz []byte

// jscMap is the JSC dictionary of JS8Call 2.2 (jsc_map.cpp: 262 144
// words, Latin-1 converted to UTF-8, NUL separated, gzip), loaded on the
// first compressed frame.
var jscMap = sync.OnceValues(func() ([]string, error) {
	r, err := gzip.NewReader(bytes.NewReader(jscMapGz))
	if err != nil {
		return nil, err
	}

	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	words := strings.Split(string(b), "\x00")
	if len(words) != jscSize {
		return nil, fmt.Errorf("jsc map: %d words", len(words))
	}

	return words, nil
})

const jscSize = 262144

// jscDecompress decodes a compressed data frame (JS8Call JSC::decompress).
func jscDecompress(bits []byte) (string, error) {
	words, err := jscMap()
	if err != nil {
		return "", err
	}

	const s, c = 7, 1<<4 - 7

	var base [8]int
	for k := 1; k < 8; k++ {
		p := s
		for range k - 1 {
			p *= c
		}

		base[k] = base[k-1] + p
	}

	var nibbles, separators []int

	for i := 0; i+4 <= len(bits); {
		v := bitsInt(bits[i : i+4])
		nibbles = append(nibbles, v)
		i += 4

		if v < s {
			if i < len(bits) && bits[i] == 1 {
				separators = append(separators, len(nibbles)-1)
			}

			i++
		}
	}

	var out strings.Builder

	for start := 0; start < len(nibbles); {
		k, j := 0, 0

		for start+k < len(nibbles) && nibbles[start+k] >= s {
			j = j*c + nibbles[start+k] - s
			k++
		}

		// A word has at most 7 continuation nibbles (base): a longer run
		// is not a valid frame (JS8Call reads past its table there).
		if k >= len(base) || j >= jscSize || start+k >= len(nibbles) {
			break
		}

		j = j*s + nibbles[start+k] + base[k]
		if j >= jscSize {
			break
		}

		out.WriteString(words[j])

		if len(separators) > 0 && separators[0] == start+k {
			out.WriteByte(' ')
			separators = separators[1:]
		}

		start += k + 1
	}

	return out.String(), nil
}
