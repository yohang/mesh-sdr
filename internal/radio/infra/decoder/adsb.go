package decoder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// The ADS-B decoder (AIR-004, ADR 0029): readsb (Debian trixie, a dump1090
// fork) reads the 2.4 MS/s wide IQ tap as SC16 on stdin and writes its
// aircraft list as JSON (--write-json) into a directory of the session's
// private workdir; the session polls it every second and reports each
// aircraft that sent messages since the last poll as an aircraft.v1
// record.
//
// readsb 3.14 quirks the session works around (verified with the pinned
// package):
//   - in ifile mode its clock is the sample count, but a periodic task
//     scheduled before the first full input buffer is scheduled on the wall
//     clock and never runs (no aircraft in aircraft.json, never a
//     stats.json): the session spawns it only once the input buffer holds a
//     few of its reads;
//   - stats.json is written at exit only, so the liveness rule (§8.4
//     idle_output, 30 s, restart after 3) reads aircraft.json, whose "now"
//     (the sample clock) advances while readsb consumes its input;
//   - without input for about 10 s it exits ("SDR wedged") with status 0:
//     the supervisor restarts it.

// AircraftSchema names the aircraft.v1 payload.
const AircraftSchema = "aircraft.v1"

// readsb session timings and paths.
const (
	// adsbDir is the --write-json directory, in the workdir.
	adsbDir = "adsb"
	// adsbPoll is the aircraft.json poll period (readsb writes it every
	// second, --write-json-every 1).
	adsbPoll = time.Second
	// adsbIdle is the liveness rule: aircraft.json's clock must advance
	// within it.
	adsbIdle = 30 * time.Second
	// adsbPrefill is the input buffered before each spawn: four reads of
	// readsb (256 KiB each).
	adsbPrefill = 1 << 20
	// adsbPrefillWait bounds the wait for it.
	adsbPrefillWait = 5 * time.Second
	// maxAircraftFile bounds the aircraft.json read.
	maxAircraftFile = 4 << 20
)

// readsbArgs runs readsb on SC16 IQ from stdin, with the receiver position
// when the node knows it (positions decoded relative to it, ranges).
func readsbArgs(c sessionConfig) []string {
	args := []string{
		"--device-type", "ifile", "--ifile", "-", "--iformat", "SC16", "--quiet",
		"--write-json", adsbDir, "--write-json-every", "1",
	}

	if c.position != nil {
		if lat, lon, ok := c.position(); ok && validLatLon(lat, lon) {
			args = append(args, "--lat", strconv.FormatFloat(lat, 'f', 5, 64), "--lon", strconv.FormatFloat(lon, 'f', 5, 64))
		}
	}

	return args
}

// readsbRules classify the stderr of readsb: argument errors are fatal,
// the wedge watchdog is a warning (the exit restarts it).
var readsbRules = []process.Rule{
	{Pattern: regexp.MustCompile(`(?i)unrecognized option|not understood|error parsing the given command line|unknown sdr specific option`), Class: process.ClassFatalConfig},
	{Pattern: regexp.MustCompile(`(?i)wedged|error reading input`), Class: process.ClassWarn},
	{Pattern: regexp.MustCompile(`.`), Class: process.ClassInfo},
}

// workdirPoll is a tool whose output is a file it rewrites in a directory
// of its workdir: the session polls it.
type workdirPoll struct {
	dir, file string
	every     time.Duration
	// idle is the liveness rule (Timeouts.IdleOutput).
	idle time.Duration
	// prefill is the input buffered before each spawn (bounded by
	// adsbPrefillWait).
	prefill int
	// parser returns the parser of one run's output.
	parser func(c sessionConfig) filePoller
}

// filePoller parses one read of the polled file at time at: the records
// and whether the tool made progress since the last read.
type filePoller func(data []byte, at time.Time) (recs []app.DecodeRecord, progress bool)

var readsbPoll = &workdirPoll{dir: adsbDir, file: "aircraft.json", every: adsbPoll, idle: adsbIdle, prefill: adsbPrefill, parser: newAircraftPoller}

// pollSpec sets up a polled tool: the output directory is created in the
// workdir; before each spawn the input is buffered (prefill) and the
// output of the previous run removed; a side channel polls the output
// while the tool runs, and the liveness rule applies (§8.4 idle_output,
// restart after 3 violations).
func (s *toolSession) pollSpec(ps *process.Spec, p *workdirPoll, cfg sessionConfig) {
	var path string

	ps.Prepare = func(wd string) error {
		path = filepath.Join(wd, p.dir, p.file)

		return os.Mkdir(filepath.Join(wd, p.dir), 0o700)
	}

	args := ps.Args
	ps.PerRun = func() (process.Run, error) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return process.Run{}, err
		}

		s.buf.Wait(p.prefill, adsbPrefillWait)

		parse := p.parser(cfg)

		return process.Run{Args: args, Attach: func(ctx context.Context, touch func()) error {
			return s.pollRun(ctx, p, path, parse, touch)
		}}, nil
	}

	ps.Timeouts.IdleOutput, ps.Timeouts.IdleStrikes = p.idle, 3
}

// pollRun is the side channel of one run: it reads the file every period
// and reports its records; progress marks the run alive.
func (s *toolSession) pollRun(ctx context.Context, p *workdirPoll, path string, parse filePoller, touch func()) error {
	t := time.NewTicker(p.every)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}

		data, err := readCapped(path, maxAircraftFile)

		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return err
		}

		recs, progress := parse(data, s.r.o.Now())
		if progress {
			touch()
		}

		for _, rec := range recs {
			s.decode(rec)
		}
	}
}

// readCapped reads a file of at most n bytes.
func readCapped(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, n+1))
	if err == nil && int64(len(data)) > n {
		err = errOutputTooLarge
	}

	return data, err
}

// errOutputTooLarge is a polled file beyond its bound: the run's side
// channel stops (the liveness rule then restarts the tool).
var errOutputTooLarge = errors.New("output file too large")

// readsbAircraft is the part of a readsb aircraft the session reads
// (readsb README-json.md). alt_baro is a number or "ground".
type readsbAircraft struct {
	Hex       string          `json:"hex"`
	Type      string          `json:"type"`
	Flight    string          `json:"flight"`
	AltBaro   json.RawMessage `json:"alt_baro"`
	AltGeom   *float64        `json:"alt_geom"`
	GS        *float64        `json:"gs"`
	Track     *float64        `json:"track"`
	BaroRate  *float64        `json:"baro_rate"`
	GeomRate  *float64        `json:"geom_rate"`
	Squawk    string          `json:"squawk"`
	Emergency string          `json:"emergency"`
	Category  string          `json:"category"`
	Lat       *float64        `json:"lat"`
	Lon       *float64        `json:"lon"`
	Messages  int64           `json:"messages"`
	Seen      float64         `json:"seen"`
	RSSI      *float64        `json:"rssi"`
}

// readsbList is aircraft.json.
type readsbList struct {
	Now      float64          `json:"now"`
	Aircraft []readsbAircraft `json:"aircraft"`
}

// AircraftRecord is the aircraft.v1 payload: the state readsb keeps of one
// aircraft. Units are the aviation ones (feet, knots, feet per minute).
type AircraftRecord struct {
	// ICAO is the 24-bit address in upper-case hex; a "~" prefix marks a
	// non-ICAO address (TIS-B).
	ICAO        string   `json:"icao"`
	AddressType string   `json:"address_type,omitempty"`
	Flight      string   `json:"flight,omitempty"`
	Squawk      string   `json:"squawk,omitempty"`
	Emergency   string   `json:"emergency,omitempty"`
	Category    string   `json:"category,omitempty"`
	Lat         *float64 `json:"lat,omitempty"`
	Lon         *float64 `json:"lon,omitempty"`
	// AltitudeFt is the barometric altitude; Ground: on the ground.
	AltitudeFt     *float64 `json:"altitude_ft,omitempty"`
	Ground         bool     `json:"ground,omitempty"`
	GeomAltitudeFt *float64 `json:"geom_altitude_ft,omitempty"`
	SpeedKt        *float64 `json:"speed_kt,omitempty"`
	TrackDeg       *float64 `json:"track_deg,omitempty"`
	VerticalRate   *float64 `json:"vertical_rate_fpm,omitempty"`
	RSSI           *float64 `json:"rssi_dbfs,omitempty"`
	Messages       int64    `json:"messages"`
}

// Validation of the readsb values (untrusted tool output).
var (
	icaoHex      = regexp.MustCompile(`^~?[0-9a-f]{6}$`)
	flightID     = regexp.MustCompile(`^[A-Z0-9]{1,8}$`)
	squawkCode   = regexp.MustCompile(`^[0-7]{4}$`)
	categoryCode = regexp.MustCompile(`^[A-D][0-7]$`)
	addressType  = regexp.MustCompile(`^[a-z_]{1,16}$`)
	emergencies  = []string{"general", "lifeguard", "minfuel", "nordo", "unlawful", "downed", "reserved"}
)

// newAircraftPoller returns the parser of one readsb run: an aircraft is
// reported when its message count grew since the last read (readsb keeps
// each aircraft's state, so each record is its full state). The clock of
// aircraft.json advancing is the run's progress.
func newAircraftPoller(sessionConfig) filePoller {
	var (
		clock float64
		seen  = map[string]int64{}
	)

	return func(data []byte, at time.Time) ([]app.DecodeRecord, bool) {
		var list readsbList
		if err := json.Unmarshal(data, &list); err != nil {
			// A file caught while readsb rewrites it: the next read.
			return nil, false
		}

		progress := list.Now > clock
		clock = max(clock, list.Now)

		var out []app.DecodeRecord

		next := make(map[string]int64, len(list.Aircraft))

		for _, a := range list.Aircraft {
			rec, ok := aircraftRecord(a)
			if !ok {
				continue
			}

			next[rec.ICAO] = a.Messages

			if a.Messages <= seen[rec.ICAO] {
				continue
			}

			payload, err := json.Marshal(rec)
			if err != nil {
				continue
			}

			when := at
			if a.Seen > 0 && a.Seen < 3600 {
				when = at.Add(-time.Duration(a.Seen * float64(time.Second)))
			}

			out = append(out, app.DecodeRecord{Time: when, Schema: AircraftSchema, Text: aircraftText(rec), Payload: payload})
		}

		seen = next

		return out, progress
	}
}

// aircraftRecord validates a readsb aircraft (ok false: no valid address).
func aircraftRecord(a readsbAircraft) (AircraftRecord, bool) {
	if !icaoHex.MatchString(a.Hex) || a.Messages <= 0 {
		return AircraftRecord{}, false
	}

	r := AircraftRecord{ICAO: strings.ToUpper(a.Hex), Messages: a.Messages}

	if addressType.MatchString(a.Type) {
		r.AddressType = a.Type
	}

	if f := strings.TrimSpace(cleanRF(a.Flight)); flightID.MatchString(f) {
		r.Flight = f
	}

	if squawkCode.MatchString(a.Squawk) {
		r.Squawk = a.Squawk
	}

	if slices.Contains(emergencies, a.Emergency) {
		r.Emergency = a.Emergency
	}

	if categoryCode.MatchString(a.Category) {
		r.Category = a.Category
	}

	if a.Lat != nil && a.Lon != nil && validLatLon(*a.Lat, *a.Lon) {
		r.Lat, r.Lon = a.Lat, a.Lon
	}

	var ground string
	if json.Unmarshal(a.AltBaro, &ground) == nil {
		r.Ground = ground == "ground"
	} else {
		var ft float64
		if json.Unmarshal(a.AltBaro, &ft) == nil {
			r.AltitudeFt = bounded(&ft, -2000, 100_000)
		}
	}

	r.GeomAltitudeFt = bounded(a.AltGeom, -2000, 100_000)
	r.SpeedKt = bounded(a.GS, 0, 5000)
	r.TrackDeg = bounded(a.Track, 0, 360)
	r.VerticalRate = bounded(a.BaroRate, -100_000, 100_000)

	if r.VerticalRate == nil {
		r.VerticalRate = bounded(a.GeomRate, -100_000, 100_000)
	}

	r.RSSI = bounded(a.RSSI, -100, 10)

	return r, true
}

// bounded returns v if it is finite and within [lo, hi], else nil.
func bounded(v *float64, lo, hi float64) *float64 {
	if v == nil || math.IsNaN(*v) || *v < lo || *v > hi {
		return nil
	}

	x := *v

	return &x
}

func validLatLon(lat, lon float64) bool {
	return !math.IsNaN(lat) && !math.IsNaN(lon) && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 && (lat != 0 || lon != 0)
}

// aircraftText renders an aircraft: address, flight, altitude, position,
// squawk and emergency ("4CA1FA EIN5AT 12000 ft 51.90001,3.79997 squawk
// 7700 emergency general").
func aircraftText(r AircraftRecord) string {
	parts := []string{r.ICAO}

	if r.Flight != "" {
		parts = append(parts, r.Flight)
	}

	switch {
	case r.Ground:
		parts = append(parts, "ground")
	case r.AltitudeFt != nil:
		parts = append(parts, strconv.FormatFloat(*r.AltitudeFt, 'f', 0, 64)+" ft")
	}

	if r.Lat != nil && r.Lon != nil {
		parts = append(parts, strconv.FormatFloat(*r.Lat, 'f', 5, 64)+","+strconv.FormatFloat(*r.Lon, 'f', 5, 64))
	}

	if r.Squawk != "" {
		parts = append(parts, "squawk "+r.Squawk)
	}

	if r.Emergency != "" {
		parts = append(parts, "emergency "+r.Emergency)
	}

	return strings.Join(parts, " ")
}
