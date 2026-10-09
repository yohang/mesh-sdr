package decoder

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp/csdr"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// Fixtures (testdata): aprs.wav and eas-tor.wav are made with direwolf's
// gen_packets (12 kHz AFSK 1200; three 11 025 Hz SAME bursts),
// pocsag1200.wav, cw-cq-f4abc.wav and rtty-cq-f4abc.wav are synthetic
// (POCSAG 1200 NRZ baseband at 22 050 Hz, "CQ CQ DE F4ABC F4ABC K" at 20
// WPM on 1 kHz, ITA2 45.45 Bd 170 Hz shift, both at 8 kHz). The ISM signal
// is synthesised by the test (a Nexus-TH sensor, OOK PPM).

// fixtureRun feeds a WAV fixture as audio, or IQ, to a session of mode and
// returns its records once until says it has enough, or after the
// deadline.
type fixtureRun struct {
	mode    string
	variant string
	wav     string
	// iq, when set, is fed instead of the WAV (a wide IQ mode, at the
	// mode's input rate).
	iq    []complex64
	opts  Options
	dial  func() int64
	until func(recs []app.DecodeRecord) bool
	// speed is the feed speed (times real time, default 10): the 2 s
	// input buffer drops the oldest data of a producer faster than the
	// tool.
	speed int
}

func (f fixtureRun) run(t *testing.T, tool string) ([]app.DecodeRecord, app.DecoderRun, *events) {
	t.Helper()

	if _, err := process.ResolveTool(tool, dirs); err != nil {
		t.Skip(tool + " not installed")
	}

	f.opts.Supervisor = supervisor(t)
	f.opts.Tools = process.Tools{Dirs: dirs}
	r := NewRunner(f.opts)

	mode, err := domain.DigitalModeOf(f.mode)
	if err != nil {
		t.Fatal(err)
	}

	var ev events

	spec := app.DecoderSpec{Session: shared.MustParseUUID("0190c8a4-0000-7000-8000-0000000000e4"), Mode: mode, Variant: f.variant, Dial: f.dial}

	run, err := r.Start(spec, ev.sink())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(run.Close)

	// The tool starts while a live input would fill the 2 s buffer.
	time.Sleep(time.Second)

	if f.speed == 0 {
		f.speed = 10
	}

	// 20 ms blocks, like the demodulator.
	pace := 20 * time.Millisecond / time.Duration(f.speed)

	if f.iq != nil {
		block := mode.InputRate / 50
		iq := append(slices.Clone(f.iq), make([]complex64, mode.InputRate)...)

		for i := 0; i < len(iq); i += block {
			run.WideIQ(app.WideIQBlock{Samples: iq[i:min(i+block, len(iq))], Rate: mode.InputRate, Time: time.Now()})
			time.Sleep(pace)
		}
	} else {
		samples, rate := readWAV(t, f.wav)
		block := rate / 50
		samples = append(samples, make([]float32, rate)...)

		for i := 0; i < len(samples); i += block {
			run.Audio(app.AudioBlock{Samples: samples[i:min(i+block, len(samples))], Rate: rate, Time: time.Now()})
			time.Sleep(pace)
		}
	}

	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		ev.mu.Lock()
		recs := slices.Clone(ev.decodes)
		ev.mu.Unlock()

		if f.until(recs) {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	ev.mu.Lock()
	defer ev.mu.Unlock()

	if len(ev.statuses) == 0 || ev.statuses[0].State != app.DecoderRunning {
		t.Errorf("statuses = %+v", ev.statuses)
	}

	return slices.Clone(ev.decodes), run, &ev
}

func countAtLeast(n int) func([]app.DecodeRecord) bool {
	return func(recs []app.DecodeRecord) bool { return len(recs) >= n }
}

// Packets through direwolf and its KISS pseudo-terminal (DEC-031), parsed
// as APRS (DEC-032).
func TestPacketDecodesFixture(t *testing.T) {
	recs, _, _ := fixtureRun{mode: "packet", wav: "aprs.wav", until: countAtLeast(5)}.run(t, "direwolf")

	var texts, types []string

	for _, r := range recs {
		var p APRSRecord
		if err := json.Unmarshal(r.Payload, &p); err != nil || r.Schema != APRSSchema {
			t.Fatalf("record %+v: %v", r, err)
		}

		texts = append(texts, r.Text)
		types = append(types, p.Type)
	}

	wantTexts := []string{
		"F4ABC-9>APRS,WIDE1-1,WIDE2-1:!4903.50N/00207.50E>088/036/A=001234 mobile",
		"F4XYZ>APDW17:>Net tonight 2000z",
		"F4ABC>APRS::F4XYZ    :Hello there{42",
		"F4DEF>T2SP0W,WIDE1-1:`c51!f?>/]\"4a}=",
		"F4WX>APRS:@092345z4903.50N/00207.50E_220/004g005t077r000p000P000h50b09900wx",
	}

	if !slices.Equal(texts, wantTexts) || !slices.Equal(types, []string{"position", "status", "message", "mic-e", "weather"}) {
		t.Errorf("texts %q, types %q", texts, types)
	}
}

// POCSAG through multimon-ng (DEC-033).
func TestPagingDecodesFixture(t *testing.T) {
	opts := Options{Settings: func() Settings { return Settings{PagingFilter: true} }}
	recs, _, _ := fixtureRun{mode: "page", wav: "pocsag1200.wav", opts: opts, until: countAtLeast(1)}.run(t, "multimon-ng")

	if len(recs) != 1 || recs[0].Text != "POCSAG1200 1234567/3 Alpha: Hello MeshSDR" {
		t.Errorf("records = %+v", recs)
	}
}

// A SAME header through multimon-ng (DEC-036).
func TestEASDecodesFixture(t *testing.T) {
	recs, _, _ := fixtureRun{mode: "eas", wav: "eas-tor.wav", until: countAtLeast(1)}.run(t, "multimon-ng")

	if len(recs) != 1 || !strings.HasPrefix(recs[0].Text, "ZCZC-WXR-TOR-029095-029037+0030-2811700-KEAX/NWS-\nTornado Warning from National Weather Service for Jackson, Missouri; Cass, Missouri") {
		t.Errorf("records = %+v", recs)
	}
}

// skimmerIQ turns an audio fixture into the wide IQ of a skimmer: the
// audio, resampled to 96 kHz, as the real part (the band above the dial).
func skimmerIQ(t *testing.T, wav string) []complex64 {
	t.Helper()

	samples, rate := readWAV(t, wav)

	rs, err := csdr.NewResampler(float64(rate), skimmerRate)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()

	out := make([]float32, len(samples)*skimmerRate/rate+4096)

	n, err := rs.Process(samples, out)
	if err != nil {
		t.Fatal(err)
	}

	iq := make([]complex64, n)
	for i, v := range out[:n] {
		iq[i] = complex(v, 0)
	}

	return iq
}

// The CW skimmer decodes the call on the real part of the wide IQ, spots
// it (DEC-013, DEC-015) and saves its text log into Files when the session
// ends.
func TestCWSkimmerDecodesFixture(t *testing.T) {
	// The tone also shows in the neighbouring bins of the skimmer, where
	// it decodes worse.
	spotted := func(recs []app.DecodeRecord) bool {
		return slices.ContainsFunc(recs, func(r app.DecodeRecord) bool { return !r.Live && strings.Contains(r.Text, "F4ABC (France)") })
	}
	dial := func() int64 { return 14_020_000 }

	recs, run, ev := fixtureRun{mode: "cwskimmer", iq: skimmerIQ(t, "cw-cq-f4abc.wav"), dial: dial, until: spotted, speed: 2}.run(t, "csdr-cwskimmer")

	var spots []SkimmerRecord

	for _, r := range recs {
		if r.Live {
			continue
		}

		var p SkimmerRecord
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatal(err)
		}

		spots = append(spots, p)
	}

	if !slices.ContainsFunc(spots, func(p SkimmerRecord) bool {
		return p.Callsign == "F4ABC" && p.Country == "France" && p.Mode == "CW" && p.OffsetHz >= 900 && p.OffsetHz <= 1100
	}) {
		t.Errorf("spots = %+v", spots)
	}

	// The log is saved once the tool has exited: one line per frequency,
	// the decoded call on the 1 kHz one.
	run.Close()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		ev.mu.Lock()
		n := len(ev.files)
		ev.mu.Unlock()

		if n > 0 {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	ev.mu.Lock()
	defer ev.mu.Unlock()

	if len(ev.files) != 1 || ev.files[0].Kind != app.FileTextLog || ev.files[0].FreqHz != 14_020_000 ||
		!strings.Contains(string(ev.files[0].Data), " 14021000 CQ CQ DE F4ABC") {
		t.Errorf("text logs = %d, %q", len(ev.files), func() string {
			if len(ev.files) == 0 {
				return ""
			}
			return string(ev.files[0].Data)
		}())
	}
}

// The RTTY skimmer decodes the 45.45 Bd signal (DEC-014).
func TestRTTYSkimmerDecodesFixture(t *testing.T) {
	decoded := func(recs []app.DecodeRecord) bool {
		var all strings.Builder
		for _, r := range recs {
			all.WriteString(r.Text)
		}

		return strings.Contains(all.String(), "CQ")
	}

	recs, _, _ := fixtureRun{mode: "rttyskimmer", iq: skimmerIQ(t, "rtty-cq-f4abc.wav"), until: decoded, speed: 2}.run(t, "csdr-rttyskimmer")

	if !decoded(recs) {
		t.Errorf("records = %+v", recs)
	}

	for _, r := range recs {
		if r.Live && (r.AudioHz < 1000 || r.AudioHz > 1300) {
			t.Errorf("offset of %+v", r)
		}
	}
}

// nexusIQ synthesises ten repeats of a Nexus-TH message (id 177, channel 1,
// 21.5 °C, 45 %) as OOK PPM at 250 kHz, 20 kHz above the dial: 500 µs
// pulses, 1 ms gaps for 0, 2 ms for 1, 4 ms between repeats.
func nexusIQ() []complex64 {
	const rate = 250_000

	bits := []byte{}
	for _, b := range []byte{0xB1, 0x80, 0xD7, 0xF2} {
		for i := 7; i >= 0; i-- {
			bits = append(bits, b>>i&1)
		}
	}

	bits = append(bits, 1, 1, 0, 1)

	us := func(n int) int { return n * rate / 1_000_000 }
	rng := rand.New(rand.NewPCG(1, 2))

	var (
		iq    []complex64
		phase float64
	)

	emit := func(n int, on bool) {
		for range n {
			phase += 2 * math.Pi * 20_000 / rate

			v := complex(0.01*rng.NormFloat64(), 0.01*rng.NormFloat64())
			if on {
				v += complex(0.5*math.Cos(phase), 0.5*math.Sin(phase))
			}

			iq = append(iq, complex64(v))
		}
	}

	emit(us(20_000), false)

	for range 10 {
		for _, b := range bits {
			emit(us(500), true)

			if b == 1 {
				emit(us(2000), false)
			} else {
				emit(us(1000), false)
			}
		}

		emit(us(500), true)
		emit(us(4000), false)
	}

	emit(us(100_000), false)

	return iq
}

// A sensor through rtl_433 on the wide IQ at 250 kHz (DEC-039).
func TestISMDecodesSensor(t *testing.T) {
	nexus := func(recs []app.DecodeRecord) bool {
		return slices.ContainsFunc(recs, func(r app.DecodeRecord) bool { return strings.HasPrefix(r.Text, "Nexus-TH") })
	}

	recs, _, _ := fixtureRun{mode: "ism", iq: nexusIQ(), until: nexus}.run(t, "rtl_433")

	i := slices.IndexFunc(recs, func(r app.DecodeRecord) bool { return strings.HasPrefix(r.Text, "Nexus-TH") })
	if i < 0 {
		t.Fatalf("records = %+v", recs)
	}

	if !strings.Contains(recs[i].Text, "id=177") || !strings.Contains(recs[i].Text, "temperature_C=21.5") || !strings.Contains(string(recs[i].Payload), `"mode":"ISM"`) ||
		strings.Contains(string(recs[i].Payload), `"rssi"`) {
		t.Errorf("record %q %s", recs[i].Text, recs[i].Payload)
	}
}
