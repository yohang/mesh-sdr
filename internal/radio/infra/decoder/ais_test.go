package decoder

import (
	"encoding/json"
	"fmt"
	"math"
	"math/cmplx"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"
)

// nmea completes a sentence with its checksum.
func nmea(body string) string {
	var cs byte
	for i := 1; i < len(body); i++ {
		cs ^= body[i]
	}

	return fmt.Sprintf("%s*%02X", body, cs)
}

// direwolfAIS is the information field direwolf 1.7 wrote for a type 1
// burst (synthesised GMSK, decoded by direwolf -B AIS).
const direwolfAIS = "{DA!AIVDM,1,1,,A,13HOI:001swcL5@KcnL9s7tt0000,0*48"

func TestParseAISFrame(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)

	for _, tc := range []struct {
		name string
		info string
		text string
		want *AISRecord
	}{
		{
			name: "direwolf type 1", info: direwolfAIS,
			text: "227006760 France: 48.38333 -4.49167, 12.3 kn, 254°, under way using engine",
			want: &AISRecord{
				Type: 1, MMSI: "227006760", MMSIKind: MMSIShip, Country: "France", Lat: ptr(48.383333333333333), Lon: ptr(-4.491666666666666),
				SpeedKn: ptr(12.3), Course: ptr(254.0), Heading: ptr(254), Status: "under way using engine",
				NMEA: "!AIVDM,1,1,,A,13HOI:001swcL5@KcnL9s7tt0000,0*48",
			},
		},
		// The examples of the AIVDM/AIVDO protocol decoding guide (gpsd).
		{
			name: "type 1", info: aisInfoPrefix + nmea("!AIVDM,1,1,,A,15RTgt0PAso;90TKcjM8h6g208CQ,0"),
			text: "371798000 Panama: 48.38163 -123.39538, 12.3 kn, 224°, under way using engine",
			want: &AISRecord{
				Type: 1, MMSI: "371798000", MMSIKind: MMSIShip, Country: "Panama", Lat: ptr(48.38163333333333), Lon: ptr(-123.39538333333333),
				SpeedKn: ptr(12.3), Course: ptr(224.0), Heading: ptr(215), Status: "under way using engine",
				NMEA: "!AIVDM,1,1,,A,15RTgt0PAso;90TKcjM8h6g208CQ,0*4A",
			},
		},
		{
			// direwolf writes a burst as one sentence, a type 5 included.
			name: "type 5", info: aisInfoPrefix + nmea("!AIVDM,1,1,,A,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216L961O5Gf0NSQEp6ClRp888888888880,2"),
			text: "351759000 Panama: EVER DIADEM 3FOF8, cargo, to NEW YORK",
			want: &AISRecord{
				Type: 5, MMSI: "351759000", MMSIKind: MMSIShip, Country: "Panama", Name: "EVER DIADEM", Callsign: "3FOF8", IMO: 9134270, ShipType: 70,
				ShipTypeLabel: "cargo", Destination: "NEW YORK",
				NMEA: "!AIVDM,1,1,,A,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216L961O5Gf0NSQEp6ClRp888888888880,2*1C",
			},
		},
		{
			name: "type 18, no heading", info: aisInfoPrefix + nmea("!AIVDM,1,1,,B,B5NJ;PP005l4ot5Isbl03wsUkP06,0"),
			text: "367430530 United States: 37.78504 -122.26732, 0.0 kn, 0°",
			want: &AISRecord{
				Type: 18, MMSI: "367430530", MMSIKind: MMSIShip, Country: "United States", Lat: ptr(37.785035), Lon: ptr(-122.26732), SpeedKn: ptr(0.0),
				Course: ptr(0.0), NMEA: "!AIVDM,1,1,,B,B5NJ;PP005l4ot5Isbl03wsUkP06,0*75",
			},
		},
		{name: "bad checksum", info: "{DA!AIVDM,1,1,,A,13HOI:001swcL5@KcnL9s7tt0000,0*49"},
		{name: "fragment", info: aisInfoPrefix + nmea("!AIVDM,2,1,1,A,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216L961O5Gf0NSQEp6ClRp8,0")},
		{name: "invalid armour", info: aisInfoPrefix + nmea("!AIVDM,1,1,,A,13HOI:001swcL5@KcnL9s7tt000X,0")},
		{name: "short type 1", info: aisInfoPrefix + nmea("!AIVDM,1,1,,A,13HOI:001swcL5@,0")},
		{name: "not ais", info: "!4237.14N/07120.83W#"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, ok := parseAISFrame(ax25("APDW17", "AIS", nil, tc.info), at)
			if ok != (tc.want != nil) {
				t.Fatalf("ok %v", ok)
			}

			if !ok {
				return
			}

			var got AISRecord
			if err := json.Unmarshal(rec.Payload, &got); err != nil {
				t.Fatal(err)
			}

			if rec.Text != tc.text || rec.Schema != AISSchema || !rec.Time.Equal(at) || !reflect.DeepEqual(&got, tc.want) {
				t.Fatalf("record %q %s", rec.Text, rec.Payload)
			}
		})
	}
}

// TestAISArgs: direwolf runs its AIS modem; its frames are parsed.
func TestAISArgs(t *testing.T) {
	args := aisArgs(sessionConfig{})
	if n := len(args); n < 2 || args[n-2] != "-B" || args[n-1] != "AIS" {
		t.Fatalf("args %q", args)
	}

	if recs := newAISParser(sessionConfig{})(ax25("APDW17", "AIS", nil, direwolfAIS), time.Now()); len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
}

// TestAISPosition: 181° and 91° (not available) give no position, the
// limits do.
func TestAISPosition(t *testing.T) {
	for _, tc := range []struct {
		lon, lat, per int
		ok            bool
	}{
		{181 * 600_000, 91 * 600_000, 600_000, false},
		{181 * 600_000, 0, 600_000, false},
		{0, 91 * 600_000, 600_000, false},
		{181 * 600, 91 * 600, 600, false},
		{-180 * 600_000, 90 * 600_000, 600_000, true},
		{180 * 600, -90 * 600, 600, true},
	} {
		if lon, lat := position(tc.lon, tc.lat, tc.per); (lon != nil && lat != nil) != tc.ok || (lon == nil) != (lat == nil) {
			t.Errorf("position(%d, %d, %d) = %v, %v", tc.lon, tc.lat, tc.per, lon, lat)
		}
	}
}

// aisBurst returns the channel bits (NRZI, ±1) of an AIS burst: training
// sequence, flag, the message bytes (MSB first in the message, each byte
// sent LSB first) and their FCS, bit-stuffed, flag.
func aisBurst(msg []byte) []float64 {
	crc := uint16(0xffff)

	for _, d := range msg {
		crc ^= uint16(d)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0x8408
			} else {
				crc >>= 1
			}
		}
	}

	crc = ^crc
	data := append(slices.Clone(msg), byte(crc), byte(crc>>8))
	flag := []int{0, 1, 1, 1, 1, 1, 1, 0}

	var raw []int
	for i := range 24 {
		raw = append(raw, i%2)
	}

	raw = append(raw, flag...)
	ones := 0

	for _, b := range data {
		for i := range 8 {
			v := int(b >> i & 1)
			raw = append(raw, v)

			if v == 0 {
				ones = 0

				continue
			}

			if ones++; ones == 5 {
				raw, ones = append(raw, 0), 0
			}
		}
	}

	raw = append(raw, flag...)
	raw = append(raw, 0, 0, 0, 0, 0, 0, 0, 0)

	out := make([]float64, len(raw))
	level := 1.0

	for i, v := range raw {
		if v == 0 {
			level = -level
		}

		out[i] = level
	}

	return out
}

// gmsk is the Gaussian filtered (BT 0.4) level of 9600 Bd bits at t.
func gmsk(bits []float64, t float64) float64 {
	const symbol = 1.0 / 9600

	c := math.Pi * 0.4 / symbol * math.Sqrt(2/math.Ln2)
	k0 := int(t / symbol)
	v := 0.0

	for k := max(k0-4, 0); k <= min(k0+4, len(bits)-1); k++ {
		tc := t - (float64(k)+0.5)*symbol
		v += bits[k] * 0.5 * (math.Erf(c*(tc+symbol/2)) - math.Erf(c*(tc-symbol/2)))
	}

	return v
}

// aisIQ returns the wide IQ at 48 kHz of three AIS bursts of the message
// of direwolfAIS (FM, ±2.4 kHz deviation) 0.2 s apart, with noise.
func aisIQ(t *testing.T) []complex64 {
	t.Helper()

	payload := "13HOI:001swcL5@KcnL9s7tt0000"

	b, err := unarmor(payload, 0)
	if err != nil {
		t.Fatal(err)
	}

	msg := make([]byte, len(b)/8)
	for i, v := range b {
		msg[i/8] |= v << (7 - i%8)
	}

	const rate = 48000.0

	bits := aisBurst(msg)
	n := int(float64(len(bits)) / 9600 * rate)
	r := rand.New(rand.NewPCG(1, 2))

	var (
		iq    []complex64
		phase float64
	)

	for range 3 {
		for i := range n + int(rate/5) {
			if i < n {
				phase += 2 * math.Pi * 2400 * gmsk(bits, float64(i)/rate) / rate
			}

			iq = append(iq, complex64(cmplx.Rect(0.3, phase)+complex(r.NormFloat64()*0.01, r.NormFloat64()*0.01)))
		}
	}

	return iq
}

// AIS bursts through the FM discriminator of the wide IQ and direwolf -B
// AIS, parsed (MAR-001).
func TestAISDecodesBursts(t *testing.T) {
	recs, _, _ := fixtureRun{mode: "ais", iq: aisIQ(t), until: countAtLeast(3), speed: 2}.run(t, "direwolf")

	if len(recs) < 3 {
		t.Fatalf("records = %+v", recs)
	}

	for _, r := range recs {
		var p AISRecord
		if err := json.Unmarshal(r.Payload, &p); err != nil || r.Schema != AISSchema || p.MMSI != "227006760" ||
			p.NMEA != "!AIVDM,1,1,,A,13HOI:001swcL5@KcnL9s7tt0000,0*48" {
			t.Fatalf("record %q %s: %v", r.Text, r.Payload, err)
		}
	}
}
