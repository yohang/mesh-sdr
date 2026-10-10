package decoder

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// testdata/readsb-aircraft.json is the aircraft.json readsb 3.16.17 (the
// image's build) wrote for adsbFrames, modulated at 2.4 MS/s and read
// from stdin as SC16, with --lat 52 --lon 4: an airliner with a DF5
// squawk, an emergency (7700) and an aircraft on the ground.

// adsbFrames are Mode S frames of three aircraft: 3C6586 (DLH9LF, A3,
// airborne position pair at 35000 ft, velocity, DF5 squawk 1000), 4CA1FA
// (EIN5AT, A2, emergency 7700, position pair at 12000 ft, velocity) and
// A4F3B2 (N123AB, A1, surface position pair).
var adsbFrames = []string{
	"8D3C65862310C239306820E94245", "8D3C658658B502BBBCDC296FC266", "8D3C658658B506278AD60B0B25DB", "8D3C658699092D99284400B92E47",
	"280008006C7148",
	"8D4CA1FA221493B505482028DE4D", "8D4CA1FAE12AAA00000000CA30C7", "8D4CA1FA584182999AC28F483F8F", "8D4CA1FA58418605FABD283B7892",
	"8D4CA1FA990C9712E06400EACFF3",
	"8DA4F3B2213B1CB3042820AD97F9", "8DA4F3B238AA02B17F353F25C29F", "8DA4F3B238AA0461BD1E70B5C11C",
}

func readsbFixture(t *testing.T) []byte {
	t.Helper()

	b, err := os.ReadFile("testdata/readsb-aircraft.json")
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// An aircraft is reported when its message count grew; its record is its
// whole state, its time the poll time minus its age.
func TestAircraftPollerFixture(t *testing.T) {
	var p aircraftPoller

	parse := p.read
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	data := readsbFixture(t)

	recs, progress := parse(data, at)
	if !progress || len(recs) != 3 {
		t.Fatalf("first read: progress %v, %d records", progress, len(recs))
	}

	byICAO := map[string]AircraftRecord{}

	for _, r := range recs {
		var a AircraftRecord
		if err := json.Unmarshal(r.Payload, &a); err != nil || r.Schema != AircraftSchema {
			t.Fatalf("record %+v: %v", r, err)
		}

		byICAO[a.ICAO] = a
	}

	ein := byICAO["4CA1FA"]
	if ein.Flight != "EIN5AT" || ein.Squawk != "7700" || ein.Emergency != "general" || ein.Category != "A2" || ein.AddressType != "adsb_icao" ||
		*ein.AltitudeFt != 12000 || *ein.SpeedKt != 212.1 || *ein.TrackDeg != 315 || *ein.VerticalRate != 1536 || *ein.Lat != 51.900009 ||
		*ein.Lon != 3.799973 || ein.Messages != 37 || *ein.RSSI != -16.3 || ein.Ground {
		t.Errorf("4CA1FA = %+v", ein)
	}

	if n := byICAO["A4F3B2"]; !n.Ground || n.AltitudeFt != nil || n.Flight != "N123AB" || n.Lat == nil {
		t.Errorf("A4F3B2 = %+v", n)
	}

	if d := byICAO["3C6586"]; d.Squawk != "1000" || d.Emergency != "" || *d.AltitudeFt != 35000 || *d.VerticalRate != -1024 {
		t.Errorf("3C6586 = %+v", d)
	}

	i := slices.IndexFunc(recs, func(r app.DecodeRecord) bool { return strings.HasPrefix(r.Text, "4CA1FA") })
	if r := recs[i]; r.Text != "4CA1FA EIN5AT 12000 ft 51.90001,3.79997 squawk 7700 emergency general" || !r.Time.Equal(at.Add(-100*time.Millisecond)) {
		t.Errorf("4CA1FA record: %q at %v", r.Text, r.Time)
	}

	// The same file again: nothing new, no progress.
	if recs, progress := parse(data, at.Add(time.Second)); progress || len(recs) != 0 {
		t.Errorf("same file: progress %v, %d records", progress, len(recs))
	}

	// One aircraft heard again, its position not (a record carries a
	// position only when it is new), the clock advanced.
	next := strings.NewReplacer(`"now" : 1791625971.003`, `"now" : 1791625972.003`, `"seen_pos":0.000,`, `"seen_pos":1.000,`,
		`"messages":17,`, `"messages":18,`).Replace(string(data))
	if recs, progress := parse([]byte(next), at.Add(2*time.Second)); !progress || len(recs) != 1 || recs[0].Text != "A4F3B2 N123AB ground" {
		t.Errorf("next file: progress %v, records %+v", progress, recs)
	}

	// A file caught mid-write.
	if recs, progress := parse(data[:len(data)/2], at); progress || recs != nil {
		t.Errorf("partial file: %v %+v", progress, recs)
	}
}

// Untrusted readsb values are validated: no address, no record; junk
// fields are dropped.
func TestAircraftRecordValidates(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
		want AircraftRecord
	}{
		{"no address", `{"hex":"zz1234","messages":3}`, false, AircraftRecord{}},
		{"no messages", `{"hex":"4ca1fa","messages":0}`, false, AircraftRecord{}},
		{"tis-b", `{"hex":"~4ca1fa","messages":1}`, true, AircraftRecord{ICAO: "~4CA1FA", Messages: 1}},
		{
			"junk", `{"hex":"4ca1fa","messages":2,"flight":"<b>\u0007","squawk":"7800","category":"Z9","emergency":"panic","type":"x y",` +
				`"lat":0,"lon":0,"alt_baro":"air","gs":-3,"track":400}`,
			true, AircraftRecord{ICAO: "4CA1FA", Messages: 2},
		},
		{"out of range", `{"hex":"4ca1fa","messages":2,"lat":91,"lon":4,"alt_baro":250000}`, true, AircraftRecord{ICAO: "4CA1FA", Messages: 2}},
	}

	for _, c := range cases {
		var a readsbAircraft
		if err := json.Unmarshal([]byte(c.in), &a); err != nil {
			t.Fatal(err)
		}

		got, ok := aircraftRecord(a)
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(c.want)

		if ok != c.ok || string(g) != string(w) {
			t.Errorf("%s: %v %s, want %v %s", c.name, ok, g, c.ok, w)
		}
	}
}

// readsb gets the receiver position when the node knows it.
func TestReadsbArgs(t *testing.T) {
	base := "--device-type ifile --ifile - --iformat SC16 --quiet --write-json adsb --write-json-every 1"

	if got := strings.Join(readsbArgs(sessionConfig{}), " "); got != base {
		t.Errorf("no position: %s", got)
	}

	c := sessionConfig{position: func() (float64, float64, bool) { return 48.8566, 2.3522, true }}
	if got := strings.Join(readsbArgs(c), " "); got != base+" --lat 48.85660 --lon 2.35220" {
		t.Errorf("position: %s", got)
	}

	c.position = func() (float64, float64, bool) { return 0, 0, false }
	if got := strings.Join(readsbArgs(c), " "); got != base {
		t.Errorf("unknown position: %s", got)
	}
}

// adsbIQ modulates frames (PPM, 0.5 µs pulses after the 8 µs preamble)
// into one second of 2.4 MS/s IQ, one frame every 50 ms, cycling; no
// frames: noise only.
func adsbIQ(t *testing.T, frames []string) []complex64 {
	t.Helper()

	const (
		rate = domain.ADSBRate
		slot = rate / 20
		amp  = 0.25
		dt   = 1e6 / float64(rate) // µs per sample
	)

	rnd := rand.New(rand.NewPCG(1, 2))
	out := make([]complex64, rate)

	for i := range out {
		out[i] = complex(float32(rnd.NormFloat64()*0.002), float32(rnd.NormFloat64()*0.002))
	}

	for k := range 20 {
		if len(frames) == 0 {
			break
		}

		msg, err := hex.DecodeString(frames[k%len(frames)])
		if err != nil {
			t.Fatal(err)
		}

		pulses := [][2]float64{{0, .5}, {1, 1.5}, {3.5, 4}, {4.5, 5}}

		for i := range len(msg) * 8 {
			s := 8 + float64(i)
			if msg[i/8]>>(7-i%8)&1 == 0 {
				s += .5
			}

			pulses = append(pulses, [2]float64{s, s + .5})
		}

		start := float64(k*slot)*dt + 1000
		phase := rnd.Float64() * 2 * math.Pi
		rot := complex(float32(math.Cos(phase)), float32(math.Sin(phase)))

		for _, p := range pulses {
			a, b := start+p[0], start+p[1]

			for n := int(a / dt); n <= int(b/dt)+1; n++ {
				lo := float64(n) * dt
				if ov := min(lo+dt, b) - max(lo, a); ov > 0 {
					out[n] += complex(float32(amp*ov/dt), 0) * rot
				}
			}
		}
	}

	return out
}

// ADS-B through readsb (AIR-004): the wide IQ as SC16 on stdin, the
// aircraft list polled from the workdir.
// The second run starts with 4 s of noise only: readsb starts on input
// without any message.
func TestADSBDecodesReadsb(t *testing.T) {
	if _, err := process.ResolveTool("readsb", dirs); err != nil {
		t.Skip("readsb not installed")
	}

	t.Run("signal", func(t *testing.T) { runADSB(t, 0) })
	t.Run("quiet start", func(t *testing.T) { runADSB(t, 4*time.Second) })
}

// runADSB feeds quiet of noise, then the frames in real time, to an ADS-B
// session until the three aircraft are reported.
func runADSB(t *testing.T, quiet time.Duration) {
	t.Helper()

	mode, err := domain.DigitalModeOf("adsb")
	if err != nil {
		t.Fatal(err)
	}

	r := NewRunner(Options{
		Supervisor: supervisor(t), Tools: process.Tools{Dirs: dirs},
		Position: func(device string) (float64, float64, bool) { return 52, 4, device == "dev" },
	})

	var ev events

	run, err := r.Start(app.DecoderSpec{Session: shared.MustParseUUID("0190c8a4-0000-7000-8000-0000000000e5"), Mode: mode, Device: "dev"}, ev.sink())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(run.Close)

	iq := adsbIQ(t, adsbFrames)
	noise := adsbIQ(t, nil)
	block := mode.InputRate / 50

	// Positions need both CPR frames of each aircraft: wait for the three
	// aircraft, two of them placed.
	done := func() bool {
		ev.mu.Lock()
		defer ev.mu.Unlock()

		got := map[string]bool{}

		for _, rec := range ev.decodes {
			var a AircraftRecord
			if json.Unmarshal(rec.Payload, &a) == nil && (a.Lat != nil || a.ICAO == "3C6586") {
				got[a.ICAO] = true
			}
		}

		return len(got) == 3
	}

	start := time.Now()
	deadline := start.Add(quiet + 20*time.Second)

	for i := 0; time.Now().Before(deadline) && !done(); i += block {
		i %= len(iq)

		in := iq
		if time.Since(start) < quiet {
			in = noise
		}

		run.WideIQ(app.WideIQBlock{Samples: in[i:min(i+block, len(in))], Rate: mode.InputRate, Time: time.Now()})
		time.Sleep(20 * time.Millisecond)
	}

	if !done() {
		t.Fatalf("decodes = %v", ev.texts())
	}

	ev.mu.Lock()
	defer ev.mu.Unlock()

	if len(ev.statuses) == 0 || ev.statuses[0].State != app.DecoderRunning {
		t.Errorf("statuses = %+v", ev.statuses)
	}

	if !slices.ContainsFunc(ev.decodes, func(r app.DecodeRecord) bool { return strings.HasPrefix(r.Text, "4CA1FA EIN5AT 12000 ft 51.9") }) {
		t.Errorf("decodes = %v", ev.texts())
	}
}
