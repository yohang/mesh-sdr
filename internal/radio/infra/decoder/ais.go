package decoder

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
)

// The AIS decoder (MAR-001): the session FM-demodulates a 48 kHz wide IQ
// channel (iqFMS16) and direwolf -B AIS demodulates the 9600 Bd GMSK
// bursts of that flat audio; it hands each one over KISS as a UI frame AIS>APDW17 whose
// information field is "{DA" (user-defined data, AIS) followed by one
// !AIVDM sentence carrying the whole burst (radio channel always "A").
// The sentence is decoded (ITU-R M.1371): the MMSI and its country, the
// position, course and speed, the name, call sign and ship type when the
// message has them. The position stays in the payload for the map (M3).
// AIS is never sent to the APRS iGate.

// AISSchema names the ais.v1 payload.
const AISSchema = "ais.v1"

// aisInfoPrefix starts the information field of direwolf's AIS frames:
// user-defined data ("{"), user id D (direwolf), type A (AIS).
const aisInfoPrefix = "{DA"

// AISRecord is the ais.v1 payload.
type AISRecord struct {
	// Type is the message type (1 to 27).
	Type     int      `json:"type"`
	MMSI     string   `json:"mmsi"`
	MMSIKind string   `json:"mmsi_kind,omitempty"`
	Country  string   `json:"country,omitempty"`
	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	// SpeedKn is the speed over ground (knots), Course the course over
	// ground and Heading the true heading (degrees).
	SpeedKn *float64 `json:"speed_kn,omitempty"`
	Course  *float64 `json:"course,omitempty"`
	Heading *int     `json:"heading,omitempty"`
	// Status is the navigational status (types 1 to 3, 27).
	Status string `json:"status,omitempty"`
	// AltitudeM is the altitude of a SAR aircraft (type 9).
	AltitudeM *int `json:"altitude_m,omitempty"`
	// Static and voyage data (types 5, 19, 21, 24).
	Name          string `json:"name,omitempty"`
	Callsign      string `json:"callsign,omitempty"`
	IMO           int    `json:"imo,omitempty"`
	ShipType      int    `json:"ship_type,omitempty"`
	ShipTypeLabel string `json:"ship_type_label,omitempty"`
	Destination   string `json:"destination,omitempty"`
	// NMEA is the sentence as direwolf wrote it.
	NMEA string `json:"nmea"`
}

// aisArgs runs direwolf as the packet decoder does (direwolfArgs), with
// its channel 0 demodulating AIS (9600 Bd GMSK, NRZI, no scrambling)
// instead of the config's 1200 Bd modem.
func aisArgs(c sessionConfig) []string {
	return append(direwolfArgs(c), "-B", "AIS")
}

// newAISParser returns the parser of the AX.25 frames of an AIS session.
func newAISParser(sessionConfig) frameParser {
	return func(frame []byte, at time.Time) []app.DecodeRecord { return single(parseAISFrame(frame, at)) }
}

// parseAISFrame turns a frame of direwolf's AIS modem into an ais.v1
// record; anything else is dropped.
func parseAISFrame(frame []byte, at time.Time) (app.DecodeRecord, bool) {
	f, err := parseAX25(frame)
	if err != nil {
		return app.DecodeRecord{}, false
	}

	sentence, ok := strings.CutPrefix(strings.TrimRight(string(f.Info), "\r\n"), aisInfoPrefix)
	if !ok {
		return app.DecodeRecord{}, false
	}

	r, err := parseAIVDM(sentence)
	if err != nil {
		return app.DecodeRecord{}, false
	}

	payload, err := json.Marshal(r)
	if err != nil {
		return app.DecodeRecord{}, false
	}

	return app.DecodeRecord{Time: at, Schema: AISSchema, Text: cleanRF(aisText(r)), Payload: payload}, true
}

// errAIS is a sentence that is not a single-sentence AIS message.
var errAIS = errors.New("ais: invalid sentence")

// parseAIVDM decodes a single-sentence !AIVDM (or !AIVDO) message.
func parseAIVDM(s string) (AISRecord, error) {
	body, sum, ok := strings.Cut(s, "*")
	if !ok || len(body) < 2 || (body[0] != '!' && body[0] != '$') {
		return AISRecord{}, errAIS
	}

	want, err := strconv.ParseUint(sum, 16, 8)
	if err != nil {
		return AISRecord{}, errAIS
	}

	var cs byte
	for i := 1; i < len(body); i++ {
		cs ^= body[i]
	}

	if cs != byte(want) {
		return AISRecord{}, fmt.Errorf("%w: checksum", errAIS)
	}

	// !AIVDM,count,number,sequence,channel,payload,fill
	f := strings.Split(body, ",")
	if len(f) != 7 || (f[0] != "!AIVDM" && f[0] != "!AIVDO") || f[1] != "1" || f[2] != "1" {
		return AISRecord{}, errAIS
	}

	fill, err := strconv.Atoi(f[6])
	if err != nil || fill < 0 || fill > 5 {
		return AISRecord{}, errAIS
	}

	b, err := unarmor(f[5], fill)
	if err != nil {
		return AISRecord{}, err
	}

	r, err := decodeAIS(b)
	if err != nil {
		return AISRecord{}, err
	}

	r.NMEA = s

	return r, nil
}

// aisBits are the bits of a message, MSB first.
type aisBits []byte

// unarmor turns the 6-bit ASCII armour of a payload into bits, the fill
// bits dropped.
func unarmor(p string, fill int) (aisBits, error) {
	bits := make(aisBits, 0, len(p)*6)

	for i := range len(p) {
		c := int(p[i]) - 48
		if c > 40 {
			c -= 8
		}

		if c < 0 || c > 63 || (p[i] > 'W' && p[i] < '`') {
			return nil, fmt.Errorf("%w: payload character %q", errAIS, p[i])
		}

		for k := 5; k >= 0; k-- {
			bits = append(bits, byte(c>>k&1))
		}
	}

	if fill > len(bits) {
		return nil, errAIS
	}

	return bits[:len(bits)-fill], nil
}

// u reads an unsigned field.
func (b aisBits) u(start, n int) int {
	v := 0
	for i := start; i < start+n; i++ {
		v = v<<1 | int(b[i])
	}

	return v
}

// s reads a two's complement field.
func (b aisBits) s(start, n int) int {
	v := b.u(start, n)
	if v>>(n-1)&1 == 1 {
		v -= 1 << n
	}

	return v
}

// text reads a 6-bit ASCII field, its '@' padding and spaces trimmed.
func (b aisBits) text(start, chars int) string {
	out := make([]byte, chars)

	for i := range chars {
		c := b.u(start+6*i, 6)
		if c < 32 {
			c += 64
		}

		out[i] = byte(c)
	}

	s := string(out)
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}

	return strings.TrimSpace(s)
}

// Message lengths: the bits a message type needs to be decoded.
var aisLengths = map[int]int{1: 168, 2: 168, 3: 168, 4: 168, 5: 422, 9: 168, 11: 168, 18: 168, 19: 312, 21: 272, 27: 96}

// decodeAIS decodes the fields of the message types that carry a position
// or static data; others keep their type and MMSI.
func decodeAIS(b aisBits) (AISRecord, error) {
	if len(b) < 38 {
		return AISRecord{}, fmt.Errorf("%w: %d bits", errAIS, len(b))
	}

	r := AISRecord{Type: b.u(0, 6), MMSI: fmt.Sprintf("%09d", b.u(8, 30))}
	if info, ok := mmsiInfo(r.MMSI); ok {
		r.MMSIKind, r.Country = info.Kind, info.Country
	}

	if need, ok := aisLengths[r.Type]; ok && len(b) < need {
		return AISRecord{}, fmt.Errorf("%w: type %d of %d bits", errAIS, r.Type, len(b))
	}

	switch r.Type {
	case 1, 2, 3:
		r.Status = navStatus(b.u(38, 4))
		r.SpeedKn = speed(b.u(50, 10))
		r.Lon, r.Lat = position(b.s(61, 28), b.s(89, 27), 600_000)
		r.Course = course(b.u(116, 12))
		r.Heading = heading(b.u(128, 9))
	case 4, 11:
		r.Lon, r.Lat = position(b.s(79, 28), b.s(107, 27), 600_000)
	case 5:
		r.IMO, r.Callsign, r.Name = b.u(40, 30), b.text(70, 7), b.text(112, 20)
		r.shipType(b.u(232, 8))
		r.Destination = b.text(302, 20)
	case 9:
		if alt := b.u(38, 12); alt != 4095 {
			r.AltitudeM = &alt
		}

		if sog := b.u(50, 10); sog != 1023 {
			v := float64(sog)
			r.SpeedKn = &v
		}

		r.Lon, r.Lat = position(b.s(61, 28), b.s(89, 27), 600_000)
		r.Course = course(b.u(116, 12))
	case 18, 19:
		r.SpeedKn = speed(b.u(46, 10))
		r.Lon, r.Lat = position(b.s(57, 28), b.s(85, 27), 600_000)
		r.Course = course(b.u(112, 12))
		r.Heading = heading(b.u(124, 9))

		if r.Type == 19 {
			r.Name = b.text(143, 20)
			r.shipType(b.u(263, 8))
		}
	case 21:
		r.Name = b.text(43, 20)
		r.Lon, r.Lat = position(b.s(164, 28), b.s(192, 27), 600_000)
	case 24:
		switch {
		case len(b) >= 160 && b.u(38, 2) == 0:
			r.Name = b.text(40, 20)
		case len(b) >= 132 && b.u(38, 2) == 1:
			r.shipType(b.u(40, 8))
			r.Callsign = b.text(90, 7)
		}
	case 27:
		r.Status = navStatus(b.u(40, 4))
		r.Lon, r.Lat = position(b.s(44, 18), b.s(62, 17), 600)

		if sog := b.u(79, 6); sog != 63 {
			v := float64(sog)
			r.SpeedKn = &v
		}

		if cog := b.u(85, 9); cog < 360 {
			v := float64(cog)
			r.Course = &v
		}
	}

	return r, nil
}

// position converts a longitude and latitude in 1/perDegree degrees into
// the longitude and latitude; out of range values (181°, 91°: not
// available) give none.
func position(lon, lat, perDegree int) (*float64, *float64) {
	x, y := float64(lon)/float64(perDegree), float64(lat)/float64(perDegree)
	if x < -180 || x > 180 || y < -90 || y > 90 {
		return nil, nil
	}

	return &x, &y
}

// speed converts a speed over ground in 1/10 knot (1023: not available).
func speed(v int) *float64 {
	if v == 1023 {
		return nil
	}

	kn := float64(v) / 10

	return &kn
}

// course converts a course over ground in 1/10 degree (3600 and above:
// not available).
func course(v int) *float64 {
	if v >= 3600 {
		return nil
	}

	deg := float64(v) / 10

	return &deg
}

// heading returns a true heading in degrees (511: not available).
func heading(v int) *int {
	if v > 359 {
		return nil
	}

	return &v
}

// navStatuses are the navigational statuses; 9 to 13 are reserved, 15 is
// "not defined".
var navStatuses = []string{
	"under way using engine", "at anchor", "not under command", "restricted manoeuvrability", "constrained by her draught", "moored",
	"aground", "engaged in fishing", "under way sailing", "", "", "", "", "", "AIS-SART active",
}

func navStatus(v int) string {
	if v < len(navStatuses) {
		return navStatuses[v]
	}

	return ""
}

// shipType sets the ship and cargo type and its label (0: not available).
func (r *AISRecord) shipType(v int) {
	if v == 0 {
		return
	}

	r.ShipType = v

	switch {
	case v >= 20 && v <= 29:
		r.ShipTypeLabel = "wing in ground"
	case v == 30:
		r.ShipTypeLabel = "fishing"
	case v == 31 || v == 32:
		r.ShipTypeLabel = "towing"
	case v >= 33 && v <= 39:
		r.ShipTypeLabel = []string{"dredging", "diving", "military", "sailing", "pleasure craft", "reserved", "reserved"}[v-33]
	case v >= 40 && v <= 49:
		r.ShipTypeLabel = "high speed craft"
	case v >= 50 && v <= 59:
		r.ShipTypeLabel = []string{
			"pilot vessel", "search and rescue", "tug", "port tender", "anti-pollution", "law enforcement", "local vessel", "local vessel",
			"medical transport", "noncombatant",
		}[v-50]
	case v >= 60 && v <= 69:
		r.ShipTypeLabel = "passenger"
	case v >= 70 && v <= 79:
		r.ShipTypeLabel = "cargo"
	case v >= 80 && v <= 89:
		r.ShipTypeLabel = "tanker"
	case v >= 90 && v <= 99:
		r.ShipTypeLabel = "other"
	}
}

// aisText is the text of an AIS record: "227006760 France: 48.38333
// -4.49167, 12.3 kn, 254°, under way using engine".
func aisText(r AISRecord) string {
	head := r.MMSI
	if r.Country != "" {
		head += " " + r.Country
	}

	var parts []string

	add := func(s string) {
		if s != "" {
			parts = append(parts, s)
		}
	}

	add(strings.TrimSpace(r.Name + " " + r.Callsign))
	add(r.ShipTypeLabel)

	if r.Lat != nil && r.Lon != nil {
		add(strconv.FormatFloat(*r.Lat, 'f', 5, 64) + " " + strconv.FormatFloat(*r.Lon, 'f', 5, 64))
	}

	if r.AltitudeM != nil {
		add(strconv.Itoa(*r.AltitudeM) + " m")
	}

	if r.SpeedKn != nil {
		add(strconv.FormatFloat(*r.SpeedKn, 'f', 1, 64) + " kn")
	}

	if r.Course != nil {
		add(strconv.FormatFloat(*r.Course, 'f', 0, 64) + "°")
	}

	add(r.Status)

	if r.Destination != "" {
		add("to " + r.Destination)
	}

	if len(parts) == 0 {
		return head + ": type " + strconv.Itoa(r.Type)
	}

	return head + ": " + strings.Join(parts, ", ")
}
