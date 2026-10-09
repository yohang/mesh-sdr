package decoder

import (
	"encoding/json"
	"fmt"
	"reflect"
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
