package decoder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// ax25 builds an AX.25 UI frame (tests).
func ax25(dst, src string, path []string, info string) []byte {
	addr := func(call string, last, repeated bool) []byte {
		ssid := 0
		h := false

		if n := len(call); n > 0 && call[n-1] == '*' {
			call, h = call[:n-1], true
		}

		for i := 0; i < len(call); i++ {
			if call[i] == '-' {
				for _, c := range call[i+1:] {
					ssid = ssid*10 + int(c-'0')
				}

				call = call[:i]

				break
			}
		}

		b := make([]byte, 7)
		for i := range 6 {
			c := byte(' ')
			if i < len(call) {
				c = call[i]
			}

			b[i] = c << 1
		}

		b[6] = 0x60 | byte(ssid)<<1
		if last {
			b[6] |= 1
		}

		if h || repeated {
			b[6] |= 0x80
		}

		return b
	}

	var f []byte
	f = append(f, addr(dst, false, false)...)
	f = append(f, addr(src, len(path) == 0, false)...)

	for i, p := range path {
		f = append(f, addr(p, i == len(path)-1, false)...)
	}

	f = append(f, 0x03, 0xF0)

	return append(f, info...)
}

func TestParseAX25AndKISS(t *testing.T) {
	frame := ax25("APRS", "F4ABC-9", []string{"F1ZZZ-2*", "WIDE2-1"}, "!4903.50N/00207.50E>")

	// KISS: FEND, port 0 data, escaped FEND/FESC, FEND; a TX command and an
	// empty frame are ignored.
	var kiss bytes.Buffer
	kiss.Write([]byte{kissFEND, 0x00})

	for _, b := range frame {
		switch b {
		case kissFEND:
			kiss.Write([]byte{kissFESC, kissTFEND})
		case kissFESC:
			kiss.Write([]byte{kissFESC, kissTFESC})
		default:
			kiss.WriteByte(b)
		}
	}

	kiss.Write([]byte{kissFEND, kissFEND, 0x01, 0x10, kissFEND})

	var got [][]byte

	if err := readKISSFrames(bufio.NewReader(&kiss), func(f []byte) { got = append(got, bytes.Clone(f)) }); err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || !bytes.Equal(got[0], frame) {
		t.Fatalf("frames = %x", got)
	}

	f, err := parseAX25(got[0])
	if err != nil {
		t.Fatal(err)
	}

	want := AX25Frame{Destination: "APRS", Source: "F4ABC-9", Path: []string{"F1ZZZ-2*", "WIDE2-1"}, Info: []byte("!4903.50N/00207.50E>")}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("frame = %+v", f)
	}

	for _, bad := range [][]byte{{0x82, 0xA0}, append(ax25("APRS", "F4ABC", nil, "x")[:14], 0x3F, 0xF0)} {
		if _, err := parseAX25(bad); err == nil {
			t.Errorf("%x: no error", bad)
		}
	}
}

func TestParseAPRS(t *testing.T) {
	at := time.Date(2026, 10, 9, 0, 0, 5, 0, time.UTC)

	for _, tc := range []struct {
		name      string
		src, dst  string
		path      []string
		info      string
		want      APRSRecord
		forwarded *APRSRecord
	}{
		{
			name: "plain position, course, speed, altitude", src: "F4ABC-9", dst: "APRS", path: []string{"F1ZZZ-2*", "WIDE1*", "WIDE2-1"},
			info: "!4903.50N/00207.50E>088/036/A=001234 mobile",
			want: APRSRecord{
				Type: "position", Key: "F4ABC-9", Hops: []string{"F1ZZZ-2"}, Lat: ptr(49.058333), Lon: ptr(2.125), Symbol: "/>",
				Course: ptr(88), Speed: ptr(66.7), Altitude: ptr(376.1), Comment: "mobile",
			},
		},
		{
			name: "ambiguity, PHG", src: "F4ABC", dst: "APRS", info: "=49  .  N/002  .  E#PHG5132 digi",
			want: APRSRecord{
				Type: "position", Key: "F4ABC", Lat: ptr(49.0), Lon: ptr(2.0), Ambiguity: 4, Symbol: "/#",
				PHG: &APRSPHG{PowerW: 25, HeightM: 6.1, GainDB: 3, Directivity: 90}, Comment: "digi",
			},
		},
		{
			name: "compressed", src: "F4ABC", dst: "APRS", info: "!/5L!!<*e7>7P[",
			want: APRSRecord{Type: "position", Key: "F4ABC", Lat: ptr(49.5), Lon: ptr(-72.750004), Compressed: true, Symbol: "/>", Course: ptr(88), Speed: ptr(67.1)},
		},
		{
			name: "mic-e", src: "F4DEF", dst: "T2SP0W", path: []string{"WIDE1-1"}, info: "`c51!f?>/]\"4a}=",
			want: APRSRecord{
				Type: "mic-e", Key: "F4DEF", Lat: ptr(42.501167), Lon: ptr(-71.420167), Symbol: "/>", Speed: ptr(105.6), Course: ptr(35),
				Altitude: ptr(74.0), MicE: "in service", Comment: "=",
			},
		},
		{
			name: "h timestamp of yesterday", src: "F4ABC", dst: "APRS", info: "/235950h4903.50N/00207.50E-",
			want: APRSRecord{Type: "position", Key: "F4ABC", Timestamp: ptr(time.Date(2026, 10, 8, 23, 59, 50, 0, time.UTC)), Lat: ptr(49.058333), Lon: ptr(2.125), Symbol: "/-"},
		},
		{
			name: "weather", src: "F4WX", dst: "APRS", info: "@082345z4903.50N/00207.50E_220/004g005t077r001p010P020h50b09900wx",
			want: APRSRecord{
				Type: "weather", Key: "F4WX", Timestamp: ptr(time.Date(2026, 10, 8, 23, 45, 0, 0, time.UTC)), Lat: ptr(49.058333), Lon: ptr(2.125), Symbol: "/_",
				Weather: &APRSWeather{
					WindDir: ptr(220), WindKMH: ptr(6.4), GustKMH: ptr(8.0), TempC: ptr(25.0), RainHourMM: ptr(0.3), Rain24hMM: ptr(2.5), RainMidMM: ptr(5.1),
					Humidity: ptr(50), PressureHPa: ptr(990.0),
				},
				Comment: "wx",
			},
		},
		{
			name: "positionless weather", src: "F4WX", dst: "APRS", info: "_10090556c220s004g005t-05",
			want: APRSRecord{Type: "weather", Key: "F4WX", Weather: &APRSWeather{WindDir: ptr(220), WindKMH: ptr(6.4), GustKMH: ptr(8.0), TempC: ptr(-20.6)}},
		},
		{
			name: "status with time", src: "F4XYZ", dst: "APDW17", info: ">082000zNet tonight",
			want: APRSRecord{Type: "status", Key: "F4XYZ", Timestamp: ptr(time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC)), Comment: "Net tonight"},
		},
		{
			name: "message", src: "F4ABC", dst: "APRS", info: ":F4XYZ    :Hello there{42",
			want: APRSRecord{Type: "message", Key: "F4ABC", Addressee: "F4XYZ", Message: "Hello there", MessageID: "42"},
		},
		{
			name: "ack", src: "F4XYZ", dst: "APRS", info: ":F4ABC    :ack42",
			want: APRSRecord{Type: "ack", Key: "F4XYZ", Addressee: "F4ABC", MessageID: "42"},
		},
		{
			name: "rej", src: "F4XYZ", dst: "APRS", info: ":F4ABC    :rej7",
			want: APRSRecord{Type: "rej", Key: "F4XYZ", Addressee: "F4ABC", MessageID: "7"},
		},
		{
			name: "object", src: "F4ABC", dst: "APRS", info: ";LEADER   *092345z4903.50N/00207.50E>088/036",
			want: APRSRecord{
				Type: "object", Key: "LEADER", Name: "LEADER", Live: ptr(true), Timestamp: ptr(time.Date(2026, 10, 9, 23, 45, 0, 0, time.UTC)),
				Lat: ptr(49.058333), Lon: ptr(2.125), Symbol: "/>", Course: ptr(88), Speed: ptr(66.7),
			},
		},
		{
			name: "killed item", src: "F4ABC", dst: "APRS", info: ")AID #2_4903.50N/00207.50EA",
			want: APRSRecord{Type: "item", Key: "AID #2", Name: "AID #2", Live: ptr(false), Lat: ptr(49.058333), Lon: ptr(2.125), Symbol: "/A"},
		},
		{
			name: "nmea", src: "F4GPS", dst: "GPSLV", info: "$GPRMC,063909,A,3349.4302,N,11700.3721,W,43.022,89.3,291099,13.6,E*52",
			want: APRSRecord{Type: "nmea", Key: "F4GPS", Lat: ptr(33.823837), Lon: ptr(-117.006202), Speed: ptr(79.7), Course: ptr(89),
				NMEA: "$GPRMC,063909,A,3349.4302,N,11700.3721,W,43.022,89.3,291099,13.6,E*52"},
		},
		{
			name: "third party", src: "F1GW", dst: "APRS", path: []string{"WIDE2*"}, info: "}F4ABC>APRS,TCPIP,F1GW*:!4903.50N/00207.50E-",
			want: APRSRecord{Type: "thirdparty", Key: "F4ABC", Hops: []string{"F1GW"}},
			forwarded: &APRSRecord{
				Source: "F4ABC", Destination: "APRS", Path: []string{"TCPIP", "F1GW*"}, Hops: []string{"F1GW"}, Type: "position", Key: "F4ABC",
				Lat: ptr(49.058333), Lon: ptr(2.125), Symbol: "/-",
			},
		},
		{
			name: "other", src: "F4ABC", dst: "APRS", info: "T#005,199,000,255,073,123,01101001",
			want: APRSRecord{Type: "other", Key: "F4ABC", Comment: "T#005,199,000,255,073,123,01101001"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAPRS(tc.src, tc.dst, tc.path, tc.info, at, 0)

			want := tc.want
			want.Source, want.Destination, want.Path = tc.src, tc.dst, tc.path

			if want.Path == nil {
				want.Path = []string{}
			}

			if want.Hops == nil {
				want.Hops = []string{}
			}

			want.Forwarded = tc.forwarded

			if !reflect.DeepEqual(*got, want) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(want)
				t.Errorf("\n got %s\nwant %s", g, w)
			}
		})
	}
}

// The "h" timestamp resolves to the day nearest to the reception (DEC-032).
func TestAPRSTimeDay(t *testing.T) {
	for _, tc := range []struct {
		ts   string
		at   time.Time
		want time.Time
	}{
		{"235950h", time.Date(2026, 10, 9, 0, 0, 5, 0, time.UTC), time.Date(2026, 10, 8, 23, 59, 50, 0, time.UTC)},
		{"000010h", time.Date(2026, 10, 8, 23, 59, 58, 0, time.UTC), time.Date(2026, 10, 9, 0, 0, 10, 0, time.UTC)},
		{"120000h", time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
		{"311200z", time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC), time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)},
		{"010030z", time.Date(2026, 12, 31, 23, 50, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 30, 0, 0, time.UTC)},
	} {
		got := aprsTime(tc.ts, tc.at)
		if got == nil || !got.Equal(tc.want) {
			t.Errorf("%s at %s = %v, want %s", tc.ts, tc.at, got, tc.want)
		}
	}

	for _, bad := range []string{"256000h", "320000z", "12345", "1234567", "120000x"} {
		if got := aprsTime(bad, time.Now()); got != nil {
			t.Errorf("%s = %v", bad, got)
		}
	}
}

// A packet record: the TNC2 line as text, control characters removed.
func TestParsePacketRecord(t *testing.T) {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	rec, ok := parsePacket(ax25("APRS", "F4ABC", []string{"WIDE1-1"}, ">Hi\x07<b>\r\n"), at)
	if !ok {
		t.Fatal("not parsed")
	}

	if rec.Text != "F4ABC>APRS,WIDE1-1:>Hi<b>" || rec.Schema != APRSSchema || !rec.Time.Equal(at) {
		t.Errorf("record = %+v", rec)
	}
}
