package decoder

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// Settings are the decoding settings of the slot decoders (DEC-024,
// DEC-021…023, DEC-029) and of the streaming tool decoders (paging, ISM),
// pushed by the hub in the desired state (Admin › Decoding). A zero field
// takes the default of the hub settings.
type Settings struct {
	// WSJTDepth is wsjt_decoding_depth (1 to 3, default 3).
	WSJTDepth int
	// WSJTDepths are the per-mode depths wsjt_decoding_depths[mode]; a
	// missing or zero entry takes WSJTDepth (JT65 defaults to 1).
	WSJTDepths map[string]int
	// FST4Intervals and FST4WIntervals are the enabled T/R periods in
	// seconds (fst4_enabled_intervals, fst4w_enabled_intervals).
	FST4Intervals, FST4WIntervals []int
	// Q65Combinations are the enabled submode and period combinations
	// ("A30", q65_enabled_combinations).
	Q65Combinations []string
	// JS8Profiles are the enabled JS8 speeds (normal, slow, fast, turbo).
	JS8Profiles []string
	// JS8Depth is js8_decoding_depth (1 to 3, default 3).
	JS8Depth int
	// PagingFilter keeps only the readable pages (DEC-033).
	PagingFilter bool
	// PagingCharset is the POCSAG charset of multimon-ng (US, FR, DE, DK,
	// SE or SI); "" is US.
	PagingCharset string
	// ISMReportLevels keeps the signal levels of rtl_433 in the ISM
	// decodes (DEC-039).
	ISMReportLevels bool
}

// Default decoding settings (the hub defaults, ADR 0028).
var (
	DefaultFST4Intervals   = []int{15, 30}
	DefaultFST4WIntervals  = []int{120, 300}
	DefaultQ65Combinations = []string{"A30", "E120", "C60"}
	DefaultJS8Profiles     = []string{"normal", "slow"}
)

// Valid slot periods (WSJT-X 2.7).
var (
	FST4Periods  = []int{15, 30, 60, 120, 300, 900, 1800}
	FST4WPeriods = []int{120, 300, 900, 1800}
)

// q65Periods are the Q65 T/R periods with the occupied bandwidth (Hz) of
// submode A (Q65 Quick Start Guide); submode B to E double it each time.
var q65Periods = []struct {
	seconds, bandwidth int
}{{15, 433}, {30, 217}, {60, 108}, {120, 49}, {300, 19}}

// Q65Combinations returns the valid Q65 combinations, submode then period
// ("A15" … "E300"): those whose occupied bandwidth is below 2700 Hz
// (DEC-023).
func Q65Combinations() []string {
	var out []string

	for _, p := range q65Periods {
		for i, sub := range "ABCDE" {
			if p.bandwidth<<i < 2700 {
				out = append(out, string(sub)+strconv.Itoa(p.seconds))
			}
		}
	}

	return out
}

// JS8 speeds: their slot period and js8 submode.
var js8Speeds = map[string]struct {
	period  time.Duration
	submode string
}{
	"normal": {15 * time.Second, "A"},
	"fast":   {10 * time.Second, "B"},
	"turbo":  {6 * time.Second, "C"},
	"slow":   {30 * time.Second, "E"},
}

// JS8Speeds are the JS8 speeds, in display order.
var JS8Speeds = []string{"normal", "slow", "fast", "turbo"}

// depth returns the decoding depth of a WSJT mode.
func (s Settings) depth(mode string) int {
	if d := s.WSJTDepths[mode]; d > 0 {
		return d
	}

	if s.WSJTDepth > 0 {
		return s.WSJTDepth
	}

	return 3
}

// profile is one slot decoder of a mode (DEC-026): its slot period, its
// tool and argv for a slot file and a private job directory, its job
// deadline after the slot end (§8.4) and its output parser. shared: the
// tool runs in the session workdir without a job directory (wsprd keeps
// its hash table there).
type profile struct {
	mode    string
	period  time.Duration
	submode string
	tool    string
	shared  bool
	args    func(file, dir string) []string
	job     time.Duration
	parse   func(p profile, line string, partial bool) (app.DecodeRecord, bool)
}

// profiles returns the slot decoders of a mode under the settings: one for
// most modes, one per enabled period or speed for FST4, FST4W, Q65 and
// JS8 (read at each slot: a settings change applies live, DEC-021).
func profiles(mode string, s Settings) []profile {
	depth := strconv.Itoa(s.depth(mode))

	jt9 := func(flag string, period time.Duration, job time.Duration, extra ...string) profile {
		return profile{
			mode: mode, period: period, tool: "jt9", job: job, parse: parseJT9,
			// -a and -t: the data and temporary files (wisdom, timers)
			// go to the job directory.
			args: func(file, dir string) []string {
				return append(append([]string{flag}, extra...), "-a", dir, "-t", dir, "-d", depth, file)
			},
		}
	}

	switch mode {
	case "ft8":
		return []profile{jt9("--ft8", 15*time.Second, 13*time.Second)}
	case "ft4":
		return []profile{jt9("--ft4", 7500*time.Millisecond, 6*time.Second)}
	case "jt65":
		return []profile{jt9("--jt65", time.Minute, 50*time.Second)}
	case "jt9":
		return []profile{jt9("--jt9", time.Minute, 50*time.Second)}
	case "wspr":
		return []profile{{
			mode: mode, period: 2 * time.Minute, tool: "wsprd", shared: true, job: 100 * time.Second, parse: parseWSPR,
			args: func(file, _ string) []string {
				if s.depth(mode) > 1 {
					return []string{"-d", file}
				}

				return []string{file}
			},
		}}
	case "fst4", "fst4w":
		enabled, valid := orDefault(s.FST4Intervals, DefaultFST4Intervals), FST4Periods
		if mode == "fst4w" {
			enabled, valid = orDefault(s.FST4WIntervals, DefaultFST4WIntervals), FST4WPeriods
		}

		var out []profile

		for _, sec := range valid {
			if slices.Contains(enabled, sec) {
				period := time.Duration(sec) * time.Second
				out = append(out, jt9("--"+mode, period, period*8/10, "-p", strconv.Itoa(sec)))
			}
		}

		return out
	case "q65":
		enabled := orDefault(s.Q65Combinations, DefaultQ65Combinations)

		var out []profile

		for _, c := range Q65Combinations() {
			if slices.Contains(enabled, c) {
				sec, _ := strconv.Atoi(c[1:])
				period := time.Duration(sec) * time.Second
				p := jt9("--q65", period, period*8/10, "-p", c[1:], "-b", c[:1])
				p.submode = c[:1]
				out = append(out, p)
			}
		}

		return out
	case "js8":
		return js8Profiles(s)
	}

	return nil
}

func orDefault[T any](v, def []T) []T {
	if len(v) == 0 {
		return def
	}

	return v
}

// wsjtRules classify the stderr of jt9, wsprd and js8.
var wsjtRules = []process.Rule{
	{Pattern: regexp.MustCompile(`(?i)(unrecognized|unknown|invalid) (option|argument)|usage:`), Class: process.ClassFatalConfig},
	{Pattern: regexp.MustCompile(`(?i)(cannot|can't|unable to|failed to) open|no such file`), Class: process.ClassInputError},
	{Pattern: regexp.MustCompile(`.`), Class: process.ClassInfo},
}

// WSJTSchema names the wsjt.v1 payload.
const WSJTSchema = "wsjt.v1"

// WSJTRecord is the wsjt.v1 payload (DEC-027): the fixed columns of a
// decode and what the message parse found. Calls and locators stay here
// until the map (M3) and the reporters (M4, ADR 0028).
type WSJTRecord struct {
	Mode string `json:"mode"`
	// Period is the slot period in seconds.
	Period float64 `json:"period"`
	// Submode is the Q65 submode (A to E).
	Submode string  `json:"submode,omitempty"`
	DB      int     `json:"db"`
	DT      float64 `json:"dt"`
	// DF is the audio frequency of the signal (Hz above the dial).
	DF  int64  `json:"df"`
	Msg string `json:"msg"`
	// Callsign is the sender, Locator its grid square, Callee the station
	// it answers (QSO messages); WSPR and FST4W also give the power (dBm)
	// and WSPR the drift (Hz).
	Callsign string `json:"callsign,omitempty"`
	Locator  string `json:"locator,omitempty"`
	Callee   string `json:"callee,omitempty"`
	DBm      *int   `json:"dbm,omitempty"`
	Drift    *int   `json:"drift,omitempty"`
	// PartialSlot: the slot lost more than 10 % of its audio (DEC-026).
	PartialSlot bool `json:"partial_slot,omitempty"`
}

// noise are the lines of the tools that are not decodes.
var noise = regexp.MustCompile(`^\s*(<Decode(Started|Debug|Finished)>|EOF on input file)`)

// splitTime cuts the time of day a decoder prints first (HHMM, HHMMSS or
// "****" for long FST4 periods).
func splitTime(line string) (string, bool) {
	i := strings.IndexByte(line, ' ')
	if i < 4 || i > 6 {
		return "", false
	}

	return line[i+1:], true
}

// field returns s[from:to] trimmed, "" when s is shorter.
func field(s string, from, to int) string {
	if from >= len(s) {
		return ""
	}

	return strings.TrimSpace(s[from:min(to, len(s))])
}

// parseJT9 parses a fixed-column jt9 decode (DEC-027):
//
//	120015 -12  0.0 1200 ~  CQ DL1ABC JO62
//	1201 -10  0.0 1500 `  CQ K1ABC FN42
//
// dB, DT, audio frequency, mode character, message (columns 17 to 53 after
// the time, which leaves out the a1/q3 annotations). Q65 lines without a
// message are dropped (DEC-023).
func parseJT9(p profile, line string, partial bool) (app.DecodeRecord, bool) {
	if noise.MatchString(line) {
		return app.DecodeRecord{}, false
	}

	rest, ok := splitTime(line)
	if !ok || len(rest) < 18 {
		return app.DecodeRecord{}, false
	}

	db, err1 := strconv.Atoi(field(rest, 0, 3))
	dt, err2 := strconv.ParseFloat(field(rest, 4, 8), 64)
	df, err3 := strconv.ParseInt(field(rest, 9, 13), 10, 64)
	msg := field(rest, 17, 53)

	if err1 != nil || err2 != nil || err3 != nil || msg == "" {
		return app.DecodeRecord{}, false
	}

	rec := WSJTRecord{
		Mode: strings.ToUpper(p.mode), Period: p.period.Seconds(), Submode: p.submode, DB: db, DT: dt, DF: df, Msg: msg, PartialSlot: partial,
	}

	if p.mode == "fst4w" {
		parseBeacon(&rec)
	} else {
		parseQSO(&rec)
	}

	return wsjtRecord(rec)
}

// parseWSPR parses a wsprd decode (DEC-020): time, dB, DT, frequency (MHz
// above the dial), drift and message.
//
//	1200 -20 -0.0   0.001500  0  K1ABC FN42 33
func parseWSPR(p profile, line string, partial bool) (app.DecodeRecord, bool) {
	if noise.MatchString(line) {
		return app.DecodeRecord{}, false
	}

	f := strings.Fields(line)
	if len(f) < 6 {
		return app.DecodeRecord{}, false
	}

	db, err1 := strconv.Atoi(f[1])
	dt, err2 := strconv.ParseFloat(f[2], 64)
	mhz, err3 := strconv.ParseFloat(f[3], 64)
	drift, err4 := strconv.Atoi(f[4])

	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return app.DecodeRecord{}, false
	}

	rec := WSJTRecord{
		Mode: "WSPR", Period: p.period.Seconds(), DB: db, DT: dt, DF: int64(mhz*1e6 + 0.5), Drift: &drift, Msg: strings.Join(f[5:], " "),
		PartialSlot: partial,
	}
	parseBeacon(&rec)

	return wsjtRecord(rec)
}

func wsjtRecord(rec WSJTRecord) (app.DecodeRecord, bool) {
	if rec.DT == 0 {
		rec.DT = 0 // the tools print -0.0
	}

	payload, err := json.Marshal(rec)
	if err != nil {
		return app.DecodeRecord{}, false
	}

	return app.DecodeRecord{Schema: WSJTSchema, Text: rec.Msg, Payload: payload, AudioHz: rec.DF}, true
}

var (
	// qsoLocator: "<…> CALL [R] GRID|73|RRR|RR73" (WSJT-X standard
	// messages).
	qsoLocator = regexp.MustCompile(`^(.*)\s([A-Z0-9/]{2,})(\sR)?\s(([A-R]{2}[0-9]{2})|73|RRR)$`)
	qsoCallee  = regexp.MustCompile(`^([A-Z0-9/]{2,})(\s.*)?$`)
	beacon     = regexp.MustCompile(`^([A-Z0-9/]*)\s([A-R]{2}[0-9]{2})\s([0-9]+)`)
)

// parseQSO finds the sender and its locator, or the sender and the station
// it answers, in a QSO message (DEC-027). RR73 is a valid grid square in
// the Arctic Ocean, but means "roger roger, 73" here.
func parseQSO(rec *WSJTRecord) {
	m := qsoLocator.FindStringSubmatch(rec.Msg)
	if m == nil {
		return
	}

	rec.Callsign = m[2]

	if m[4] != "RR73" && m[4] != "73" && m[4] != "RRR" {
		rec.Locator = m[4]

		return
	}

	if c := qsoCallee.FindStringSubmatch(m[1]); c != nil {
		rec.Callee = c[1]
	}
}

// parseBeacon reads a beacon message (WSPR, FST4W): call, locator, power
// in dBm.
func parseBeacon(rec *WSJTRecord) {
	m := beacon.FindStringSubmatch(rec.Msg)
	if m == nil {
		return
	}

	dbm, err := strconv.Atoi(m[3])
	if err != nil {
		return
	}

	rec.Callsign, rec.Locator, rec.DBm = m[1], m[2], &dbm
}
