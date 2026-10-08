package decoder

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
)

// The APRS parser (DEC-032, APRS 1.0.1 and its 1.1/1.2 addenda): every
// AX.25 frame of the packet decoder becomes one aprs.v1 record with the
// TNC2 monitor line as its text. Positions (plain, compressed, Mic-E),
// status, messages and acknowledgements, objects, items, third-party
// traffic, NMEA sentences and weather reports are parsed into the payload,
// with the map key (source, object or item) and the digipeater hops: the
// map (M3) and the iGate (M4) read them later (ADR 0028).

// APRSSchema names the aprs.v1 payload.
const APRSSchema = "aprs.v1"

// Unit conversions: APRS uses knots, feet, miles, inches, °F.
const (
	knotKMH = 1.852
	mileKM  = 1.609344
	footM   = 0.3048
	inchMM  = 25.4
)

// APRSRecord is the aprs.v1 payload.
type APRSRecord struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	Path        []string `json:"path"`
	// Hops are the stations that digipeated the packet, the first one
	// (for third-party, objects and items) being the sender.
	Hops []string `json:"hops"`
	// Type is position, mic-e, status, message, ack, rej, object, item,
	// weather, nmea, thirdparty or other.
	Type string `json:"type"`
	// Key is the map key: the source, or the object or item name.
	Key string `json:"key"`
	// Timestamp is the time the packet carries, resolved to a full UTC
	// date around the reception.
	Timestamp *time.Time `json:"timestamp,omitempty"`
	Lat       *float64   `json:"lat,omitempty"`
	Lon       *float64   `json:"lon,omitempty"`
	// Ambiguity is the number of position digits blanked by the sender.
	Ambiguity  int    `json:"ambiguity,omitempty"`
	Compressed bool   `json:"compressed,omitempty"`
	Symbol     string `json:"symbol,omitempty"` // table + code
	// Course (degrees), speed (km/h), altitude (m), range (km).
	Course   *int     `json:"course,omitempty"`
	Speed    *float64 `json:"speed_kmh,omitempty"`
	Altitude *float64 `json:"altitude_m,omitempty"`
	Range    *float64 `json:"range_km,omitempty"`
	// PHG: power (W), antenna height (m), gain (dB), directivity (degrees,
	// 0 omni).
	PHG     *APRSPHG     `json:"phg,omitempty"`
	Weather *APRSWeather `json:"weather,omitempty"`
	Comment string       `json:"comment,omitempty"`
	// Messages.
	Addressee string `json:"addressee,omitempty"`
	Message   string `json:"message,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	// Objects and items: their name and whether they are alive (false:
	// killed).
	Name string `json:"name,omitempty"`
	Live *bool  `json:"live,omitempty"`
	// MicE is the Mic-E message type (en route, in service…).
	MicE string `json:"mic_e,omitempty"`
	// NMEA is the raw sentence of an NMEA packet.
	NMEA string `json:"nmea,omitempty"`
	// Forwarded is the inner packet of third-party traffic.
	Forwarded *APRSRecord `json:"forwarded,omitempty"`
}

// APRSPHG is a power-height-gain-directivity extension.
type APRSPHG struct {
	PowerW      int     `json:"power_w"`
	HeightM     float64 `json:"height_m"`
	GainDB      int     `json:"gain_db"`
	Directivity int     `json:"directivity"`
}

// APRSWeather is a weather report, in metric units.
type APRSWeather struct {
	WindDir      *int     `json:"wind_dir,omitempty"`
	WindKMH      *float64 `json:"wind_kmh,omitempty"`
	GustKMH      *float64 `json:"gust_kmh,omitempty"`
	TempC        *float64 `json:"temp_c,omitempty"`
	RainHourMM   *float64 `json:"rain_1h_mm,omitempty"`
	Rain24hMM    *float64 `json:"rain_24h_mm,omitempty"`
	RainMidMM    *float64 `json:"rain_midnight_mm,omitempty"`
	Humidity     *int     `json:"humidity,omitempty"`
	PressureHPa  *float64 `json:"pressure_hpa,omitempty"`
	Luminosity   *int     `json:"luminosity,omitempty"`
	SnowfallMM   *float64 `json:"snow_24h_mm,omitempty"`
	RawRainCount *int     `json:"rain_count,omitempty"`
}

// newPacketParser returns the frame parser of a packet session.
func newPacketParser(sessionConfig) frameParser {
	return func(frame []byte, at time.Time) []app.DecodeRecord {
		rec, ok := parsePacket(frame, at)
		if !ok {
			return nil
		}

		return []app.DecodeRecord{rec}
	}
}

// parsePacket turns an AX.25 frame into an aprs.v1 record; a frame that is
// not a UI frame is dropped.
func parsePacket(frame []byte, at time.Time) (app.DecodeRecord, bool) {
	f, err := parseAX25(frame)
	if err != nil {
		return app.DecodeRecord{}, false
	}

	info := strings.TrimRight(strings.ToValidUTF8(string(f.Info), "�"), "\r\n")
	r := parseAPRS(f.Source, f.Destination, f.Path, info, at, 0)

	payload, err := json.Marshal(r)
	if err != nil {
		return app.DecodeRecord{}, false
	}

	return app.DecodeRecord{Time: at, Schema: APRSSchema, Text: cleanRF(tnc2(f.Source, f.Destination, f.Path, info)), Payload: payload}, true
}

// tnc2 is the monitor line of a packet: SRC>DEST,PATH:info.
func tnc2(src, dst string, path []string, info string) string {
	head := src + ">" + dst
	if len(path) > 0 {
		head += "," + strings.Join(path, ",")
	}

	return head + ":" + info
}

// noHop are path aliases: they are not stations.
var noHop = regexp.MustCompile(`^(WIDE[0-9]?(-[0-9])?|TRACE[0-9]?(-[0-9])?|RELAY|ECHO|GATE|RFONLY|NOGATE|TCPIP|TCPXX|Q[A-Z]{2})\*?$`)

// hops returns the stations that digipeated a packet (marked with "*").
func hops(path []string) []string {
	out := []string{}

	for _, p := range path {
		if strings.HasSuffix(p, "*") && !noHop.MatchString(p) {
			out = append(out, strings.TrimSuffix(p, "*"))
		}
	}

	return out
}

// parseAPRS parses the information field of a packet. depth bounds the
// third-party nesting.
func parseAPRS(src, dst string, path []string, info string, at time.Time, depth int) *APRSRecord {
	if path == nil {
		path = []string{}
	}

	r := &APRSRecord{Source: src, Destination: dst, Path: path, Type: "other", Key: src}
	r.Hops = hops(path)

	if info == "" {
		return r
	}

	switch dti := info[0]; dti {
	case '!', '=':
		r.Type = "position"
		r.position(info[1:])
	case '/', '@':
		r.Type = "position"
		if len(info) >= 8 {
			r.Timestamp = aprsTime(info[1:8], at)
			r.position(info[8:])
		}
	case '`', '\'', 0x1c, 0x1d:
		r.Type = "mic-e"
		r.micE(dst, info)
	case '>':
		r.Type = "status"
		s := info[1:]
		if len(s) >= 7 && s[6] == 'z' && digits(s[:6]) {
			r.Timestamp = aprsTime(s[:7], at)
			s = s[7:]
		}

		r.Comment = s
	case ':':
		r.message(info[1:])
	case ';':
		r.Type = "object"
		r.object(info[1:], at)
	case ')':
		r.Type = "item"
		r.item(info[1:])
	case '}':
		r.Type = "thirdparty"
		if depth == 0 {
			r.thirdParty(info[1:], at)
		}
	case '_':
		r.Type = "weather"
		s := info[1:]
		if len(s) >= 8 && digits(s[:8]) {
			s = s[8:]
		}

		r.Weather, r.Comment = parseWeather(s, &APRSWeather{})
	case '$':
		r.Type = "nmea"
		r.NMEA = info
		r.nmea(info)
	default:
		r.Comment = info
	}

	if r.Type == "position" && r.Symbol != "" && r.Symbol[1] == '_' {
		r.Type = "weather"
	}

	r.clean()

	return r
}

// clean removes control characters from the RF strings of the record
// (§8.4 rule 4).
func (r *APRSRecord) clean() {
	for _, p := range []*string{&r.Source, &r.Destination, &r.Key, &r.Symbol, &r.Comment, &r.Addressee, &r.Message, &r.MessageID, &r.Name, &r.MicE, &r.NMEA} {
		*p = cleanRF(*p)
	}

	for i := range r.Path {
		r.Path[i] = cleanRF(r.Path[i])
	}

	for i := range r.Hops {
		r.Hops[i] = cleanRF(r.Hops[i])
	}
}

func digits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return s != ""
}

// aprsTime resolves a 7-character APRS timestamp to a full UTC time near
// the reception time at: DDHHMMz and DDHHMM/ (day of month, zulu or local;
// local time is not known and read as UTC), HHMMSSh (time of day). The
// day, month and year are those of the candidate nearest to at, so that a
// timestamp of 23:59:50 received at 00:00:05 is yesterday's.
func aprsTime(s string, at time.Time) *time.Time {
	if len(s) != 7 || !digits(s[:6]) {
		return nil
	}

	a, _ := strconv.Atoi(s[0:2])
	b, _ := strconv.Atoi(s[2:4])
	c, _ := strconv.Atoi(s[4:6])
	at = at.UTC()

	var candidates []time.Time

	switch s[6] {
	case 'h':
		if a > 23 || b > 59 || c > 59 {
			return nil
		}

		for d := -1; d <= 1; d++ {
			day := at.AddDate(0, 0, d)
			candidates = append(candidates, time.Date(day.Year(), day.Month(), day.Day(), a, b, c, 0, time.UTC))
		}
	case 'z', '/':
		if a < 1 || a > 31 || b > 23 || c > 59 {
			return nil
		}

		for m := -1; m <= 1; m++ {
			first := time.Date(at.Year(), at.Month()+time.Month(m), 1, 0, 0, 0, 0, time.UTC)
			t := time.Date(first.Year(), first.Month(), a, b, c, 0, 0, time.UTC)

			// A day past the end of that month is not a candidate.
			if t.Month() == first.Month() {
				candidates = append(candidates, t)
			}
		}
	default:
		return nil
	}

	var best *time.Time

	for _, t := range candidates {
		if best == nil || absDur(t.Sub(at)) < absDur(best.Sub(at)) {
			best = &t
		}
	}

	return best
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}

	return d
}

// position parses a position (plain or compressed) and what follows it.
func (r *APRSRecord) position(s string) {
	// A plain latitude starts with a digit (or a space, ambiguity); a
	// compressed position with its symbol table.
	if len(s) >= 13 && (s[0] == '/' || s[0] == '\\' || (s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'j')) {
		r.compressedPosition(s)

		return
	}

	if len(s) < 19 {
		r.Comment = s

		return
	}

	lat, amb, ok := plainCoord(s[0:8], 2, 'N', 'S', 90)
	if !ok {
		r.Comment = s

		return
	}

	lon, _, ok := plainCoord(s[9:18], 3, 'E', 'W', 180)
	if !ok {
		r.Comment = s

		return
	}

	r.Lat, r.Lon, r.Ambiguity = &lat, &lon, amb
	r.Symbol = string([]byte{s[8], s[18]})
	r.extensions(s[19:])
}

// plainCoord parses DDMM.mmN or DDDMM.mmE, spaces (position ambiguity)
// read as zeros.
func plainCoord(s string, degDigits int, pos, neg byte, limit float64) (float64, int, bool) {
	amb := strings.Count(s, " ")
	t := strings.ReplaceAll(s, " ", "0")

	if t[degDigits+2] != '.' || !digits(t[:degDigits+2]) || !digits(t[degDigits+3:len(t)-1]) {
		return 0, 0, false
	}

	deg, _ := strconv.Atoi(t[:degDigits])
	minutes, err := strconv.ParseFloat(t[degDigits:len(t)-1], 64)

	if err != nil || minutes >= 60 {
		return 0, 0, false
	}

	v := float64(deg) + minutes/60

	switch t[len(t)-1] {
	case pos:
	case neg:
		v = -v
	default:
		return 0, 0, false
	}

	if math.Abs(v) > limit {
		return 0, 0, false
	}

	return round6(v), amb, true
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// base91 decodes printable base-91 digits ('!' is 0).
func base91(s string) (int, bool) {
	n := 0

	for i := range len(s) {
		c := int(s[i]) - 33
		if c < 0 || c > 90 {
			return 0, false
		}

		n = n*91 + c
	}

	return n, true
}

// compressedPosition parses a compressed position: table, YYYY, XXXX,
// code, course/speed or range or altitude, type.
func (r *APRSRecord) compressedPosition(s string) {
	y, ok1 := base91(s[1:5])
	x, ok2 := base91(s[5:9])

	if !ok1 || !ok2 {
		r.Comment = s

		return
	}

	lat, lon := round6(90-float64(y)/380926), round6(-180+float64(x)/190463)
	if math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		r.Comment = s

		return
	}

	r.Lat, r.Lon, r.Compressed = &lat, &lon, true
	r.Symbol = string([]byte{s[0], s[9]})

	c, sp, t := s[10], s[11], s[12]

	switch {
	case c == ' ':
	case (t-33)&0x18 == 0x10:
		if v, ok := base91(s[10:12]); ok {
			alt := round1(math.Pow(1.002, float64(v)) * footM)
			r.Altitude = &alt
		}
	case c == '{':
		rng := round1(2 * math.Pow(1.08, float64(int(sp)-33)) * mileKM)
		r.Range = &rng
	case c >= '!' && c <= 'z':
		course := (int(c) - 33) * 4
		speed := round1((math.Pow(1.08, float64(int(sp)-33)) - 1) * knotKMH)
		r.Course, r.Speed = &course, &speed
	}

	r.comment(s[13:])
}

var phgPower = [10]int{0, 1, 4, 9, 16, 25, 36, 49, 64, 81}

// extensions parses the data extension after a plain position, then the
// comment.
func (r *APRSRecord) extensions(s string) {
	if r.Symbol != "" && r.Symbol[1] == '_' {
		w := &APRSWeather{}

		if len(s) >= 7 && s[3] == '/' {
			if d, err := strconv.Atoi(s[0:3]); err == nil {
				w.WindDir = &d
			}

			if v, err := strconv.Atoi(s[4:7]); err == nil {
				kmh := round1(float64(v) * mileKM)
				w.WindKMH = &kmh
			}

			s = s[7:]
		}

		r.Weather, s = parseWeather(s, w)
		r.comment(s)

		return
	}

	if len(s) >= 7 {
		switch {
		case s[3] == '/' && digits(s[0:3]) && digits(s[4:7]):
			course, _ := strconv.Atoi(s[0:3])
			knots, _ := strconv.Atoi(s[4:7])
			speed := round1(float64(knots) * knotKMH)
			r.Course, r.Speed = &course, &speed
			s = s[7:]
		case strings.HasPrefix(s, "PHG") && digits(s[3:7]):
			h := round1(math.Pow(2, float64(s[4]-'0')) * 10 * footM)
			r.PHG = &APRSPHG{PowerW: phgPower[s[3]-'0'], HeightM: h, GainDB: int(s[5] - '0'), Directivity: int(s[6]-'0') * 45}
			s = s[7:]
		case strings.HasPrefix(s, "RNG") && digits(s[3:7]):
			miles, _ := strconv.Atoi(s[3:7])
			rng := round1(float64(miles) * mileKM)
			r.Range = &rng
			s = s[7:]
		}
	}

	r.comment(s)
}

var altitudeTag = regexp.MustCompile(`/A=(-?\d{5,6})`)

// comment keeps the comment, its /A= altitude (feet) read out.
func (r *APRSRecord) comment(s string) {
	if m := altitudeTag.FindStringSubmatchIndex(s); m != nil {
		ft, _ := strconv.Atoi(s[m[2]:m[3]])
		alt := round1(float64(ft) * footM)
		r.Altitude = &alt
		s = s[:m[0]] + s[m[1]:]
	}

	r.Comment = strings.TrimSpace(s)
}

// parseWeather reads the weather fields (letter + digits) at the start of
// s into w; the rest is returned as the comment. Missing values ("...",
// spaces) are skipped.
func parseWeather(s string, w *APRSWeather) (*APRSWeather, string) {
	type field struct {
		n   int
		set func(v int)
	}

	fl := func(p **float64, scale float64) func(v int) {
		return func(v int) { x := round1(float64(v) * scale); *p = &x }
	}

	fields := map[byte]field{
		'c': {3, func(v int) { w.WindDir = &v }},
		's': {3, fl(&w.WindKMH, mileKM)},
		'g': {3, fl(&w.GustKMH, mileKM)},
		't': {3, func(v int) { x := round1(float64(v-32) * 5 / 9); w.TempC = &x }},
		'r': {3, fl(&w.RainHourMM, inchMM/100)},
		'p': {3, fl(&w.Rain24hMM, inchMM/100)},
		'P': {3, fl(&w.RainMidMM, inchMM/100)},
		'h': {2, func(v int) {
			if v == 0 {
				v = 100
			}
			w.Humidity = &v
		}},
		'b': {5, fl(&w.PressureHPa, 0.1)},
		'L': {3, func(v int) { w.Luminosity = &v }},
		'l': {3, func(v int) { v += 1000; w.Luminosity = &v }},
		'#': {3, func(v int) { w.RawRainCount = &v }},
	}

	for len(s) > 0 {
		f, ok := fields[s[0]]
		if !ok || len(s) < f.n+1 {
			break
		}

		// Missing values are dots or spaces: Atoi refuses them.
		if n, err := strconv.Atoi(s[1 : 1+f.n]); err == nil {
			f.set(n)
		}

		s = s[1+f.n:]
	}

	return w, strings.TrimSpace(s)
}

var msgID = regexp.MustCompile(`^(.*)\{([0-9A-Za-z]{1,5})(\}.*)?$`)

// message parses :ADDRESSEE:text{id, acks and rejects.
func (r *APRSRecord) message(s string) {
	r.Type = "message"
	if len(s) < 10 || s[9] != ':' {
		r.Comment = s

		return
	}

	r.Addressee = strings.TrimSpace(s[:9])
	text := s[10:]

	switch {
	case strings.HasPrefix(text, "ack") && len(text) <= 8:
		r.Type, r.MessageID = "ack", text[3:]
	case strings.HasPrefix(text, "rej") && len(text) <= 8:
		r.Type, r.MessageID = "rej", text[3:]
	default:
		if m := msgID.FindStringSubmatch(text); m != nil {
			text, r.MessageID = m[1], m[2]
		}

		r.Message = text
	}
}

// object parses ;NAME_____*DDHHMMzposition.
func (r *APRSRecord) object(s string, at time.Time) {
	if len(s) < 17 || (s[9] != '*' && s[9] != '_') {
		r.Comment = s

		return
	}

	live := s[9] == '*'
	r.Name, r.Live = strings.TrimSpace(s[:9]), &live
	r.Key = r.Name
	r.Timestamp = aprsTime(s[10:17], at)
	r.position(s[17:])
}

// item parses )NAME!position: a name of 3 to 9 characters ended by '!'
// (live) or '_' (killed).
func (r *APRSRecord) item(s string) {
	end := strings.IndexAny(s, "!_")
	if end < 3 || end > 9 {
		r.Comment = s

		return
	}

	live := s[end] == '!'
	r.Name, r.Live = s[:end], &live
	r.Key = r.Name
	r.position(s[end+1:])
}

var thirdPartyHeader = regexp.MustCompile(`^([A-Za-z0-9-]{1,9})>([A-Za-z0-9-]{1,9})((?:,[A-Za-z0-9-]{1,9}\*?)*):(.*)$`)

// thirdParty parses }SRC>DEST,PATH:info as the forwarded packet.
func (r *APRSRecord) thirdParty(s string, at time.Time) {
	m := thirdPartyHeader.FindStringSubmatch(s)
	if m == nil {
		r.Comment = s

		return
	}

	path := []string{}
	if m[3] != "" {
		path = strings.Split(m[3][1:], ",")
	}

	r.Forwarded = parseAPRS(strings.ToUpper(m[1]), strings.ToUpper(m[2]), path, m[4], at, 1)
	r.Key = r.Forwarded.Key
	r.Hops = append([]string{r.Source}, r.Hops...)
}

// nmea reads the position of $GPRMC/$GNRMC and $GPGGA/$GNGGA sentences.
func (r *APRSRecord) nmea(s string) {
	if i := strings.IndexByte(s, '*'); i > 0 {
		s = s[:i]
	}

	f := strings.Split(s, ",")
	if len(f) < 7 {
		return
	}

	var latF, latH, lonF, lonH string

	switch {
	case strings.HasSuffix(f[0], "RMC") && len(f) >= 9 && f[2] == "A":
		latF, latH, lonF, lonH = f[3], f[4], f[5], f[6]

		if v, err := strconv.ParseFloat(f[7], 64); err == nil {
			speed := round1(v * knotKMH)
			r.Speed = &speed
		}

		if v, err := strconv.ParseFloat(f[8], 64); err == nil {
			course := int(math.Round(v))
			r.Course = &course
		}
	case strings.HasSuffix(f[0], "GGA") && len(f) >= 10 && f[6] != "0":
		latF, latH, lonF, lonH = f[2], f[3], f[4], f[5]

		if v, err := strconv.ParseFloat(f[9], 64); err == nil {
			alt := round1(v)
			r.Altitude = &alt
		}
	default:
		return
	}

	lat, ok1 := nmeaCoord(latF, latH, 2, "N", "S")
	lon, ok2 := nmeaCoord(lonF, lonH, 3, "E", "W")

	if ok1 && ok2 {
		r.Lat, r.Lon = &lat, &lon
	}
}

func nmeaCoord(v, hemi string, degDigits int, pos, neg string) (float64, bool) {
	if len(v) < degDigits+2 {
		return 0, false
	}

	deg, err1 := strconv.Atoi(v[:degDigits])
	minutes, err2 := strconv.ParseFloat(v[degDigits:], 64)

	if err1 != nil || err2 != nil || minutes >= 60 {
		return 0, false
	}

	x := float64(deg) + minutes/60

	switch hemi {
	case pos:
	case neg:
		x = -x
	default:
		return 0, false
	}

	return round6(x), true
}

// Mic-E message types (APRS 1.0.1 ch. 10): standard messages A/B/C bits.
var micEMessages = [8]string{"emergency", "priority", "special", "committed", "returning", "in service", "en route", "off duty"}

// micE decodes a Mic-E packet: latitude, message bits and longitude offset
// in the destination, longitude, speed, course and symbol in the
// information field.
func (r *APRSRecord) micE(dst, info string) {
	if len(info) < 9 {
		r.Comment = info

		return
	}

	if i := strings.IndexByte(dst, '-'); i >= 0 {
		dst = dst[:i]
	}

	if len(dst) != 6 {
		r.Comment = info

		return
	}

	var (
		lat  [6]int
		msg  int
		cust bool
	)

	for i := range 6 {
		c := dst[i]

		switch {
		case c >= '0' && c <= '9':
			lat[i] = int(c - '0')
		case c >= 'A' && c <= 'J':
			lat[i] = int(c - 'A')
			cust = cust || i < 3
		case c >= 'P' && c <= 'Y':
			lat[i] = int(c - 'P')
		case c == 'K' || c == 'L' || c == 'Z':
			lat[i] = 0
		default:
			r.Comment = info

			return
		}

		if i < 3 && ((c >= 'A' && c <= 'K') || (c >= 'P' && c <= 'Z')) {
			msg |= 1 << (2 - i)
		}
	}

	latV := float64(lat[0]*10+lat[1]) + (float64(lat[2]*10+lat[3])+float64(lat[4]*10+lat[5])/100)/60
	if dst[3] <= '9' || dst[3] == 'L' {
		latV = -latV
	}

	d := int(info[1]) - 28
	if dst[4] >= 'P' {
		d += 100
	}

	switch {
	case d >= 180 && d <= 189:
		d -= 80
	case d >= 190 && d <= 199:
		d -= 190
	}

	m := int(info[2]) - 28
	if m >= 60 {
		m -= 60
	}

	h := int(info[3]) - 28
	lonV := float64(d) + (float64(m)+float64(h)/100)/60

	if dst[5] >= 'P' {
		lonV = -lonV
	}

	if math.Abs(latV) > 90 || math.Abs(lonV) > 180 || d < 0 || m < 0 || h < 0 {
		r.Comment = info

		return
	}

	latV, lonV = round6(latV), round6(lonV)
	r.Lat, r.Lon = &latV, &lonV

	sp := int(info[4]) - 28
	dc := int(info[5]) - 28
	se := int(info[6]) - 28
	knots := sp*10 + dc/10
	course := (dc%10)*100 + se

	if knots >= 800 {
		knots -= 800
	}

	if course >= 400 {
		course -= 400
	}

	// Out of range values are not reported (corrupted or unknown).
	if knots >= 0 && knots < 800 && course >= 0 && course <= 360 {
		speed := round1(float64(knots) * knotKMH)
		r.Speed, r.Course = &speed, &course
	}

	r.Symbol = string([]byte{info[8], info[7]})

	if cust {
		r.MicE = "custom"
	} else {
		r.MicE = micEMessages[msg]
	}

	rest := info[9:]
	if len(rest) > 0 && strings.IndexByte(">]`'", rest[0]) >= 0 {
		rest = rest[1:]
	}

	if len(rest) >= 4 && rest[3] == '}' {
		if v, ok := base91(rest[:3]); ok {
			alt := float64(v - 10000)
			r.Altitude = &alt
			rest = rest[4:]
		}
	}

	r.Comment = strings.TrimSpace(rest)
}
