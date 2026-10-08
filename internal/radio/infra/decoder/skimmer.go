package decoder

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// The CW and RTTY skimmers (DEC-013, DEC-014): csdr-cwskimmer and
// csdr-rttyskimmer decode every signal of the 48 kHz of band above the dial
// (the real part of the 96 kHz wide IQ tap, as OpenWebRX+ does) and print
// "<offset Hz>:<dB>:<characters>" lines. Each line goes to the listener
// (live records, kept in the session's text log, FIL-005); the
// rolling text of each frequency is matched against QSO patterns and the
// callsigns found in the callsign country table are spots, persisted in
// decoded_messages (DEC-015; the reporters come in M4).

// SkimmerSchema names the skimmer.v1 payload.
const SkimmerSchema = "skimmer.v1"

// skimmerChars is the number of characters the tool prints at once
// (OpenWebRX+'s default).
const skimmerChars = 4

// skimmerKeep is the rolling text kept per frequency for the patterns.
const skimmerKeep = 32

// skimmerRate is the input rate of the skimmers (the catalogue's).
const skimmerRate = 96000

// skimmerArgs reads s16le at 96 kHz from stdin.
func skimmerArgs(sessionConfig) []string {
	return []string{"-i", "-r", strconv.Itoa(skimmerRate), "-n", strconv.Itoa(skimmerChars)}
}

// skimmerRules classify the stderr of the skimmers (they only print errors
// there).
var skimmerRules = []process.Rule{
	{Pattern: regexp.MustCompile(`(?i)unrecognized option|excessive file name`), Class: process.ClassFatalConfig},
	{Pattern: regexp.MustCompile(`(?i)failed opening`), Class: process.ClassInputError},
	{Pattern: regexp.MustCompile(`.`), Class: process.ClassInfo},
}

// SkimmerRecord is the skimmer.v1 payload: the text of one signal
// (kind "text", listener only) or a spot (kind "spot").
type SkimmerRecord struct {
	Kind string `json:"kind"`
	// Mode is CW or RTTY.
	Mode string `json:"mode"`
	// OffsetHz is the signal's offset from the dial frequency.
	OffsetHz int64 `json:"offset_hz"`
	DB       int   `json:"db"`
	// Text is the decoded characters (kind text).
	Text string `json:"text,omitempty"`
	// Changed: the dial frequency changed since the previous text.
	Changed bool `json:"changed,omitempty"`
	// Spot: the callsign, the station it calls, its country and the text
	// the pattern matched.
	Callsign    string `json:"callsign,omitempty"`
	Callee      string `json:"callee,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	Country     string `json:"country,omitempty"`
	Message     string `json:"message,omitempty"`
}

var skimmerLine = regexp.MustCompile(`^(\d{1,6}):(-?\d{1,4}):(.+)$`)

// skimmerParser is the parser of a skimmer session: the rolling text per
// frequency and the last dial frequency.
type skimmerParser struct {
	mode    string
	dialNow func() int64
	dial    int64
	text    map[int64]string
}

// newSkimmerParser returns the parser factory of a skimmer mode (CW or
// RTTY).
func newSkimmerParser(mode string) func(sessionConfig) lineParser {
	return func(c sessionConfig) lineParser {
		p := &skimmerParser{mode: mode, dialNow: c.dial, text: map[int64]string{}}

		return p.line
	}
}

func (p *skimmerParser) line(line string, at time.Time) []app.DecodeRecord {
	m := skimmerLine.FindStringSubmatch(line)
	if m == nil {
		return nil
	}

	hz, _ := strconv.ParseInt(m[1], 10, 64)
	db, _ := strconv.Atoi(m[2])
	text := cleanRF(m[3])
	offset := hz

	changed := false

	if p.dialNow != nil {
		if dial := p.dialNow(); dial != p.dial {
			changed = p.dial != 0
			p.dial = dial
			clear(p.text)
		}
	}

	rec := SkimmerRecord{Kind: "text", Mode: p.mode, OffsetHz: offset, DB: db, Text: text, Changed: changed}

	payload, err := json.Marshal(rec)
	if err != nil {
		return nil
	}

	out := []app.DecodeRecord{{Time: at, Schema: SkimmerSchema, Text: text, Payload: payload, AudioHz: offset, Live: true}}

	if spot, ok := p.spot(offset, db, text); ok {
		if payload, err := json.Marshal(spot); err == nil {
			msg := spot.Callsign
			if spot.Country != "" {
				msg += " (" + spot.Country + ")"
			}

			out = append(out, app.DecodeRecord{Time: at, Schema: SkimmerSchema, Text: cleanRF(msg + ": " + spot.Message), Payload: payload, AudioHz: offset})
		}
	}

	return out
}

// QSO patterns (OpenWebRX+ owrx/skimmer.py): callsigns of at least four
// characters, three character ones being mostly decoding errors.
var (
	cqCall   = regexp.MustCompile(`^(.*CQ +([A-Z]{2,}) +([0-9A-Z]{4,})) .*$`)
	deCall   = regexp.MustCompile(`^(.*(?:DE|TEST|DX|CW|CWT|SST|MST|QRP|POTA|SOTA) +([0-9A-Z]{4,})) .*$`)
	tuCall   = regexp.MustCompile(`^(.*TU +([0-9A-Z]{4,}) +([0-9A-Z]{4,})) .*$`)
	twoDe    = regexp.MustCompile(`^(.* +([0-9A-Z]{4,}) +DE *([0-9A-Z]{4,})) .*$`)
	twoTimes = regexp.MustCompile(`^(.* +([0-9A-Z]{4,}) +([0-9A-Z]{4,})) .*$`)
)

// qsoPatterns are the QSO patterns in the order they are tried: the
// submatch of the callsign, of the station it calls (0: none) and whether
// both are the same callsign repeated ("<call> <call>").
var qsoPatterns = []struct {
	re           *regexp.Regexp
	call, callee int
	repeated     bool
}{
	{re: twoDe, call: 3, callee: 2},
	{re: tuCall, call: 3, callee: 2},
	{re: twoTimes, call: 2, repeated: true},
	{re: cqCall, call: 3},
	{re: deCall, call: 2},
}

// spot appends text to the rolling text of a frequency and looks for a
// callsign: "<callee> DE <call>", "TU <callee> <call>", "<call> <call>",
// "CQ <…> <call>", "DE|TEST|DX|… <call>", in that order. A callsign is
// accepted when it is in the country table; the frequency's text is then
// cleared, otherwise its last characters are kept.
func (p *skimmerParser) spot(offset int64, db int, text string) (SkimmerRecord, bool) {
	all := p.text[offset] + text

	for _, q := range qsoPatterns {
		m := q.re.FindStringSubmatch(all)

		switch {
		case m == nil:
			continue
		case q.repeated && (m[2] != m[3] || strings.Contains(m[2], "NN")):
			continue
		case q.callee > 0 && m[q.callee] == m[q.call]:
			continue
		}

		if q.callee > 0 {
			if _, ok := callsignCountry(m[q.callee]); !ok {
				continue
			}
		}

		country, ok := callsignCountry(m[q.call])
		if !ok {
			continue
		}

		r := SkimmerRecord{
			Kind: "spot", Mode: p.mode, OffsetHz: offset, DB: db, Callsign: m[q.call], Message: strings.TrimSpace(m[1]),
			CountryCode: country[0], Country: country[1],
		}

		if q.callee > 0 {
			r.Callee = m[q.callee]
		}

		delete(p.text, offset)

		return r, true
	}

	p.text[offset] = lastChars(all, skimmerKeep)

	return SkimmerRecord{}, false
}

// lastChars keeps the last n bytes of s, on a rune boundary.
func lastChars(s string, n int) string {
	if len(s) <= n {
		return s
	}

	s = s[len(s)-n:]
	for len(s) > 0 && s[0]&0xC0 == 0x80 {
		s = s[1:]
	}

	return s
}

// callsignPattern is a prefix, a digit and a suffix ending with a letter.
var callsignPattern = regexp.MustCompile(`^[0-9A-Z]*[A-Z]+[0-9][0-9A-Z]*[A-Z]$`)

// callsignCountry returns the country of a callsign: the longest prefix of
// the country table (DEC-015). ok is false for a string that is not a
// callsign or has no known prefix.
func callsignCountry(call string) ([2]string, bool) {
	call = strings.ToUpper(call)
	if !callsignPattern.MatchString(call) {
		return [2]string{}, false
	}

	for n := min(4, len(call)); n > 0; n-- {
		if c, ok := callsignCountries[call[:n]]; ok {
			return c, true
		}
	}

	return [2]string{}, false
}
