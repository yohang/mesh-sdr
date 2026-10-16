package mapfeatures

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

var testSettings = Settings{
	PositionRetention: 2 * time.Hour, CallRetention: 5 * time.Minute, MaxCalls: 5, PreferRecentReports: true,
}

func dec(schema, mode, payload string) Decode {
	return Decode{
		NodeID: "n1", DeviceID: "vhf", Mode: mode, Schema: schema, FreqHz: 144_800_000, At: t0, Payload: json.RawMessage(payload),
	}
}

// One Project per mode family: APRS positions and objects, WSJT and JS8
// locators and calls; anything else reports nothing.
func TestProject(t *testing.T) {
	for _, tc := range []struct {
		name     string
		d        Decode
		keys     []string // reported features
		indirect bool
		kills    []string
		calls    []call
		check    func(t *testing.T, f Feature)
	}{
		{
			name: "aprs position",
			d: dec(schemaAPRS, "aprs", `{"source":"F4ABC-9","type":"position","key":"F4ABC-9","hops":[],"lat":50.6,"lon":3.06,
				"symbol":"/>","course":90,"speed_kmh":30.5,"altitude_m":120,"comment":"hi\u0007 there","weather":{"temp_c":12.5}}`),
			keys: []string{"aprs:F4ABC-9@vhf"},
			check: func(t *testing.T, f Feature) {
				t.Helper()

				g := f.Geometry
				if f.Kind != KindAPRS || *f.Lat != 50.6 || *f.Lon != 3.06 || g.Type != GeometryPoint || *g.Lat != 50.6 || f.DeviceID != "vhf" ||
					!f.ExpiresAt.Equal(t0.Add(2*time.Hour)) || !f.UpdatedAt.Equal(t0) || f.Source != SourceDecode {
					t.Errorf("feature %+v", f)
				}

				want := `{"altitude_m":120,"aprs_type":"position","comment":"hi there","course":90,"freq_hz":144800000,"label":"F4ABC-9",` +
					`"mode":"aprs","source":"F4ABC-9","speed_kmh":30.5,"symbol":"/\u003e","weather":{"temp_c":12.5}}`
				if b, _ := json.Marshal(f.Details); string(b) != want {
					t.Errorf("details %s", b)
				}
			},
		},
		{
			name: "aprs digipeated", d: dec(schemaAPRS, "aprs", `{"type":"mic-e","key":"F4ABC-9","hops":["F1ZZZ-1"],"lat":50,"lon":3}`),
			keys: []string{"aprs:F4ABC-9@vhf"}, indirect: true,
		},
		{
			name: "aprs third party", indirect: true, keys: []string{"aprs:F5XYZ@vhf"},
			d: dec(schemaAPRS, "aprs", `{"type":"thirdparty","key":"F5XYZ","hops":["IGATE"],"forwarded":{"type":"position","key":"F5XYZ","hops":[],"lat":45,"lon":5}}`),
		},
		{
			name: "aprs object", keys: []string{"aprs:LEADER@vhf"},
			d: dec(schemaAPRS, "aprs", `{"type":"object","key":"LEADER","name":"LEADER","source":"F4ABC","live":true,"lat":45,"lon":5}`),
		},
		{
			name: "aprs killed item", kills: []string{"aprs:LEADER@vhf"},
			d: dec(schemaAPRS, "aprs", `{"type":"item","key":"LEADER","name":"LEADER","live":false,"lat":45,"lon":5}`),
		},
		{name: "aprs status", d: dec(schemaAPRS, "aprs", `{"type":"status","key":"F4ABC","comment":"on air"}`)},
		{name: "aprs bad position", d: dec(schemaAPRS, "aprs", `{"type":"position","key":"F4ABC","lat":95,"lon":3}`)},
		{name: "aprs no key", d: dec(schemaAPRS, "aprs", `{"type":"position","key":"\u0001","lat":45,"lon":3}`)},
		{name: "aprs garbage", d: dec(schemaAPRS, "aprs", `[1,2]`)},
		{
			name: "wsjt cq", keys: []string{"locator:DL1ABC@vhf"},
			d: dec(schemaWSJT, "ft8", `{"mode":"FT8","msg":"CQ DL1ABC JO62","callsign":"DL1ABC","locator":"JO62","db":-12}`),
			check: func(t *testing.T, f Feature) {
				t.Helper()

				if f.Kind != KindLocator || f.Geometry.Type != GeometryLocator || f.Geometry.Locator != "JO62" || *f.Lat != 52.5 || *f.Lon != 13 ||
					f.Details["callsign"] != "DL1ABC" || f.Details["db"] != -12 || f.Details["mode"] != "ft8" {
					t.Errorf("feature %+v", f)
				}
			},
		},
		{
			name: "wsjt answer with locator", keys: []string{"locator:DL1ABC@vhf"}, calls: []call{{from: "DL1ABC", to: "K1ABC"}},
			d: dec(schemaWSJT, "ft8", `{"msg":"K1ABC DL1ABC JO62","callsign":"DL1ABC","locator":"JO62"}`),
		},
		{
			name: "wsjt rr73", calls: []call{{from: "K1ABC", to: "DL1ABC"}},
			d: dec(schemaWSJT, "ft8", `{"msg":"DL1ABC K1ABC RR73","callsign":"K1ABC","callee":"DL1ABC"}`),
		},
		{
			name: "wspr beacon", keys: []string{"locator:K1ABC@vhf"},
			d: dec(schemaWSJT, "wspr", `{"mode":"WSPR","msg":"K1ABC FN42 33","callsign":"K1ABC","locator":"FN42","dbm":33}`),
		},
		{name: "wsjt bad locator", d: dec(schemaWSJT, "ft8", `{"msg":"CQ DL1ABC ZZ99","callsign":"DL1ABC","locator":"ZZ99"}`)},
		{name: "wsjt bad callsign", d: dec(schemaWSJT, "ft8", `{"msg":"CQ <x> JO62","callsign":"<x>","locator":"JO62"}`)},
		{
			name: "js8 heartbeat", keys: []string{"locator:F4ABC@vhf"},
			d: dec(schemaJS8, "js8", `{"frame":"heartbeat","callsign":"F4ABC","to":"@ALLCALL","locator":"JN18eu","db":-5}`),
			check: func(t *testing.T, f Feature) {
				t.Helper()

				if f.Geometry.Locator != "JN18eu" {
					t.Errorf("feature %+v", f)
				}
			},
		},
		{
			name: "js8 directed", calls: []call{{from: "F4ABC", to: "K1ABC"}},
			d: dec(schemaJS8, "js8", `{"frame":"directed","callsign":"F4ABC","to":"K1ABC","cmd":"SNR?"}`),
		},
		{name: "other schema", d: dec("paging.v1", "pocsag", `{"lat":45,"lon":5}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := project(tc.d, testSettings)

			var keys []string

			for _, r := range p.reports {
				keys = append(keys, r.feature.Key)

				if r.indirect != tc.indirect {
					t.Errorf("%s indirect = %v", r.feature.Key, r.indirect)
				}

				if tc.check != nil {
					tc.check(t, r.feature)
				}
			}

			if !slices.Equal(keys, tc.keys) || !slices.Equal(p.kills, tc.kills) || fmt.Sprint(p.calls) != fmt.Sprint(tc.calls) {
				t.Errorf("keys %v kills %v calls %v, want %v %v %v", keys, p.kills, p.calls, tc.keys, tc.kills, tc.calls)
			}
		})
	}
}

func TestCallSubject(t *testing.T) {
	if callSubject("K1ABC", "DL1ABC") != "DL1ABC>K1ABC" || callSubject("DL1ABC", "K1ABC") != "DL1ABC>K1ABC" {
		t.Error(callSubject("K1ABC", "DL1ABC"))
	}

	if KeyOf(KindCall, "DL1ABC>K1ABC", "vhf") != "call:DL1ABC>K1ABC@vhf" {
		t.Error(KeyOf(KindCall, "DL1ABC>K1ABC", "vhf"))
	}
}
