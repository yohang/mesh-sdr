package decoder

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/radio/app"
)

// The paging decoder (DEC-033): multimon-ng decodes FLEX and POCSAG
// 512/1200/2400 from FM audio at 22 050 Hz. Long FLEX messages arrive in
// fragments that are joined per capcode in a capped buffer. The
// paging_filter setting keeps only readable messages; paging_charset is
// the POCSAG charset of multimon-ng.

// PagingSchema names the paging.v1 payload.
const PagingSchema = "paging.v1"

// FLEX reassembly bounds: capcodes with a pending message, bytes per
// message.
const (
	maxFlexPending = 256
	maxFlexBytes   = MaxText
)

// pagingArgs runs multimon-ng with FLEX and the three POCSAG rates.
func pagingArgs(c sessionConfig) []string {
	cs := c.settings.PagingCharset
	if !slices.Contains(ctl.PagingCharsets, cs) {
		cs = "US"
	}

	return []string{"-c", "-v", "0", "-C", cs, "-t", "raw", "-a", "FLEX", "-a", "POCSAG512", "-a", "POCSAG1200", "-a", "POCSAG2400", "-"}
}

// PagingRecord is the paging.v1 payload.
type PagingRecord struct {
	// Protocol is POCSAG or FLEX.
	Protocol string `json:"protocol"`
	Baud     int    `json:"baud"`
	// Address is the POCSAG address or the FLEX capcode(s).
	Address string `json:"address"`
	// Function is the POCSAG function bits (0 to 3).
	Function *int `json:"function,omitempty"`
	// Type is Alpha, Numeric, Skyper or none (POCSAG); ALN, NUM, TON or
	// UNK (FLEX).
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
	// FLEX: the phase, cycle and frame of the message.
	Phase string `json:"phase,omitempty"`
	Frame string `json:"frame,omitempty"`
}

// multimon-ng 1.3.1 output lines (pocsag.c, demod_flex.c):
//
//	POCSAG1200: Address: 1234567  Function: 3  Alpha:   message
//	POCSAG512: Address:  123456  Function: 0
//	FLEX|2026-10-08 12:00:00|1600/2/K/A|07.104|001234567|ALN|message
//	FLEX: 2026-10-08 12:00:00 1600/2/A 07.104 [001234567] NUM 0123
var (
	pocsagLine  = regexp.MustCompile(`^POCSAG(512|1200|2400):\s+Address:\s+(\d+|-)\s+Function:\s+(\d|-)(?:\s+Certainty:\s+-?\d+)?(?:\s+(Alpha|Numeric|Skyper):\s?(.*))?\s*$`)
	flexALNLine = regexp.MustCompile(`^FLEX\|\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\|(\d+)/\d+/([KCF?])/([A-D])\|(\d\d\.\d{3})\|(\d{9}(?: \d{9})*)\|ALN\|(.*)$`)
	flexLine    = regexp.MustCompile(`^FLEX: \d{4}-\d\d-\d\d \d\d:\d\d:\d\d (\d+)/\d+/([A-D]) (\d\d\.\d{3}) \[(\d{9})\] (NUM|TON|UNK) ?(.*)$`)
	// pocsagControl are the <NUL>, <ETX>… names of control characters.
	pocsagControl = regexp.MustCompile(`<[A-Za-z0-9]{2,3}>`)
	spaces        = regexp.MustCompile(`\s+`)
)

// pagingParser is the parser of a paging session: its FLEX fragments.
type pagingParser struct {
	filter bool
	flex   map[string]string
	order  []string
}

func newPagingParser(c sessionConfig) lineParser {
	p := &pagingParser{filter: c.settings.PagingFilter, flex: map[string]string{}}

	return p.line
}

func (p *pagingParser) line(line string, at time.Time) []app.DecodeRecord {
	var (
		r  PagingRecord
		ok bool
	)

	switch {
	case strings.HasPrefix(line, "POCSAG"):
		r, ok = p.pocsag(line)
	case strings.HasPrefix(line, "FLEX"):
		r, ok = p.flexLine(line)
	}

	if !ok {
		return nil
	}

	payload, err := json.Marshal(r)
	if err != nil {
		return nil
	}

	text := r.Protocol + strconv.Itoa(r.Baud) + " " + r.Address
	if r.Function != nil {
		text += "/" + strconv.Itoa(*r.Function)
	}

	if r.Type != "" {
		text += " " + r.Type
	}

	if r.Message != "" {
		text += ": " + r.Message
	}

	return []app.DecodeRecord{{Time: at, Schema: PagingSchema, Text: cleanRF(text), Payload: payload}}
}

// collapse removes control characters and collapses white space.
func collapse(s string) string {
	return strings.TrimSpace(spaces.ReplaceAllString(cleanRF(strings.Map(func(r rune) rune {
		if r < ' ' {
			return ' '
		}

		return r
	}, s)), " "))
}

// readable: the average word length of a readable message is small
// (OpenWebRX+'s heuristic).
func readable(msg string) bool {
	sp := strings.Count(msg, " ")
	letters := len(msg) - sp

	return letters > 0 && letters/(sp+1) < 40
}

func (p *pagingParser) pocsag(line string) (PagingRecord, bool) {
	m := pocsagLine.FindStringSubmatch(line)
	if m == nil {
		return PagingRecord{}, false
	}

	baud, _ := strconv.Atoi(m[1])
	r := PagingRecord{Protocol: "POCSAG", Baud: baud, Address: m[2], Type: m[4]}

	if f, err := strconv.Atoi(m[3]); err == nil {
		r.Function = &f
	}

	r.Message = collapse(pocsagControl.ReplaceAllString(m[5], " "))

	if p.filter && (r.Type != "Alpha" || r.Message == "") {
		return PagingRecord{}, false
	}

	return r, true
}

func (p *pagingParser) flexLine(line string) (PagingRecord, bool) {
	if m := flexALNLine.FindStringSubmatch(line); m != nil {
		baud, _ := strconv.Atoi(m[1])
		r := PagingRecord{Protocol: "FLEX", Baud: baud, Address: m[5], Type: "ALN", Phase: m[3], Frame: m[4]}

		msg, complete := p.assemble(m[5], m[2], m[6])
		if !complete {
			return PagingRecord{}, false
		}

		r.Message = collapse(msg)

		if p.filter && !readable(r.Message) {
			return PagingRecord{}, false
		}

		return r, true
	}

	m := flexLine.FindStringSubmatch(line)
	if m == nil || p.filter {
		return PagingRecord{}, false
	}

	baud, _ := strconv.Atoi(m[1])

	return PagingRecord{Protocol: "FLEX", Baud: baud, Address: m[4], Type: m[5], Phase: m[2], Frame: m[3], Message: collapse(m[6])}, true
}

// assemble joins the FLEX fragments of a capcode: F is a fragment with more
// to come, C the last one, K a complete message. It reports whether a
// message is complete.
func (p *pagingParser) assemble(capcode, flag, text string) (string, bool) {
	prev, pending := p.flex[capcode]

	switch flag {
	case "F", "C":
		if !pending {
			if len(p.order) >= maxFlexPending {
				delete(p.flex, p.order[0])
				p.order = p.order[1:]
			}

			p.order = append(p.order, capcode)
		}

		msg := capBytes(prev+text, maxFlexBytes)
		if flag == "F" {
			p.flex[capcode] = msg

			return "", false
		}

		p.drop(capcode)

		return msg, true
	default:
		if pending {
			p.drop(capcode)
		}

		return text, true
	}
}

func (p *pagingParser) drop(capcode string) {
	delete(p.flex, capcode)

	if i := slices.Index(p.order, capcode); i >= 0 {
		p.order = slices.Delete(p.order, i, i+1)
	}
}
