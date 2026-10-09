package decoder

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
)

// The EAS decoder (DEC-036): multimon-ng decodes the SAME headers of
// Emergency Alert System messages from FM audio at 22 050 Hz (it prints a
// header once two of its three bursts agree). The record carries the raw
// header and its decoded text: originator, event, locations, start and end
// (UTC) and the sending station.

// EASSchema names the eas.v1 payload.
const EASSchema = "eas.v1"

// easArgs runs multimon-ng with the EAS decoder.
func easArgs(sessionConfig) []string { return multimon(nil, "EAS") }

// EASRecord is the eas.v1 payload.
type EASRecord struct {
	// Raw is the SAME header.
	Raw        string    `json:"raw"`
	Originator string    `json:"originator"`
	OrigName   string    `json:"originator_name,omitempty"`
	Event      string    `json:"event"`
	EventName  string    `json:"event_name,omitempty"`
	Areas      []EASArea `json:"areas"`
	// Purge is the validity (HHMM) from Start.
	Purge   string     `json:"purge"`
	Start   *time.Time `json:"start,omitempty"`
	End     *time.Time `json:"end,omitempty"`
	Station string     `json:"station"`
}

// EASArea is one location code (PSSCCC) and its name, when known.
type EASArea struct {
	Code string `json:"code"`
	Name string `json:"name,omitempty"`
}

// sameHeader is ZCZC-ORG-EEE-PSSCCC(-PSSCCC)*+TTTT-JJJHHMM-LLLLLLLL-.
var sameHeader = regexp.MustCompile(`^ZCZC-([A-Z]{3})-([A-Z0-9]{3})-((?:\d{6}-?){1,31})\+(\d{4})-(\d{7})-([A-Z0-9/ ]{1,8})-?$`)

// parseEAS turns an "EAS: ZCZC-…" line into an eas.v1 record.
func parseEAS(line string, at time.Time) (app.DecodeRecord, bool) {
	raw, ok := strings.CutPrefix(line, "EAS:")
	if !ok {
		return app.DecodeRecord{}, false
	}

	raw = strings.TrimSpace(raw)

	m := sameHeader.FindStringSubmatch(raw)
	if m == nil {
		return app.DecodeRecord{}, false
	}

	r := EASRecord{
		Raw: raw, Originator: m[1], OrigName: sameOriginators[m[1]], Event: m[2], EventName: sameEvent(m[2]),
		Purge: m[4], Station: strings.TrimSpace(m[6]), Areas: []EASArea{},
	}

	for c := range strings.SplitSeq(strings.Trim(m[3], "-"), "-") {
		r.Areas = append(r.Areas, EASArea{Code: c, Name: sameArea(c)})
	}

	if start, ok := sameTime(m[5], at); ok {
		hh, _ := strconv.Atoi(m[4][:2])
		mm, _ := strconv.Atoi(m[4][2:])
		end := start.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
		r.Start, r.End = &start, &end
	}

	payload, err := json.Marshal(r)
	if err != nil {
		return app.DecodeRecord{}, false
	}

	return app.DecodeRecord{Time: at, Schema: EASSchema, Text: cleanRF(raw + "\n" + easText(r)), Payload: payload}, true
}

// easText is the decoded header: "Tornado Warning from the National
// Weather Service for Clay, Missouri; Jackson, Missouri, 17:00 to 17:30
// UTC (KEAX/NWS)".
func easText(r EASRecord) string {
	var b strings.Builder

	b.WriteString(r.EventName)

	if r.OrigName != "" {
		b.WriteString(" from " + r.OrigName)
	}

	names := make([]string, 0, len(r.Areas))
	for _, a := range r.Areas {
		if a.Name != "" {
			names = append(names, a.Name)
		} else {
			names = append(names, a.Code)
		}
	}

	b.WriteString(" for " + strings.Join(names, "; "))

	if r.Start != nil {
		b.WriteString(", " + r.Start.Format("2006-01-02 15:04") + " to " + r.End.Format("2006-01-02 15:04") + " UTC")
	}

	b.WriteString(" (" + r.Station + ")")

	return b.String()
}

// sameEvent names an event code, or its kind for an unknown one.
func sameEvent(code string) string {
	if n, ok := sameEvents[code]; ok {
		return n
	}

	if k, ok := sameEventKinds[code[2]]; ok {
		return "Unknown " + k
	}

	return "Unknown event " + code
}

// sameArea names a PSSCCC code: its US county and state ("" when unknown).
func sameArea(code string) string {
	county, ok := sameCounties[code[1:]]
	if !ok {
		return ""
	}

	if strings.HasPrefix(county, "All of ") {
		return county
	}

	state := sameStates[code[1:3]]
	if state == "" {
		return county
	}

	return county + ", " + state
}

// sameTime resolves JJJHHMM (day of year, UTC) to the year nearest to the
// reception time.
func sameTime(s string, at time.Time) (time.Time, bool) {
	day, _ := strconv.Atoi(s[:3])
	hh, _ := strconv.Atoi(s[3:5])
	mm, _ := strconv.Atoi(s[5:7])

	if day < 1 || day > 366 || hh > 23 || mm > 59 {
		return time.Time{}, false
	}

	at = at.UTC()

	var (
		best  time.Time
		found bool
	)

	for y := at.Year() - 1; y <= at.Year()+1; y++ {
		t := time.Date(y, 1, day, hh, mm, 0, 0, time.UTC)
		if t.Year() != y {
			continue
		}

		if !found || t.Sub(at).Abs() < best.Sub(at).Abs() {
			best, found = t, true
		}
	}

	return best, found
}
