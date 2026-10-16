package decoder

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
)

// The native marine text decoders: NAVTEX (MAR-002) prints the SITOR-B
// messages from their ZCZC header line to NNNN, one record per line like
// SITOR-B, each tagged with its message header; DSC (MAR-003) writes one
// JSON line per call (libcsdr++ DscDecoder), parsed into a dsc.v1 record.

// NAVTEXSchema names the navtex.v1 payload.
const NAVTEXSchema = "navtex.v1"

// NAVTEXRecord is the navtex.v1 payload: a line of a NAVTEX message and the
// header of that message (ZCZC B1B2B3B4).
type NAVTEXRecord struct {
	Text string `json:"text"`
	// Station is the transmitter identity (B1, A to Z), Subject the
	// subject indicator (B2) and Serial the message number (B3B4, 00 to
	// 99): empty before a header.
	Station      string `json:"station,omitempty"`
	Subject      string `json:"subject,omitempty"`
	SubjectLabel string `json:"subject_label,omitempty"`
	Serial       string `json:"serial,omitempty"`
}

// navtexHeader is the header line of a NAVTEX message.
var navtexHeader = regexp.MustCompile(`^ZCZC ([A-Z])([A-Z])([0-9]{2})$`)

// navtexEnd is the end line of a NAVTEX message.
const navtexEnd = "NNNN"

// navtexSubjects are the subject indicators (B2) of IMO's NAVTEX manual.
var navtexSubjects = map[string]string{
	"A": "Navigational warning", "B": "Meteorological warning", "C": "Ice report",
	"D": "Search and rescue information, piracy warning", "E": "Meteorological forecast", "F": "Pilot and VTS service message",
	"G": "AIS message", "H": "LORAN message", "J": "SATNAV message", "K": "Other electronic navigational aid message",
	"L": "Navigational warning (additional to A)", "T": "Test transmission", "V": "Special service", "W": "Special service",
	"X": "Special service", "Y": "Special service", "Z": "No message on hand",
}

// navtexTagger tags the lines of a NAVTEX session with the header of their
// message: a header line starts a message, NNNN ends it.
type navtexTagger struct {
	emit func(app.DecodeRecord)
	cur  NAVTEXRecord
}

func (n *navtexTagger) record(rec app.DecodeRecord) {
	if !rec.Partial {
		if m := navtexHeader.FindStringSubmatch(rec.Text); m != nil {
			n.cur = NAVTEXRecord{Station: m[1], Subject: m[2], SubjectLabel: navtexSubjects[m[2]], Serial: m[3]}
		}
	}

	p := n.cur
	p.Text = rec.Text

	if payload, err := json.Marshal(p); err == nil {
		rec.Schema, rec.Payload = NAVTEXSchema, payload
	}

	if !rec.Partial && rec.Text == navtexEnd {
		n.cur = NAVTEXRecord{}
	}

	n.emit(rec)
}

// DSCSchema names the dsc.v1 payload.
const DSCSchema = "dsc.v1"

// DSCFormatError is the format of the JSON lines of a run of symbols that
// is not a DSC call (the symbols are in Data).
const DSCFormatError = "error"

// DSCRecord is the dsc.v1 payload: the fields of the libcsdr++ JSON line
// (names kept), with the source's kind and country and the position.
type DSCRecord struct {
	// Format is distress, allships, groupcall, selcall, areacall, autocall
	// or error.
	Format string `json:"format"`
	// Source is the caller's MMSI ("-" for a digit received in error).
	Source     string `json:"src,omitempty"`
	SourceKind string `json:"src_kind,omitempty"`
	Country    string `json:"country,omitempty"`
	// Destination is the called MMSI, or the area of an area call.
	Destination string `json:"dst,omitempty"`
	// DistressID is the MMSI in distress of a distress relay.
	DistressID string `json:"id,omitempty"`
	// Location is the position as received ("48.500N4.250W", "???" when
	// unknown); Lat and Lon are set when it is known.
	Location string   `json:"loc,omitempty"`
	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	// Time is the UTC time of the position (HHMM, "???" when unknown).
	Time string `json:"time,omitempty"`
	// RxFreq and TxFreq are the proposed working frequencies, in Hz or as
	// a channel ("CH16").
	RxFreq   string `json:"rxfreq,omitempty"`
	TxFreq   string `json:"txfreq,omitempty"`
	Number   string `json:"num,omitempty"`
	Category string `json:"category,omitempty"`
	Distress string `json:"distress,omitempty"`
	Next     *int   `json:"next,omitempty"`
	Cmd1     *int   `json:"cmd1,omitempty"`
	Cmd2     *int   `json:"cmd2,omitempty"`
	// EOS is the end of sequence: arq (acknowledgement requested), abq
	// (acknowledgement) or done.
	EOS string `json:"eos,omitempty"`
	// ECC: the error check character matches; nil for an error line.
	ECC *bool `json:"ecc,omitempty"`
	// Data are the symbols of an error line.
	Data string `json:"data,omitempty"`
}

// dscLocation is a DSC position: degrees and minutes as decimal degrees.
var dscLocation = regexp.MustCompile(`^([0-9]{1,2}\.[0-9]{3})([NS])([0-9]{1,3}\.[0-9]{3})([EW])$`)

// parseDSC turns a JSON line of the DSC decoder into a dsc.v1 record. An
// error line is kept only with showErrors (dsc_show_errors).
func parseDSC(line string, at time.Time, showErrors bool) (app.DecodeRecord, bool) {
	var p DSCRecord
	if err := json.Unmarshal([]byte(line), &p); err != nil || p.Format == "" {
		return app.DecodeRecord{}, false
	}

	// The tool's own time stamp is dropped: the record has the node's.
	if p.Format == DSCFormatError && !showErrors {
		return app.DecodeRecord{}, false
	}

	if info, ok := mmsiInfo(p.Source); ok {
		p.SourceKind, p.Country = info.Kind, info.Country
	}

	if m := dscLocation.FindStringSubmatch(p.Location); m != nil {
		lat, err1 := strconv.ParseFloat(m[1], 64)
		lon, err2 := strconv.ParseFloat(m[3], 64)

		if err1 == nil && err2 == nil && lat <= 90 && lon <= 180 {
			if m[2] == "S" {
				lat = -lat
			}

			if m[4] == "W" {
				lon = -lon
			}

			p.Lat, p.Lon = &lat, &lon
		}
	}

	payload, err := json.Marshal(p)
	if err != nil {
		return app.DecodeRecord{}, false
	}

	return app.DecodeRecord{Time: at, Schema: DSCSchema, Text: cleanRF(dscText(p)), Payload: payload}, true
}

// dscText is the text of a DSC record: "selcall routine from 227006760
// (France) to 002275300 rx 8414.0 kHz tx 8414.0 kHz arq".
func dscText(p DSCRecord) string {
	if p.Format == DSCFormatError {
		return "error " + p.Data
	}

	parts := []string{p.Format}

	add := func(s ...string) {
		for _, v := range s {
			if v != "" {
				parts = append(parts, v)
			}
		}
	}

	add(p.Category)

	if p.Source != "" {
		from := "from " + p.Source
		if p.Country != "" {
			from += " (" + p.Country + ")"
		}

		add(from)
	}

	if p.Destination != "" {
		add("to " + p.Destination)
	}

	if p.DistressID != "" {
		add("distress " + p.DistressID)
	}

	add(p.Distress)

	if p.Location != "" {
		add("at " + p.Location)
	}

	if p.Time != "" {
		add(p.Time + " UTC")
	}

	if p.RxFreq != "" {
		add("rx " + dscFreq(p.RxFreq))
	}

	if p.TxFreq != "" {
		add("tx " + dscFreq(p.TxFreq))
	}

	if p.Number != "" {
		add("tel " + p.Number)
	}

	add(p.EOS)

	if p.ECC != nil && !*p.ECC {
		add("ECC error")
	}

	return strings.Join(parts, " ")
}

// dscFreq shows a frequency in Hz in kHz; a channel is kept.
func dscFreq(f string) string {
	hz, err := strconv.ParseInt(f, 10, 64)
	if err != nil {
		return f
	}

	return fmt.Sprintf("%.1f kHz", float64(hz)/1000)
}

// maxDSCLine bounds a JSON line of the DSC decoder (about 400 bytes).
const maxDSCLine = 1024

// dscLines cuts the output of a DSC session into JSON lines, each parsed
// into a record; it implements textSink.
type dscLines struct {
	emit       func(app.DecodeRecord)
	showErrors bool

	line  []byte
	start time.Time
	over  bool
}

func (d *dscLines) feed(text []byte, at time.Time) {
	for _, b := range text {
		if b == '\n' {
			if !d.over {
				if rec, ok := parseDSC(string(d.line), d.start, d.showErrors); ok {
					d.emit(rec)
				}
			}

			d.line, d.over = d.line[:0], false

			continue
		}

		if len(d.line) == 0 {
			d.start = at
		}

		if len(d.line) >= maxDSCLine {
			d.over = true

			continue
		}

		d.line = append(d.line, b)
	}
}

// idle implements textSink: a call is written at once.
func (d *dscLines) idle() {}

// flush implements textSink: lost input drops the line being written.
func (d *dscLines) flush() { d.line, d.over = d.line[:0], false }
