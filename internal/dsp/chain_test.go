package dsp

import (
	"errors"
	"math"
	"math/cmplx"
	"math/rand/v2"
	"testing"
)

const goldenFS = 2_400_000

// amSignal is a carrier at freq Hz, amplitude-modulated (depth m) by a
// tone.
func amSignal(n int, fs, freq, toneHz, m float64) []complex64 {
	out := make([]complex64, n)
	for i := range out {
		t := float64(i) / fs
		out[i] = complex64(cmplx.Rect(0.2*(1+m*math.Sin(2*math.Pi*toneHz*t)), 2*math.Pi*freq*t))
	}

	return out
}

func addIQ(a, b []complex64) []complex64 {
	for i := range a {
		a[i] += b[i]
	}

	return a
}

// runChain channelizes iq at offset, runs the chain over it in 20 ms
// blocks and returns the audio and the last result.
func runChain(t *testing.T, cfg ChainConfig, iq []complex64, minRate, offset float64) ([]float32, Result, *Chain) {
	t.Helper()

	p, err := NewChannelPlan(goldenFS, NarrowChannelRate)
	if err != nil {
		t.Fatal(err)
	}

	low, high := cfg.LowHz, cfg.HighHz
	chIQ, ch := channelOut(t, p, iq, minRate, offset, low, high)

	cfg.ChannelRate, cfg.ResidualHz = ch.Rate(), ch.Residual()

	c, err := NewChain(cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(c.Close)

	block := int(ch.Rate() / 50)

	var (
		audio []float32
		res   Result
	)

	for off := 0; off < len(chIQ); off += block {
		res, err = c.Process(chIQ[off:min(off+block, len(chIQ))])
		if err != nil {
			t.Fatal(err)
		}

		audio = append(audio, res.Audio...)
	}

	return audio, res, c
}

func rms(a []float32) float64 {
	var s float64
	for _, v := range a {
		s += float64(v) * float64(v)
	}

	return math.Sqrt(s / float64(max(len(a), 1)))
}

// TestChainGolden feeds a synthetic signal per mode through the
// channelizer and the chain: the audio is the modulating tone, at a sane
// level.
func TestChainGolden(t *testing.T) {
	const (
		offset = -150_000.0
		n      = goldenFS * 6 / 10
	)

	cases := []struct {
		name      string
		cfg       ChainConfig
		minRate   float64
		signal    []complex64
		toneHz    float64
		minEnergy float64
		minRMS    float64
	}{
		{
			name:   "am",
			cfg:    ChainConfig{Demod: DemodAM, OutputRate: DefaultOutputRate, LowHz: -5000, HighHz: 5000, AGC: AGCSlow},
			signal: amSignal(n, goldenFS, offset, 1000, 0.5), toneHz: 1000, minEnergy: 0.8, minRMS: 0.05,
		},
		{
			// The carrier is 25 Hz off: the PLL pulls it in.
			name:   "sam",
			cfg:    ChainConfig{Demod: DemodSAM, OutputRate: DefaultOutputRate, LowHz: -5000, HighHz: 5000, AGC: AGCSlow},
			signal: amSignal(n, goldenFS, offset+25, 1000, 0.5), toneHz: 1000, minEnergy: 0.8, minRMS: 0.05,
		},
		{
			name:   "nfm",
			cfg:    ChainConfig{Demod: DemodNFM, OutputRate: DefaultOutputRate, LowHz: -6000, HighHz: 6000, AGC: AGCSlow},
			signal: fmSignal(n, goldenFS, offset, 1000, 2500), toneHz: 1000, minEnergy: 0.5, minRMS: 0.05,
		},
		{
			// Upper sideband tone at +1 kHz; a tone in the lower sideband
			// is rejected.
			name: "usb",
			cfg:  ChainConfig{Demod: DemodSSB, OutputRate: DefaultOutputRate, LowHz: 300, HighHz: 2700, AGC: AGCFast},
			signal: addIQ(toneAt(0, n, goldenFS, offset+1000, 0.1, 0, nil),
				toneAt(0, n, goldenFS, offset-1500, 0.1, 0, nil)),
			toneHz: 1000, minEnergy: 0.9, minRMS: 0.1,
		},
		{
			name: "lsb",
			cfg:  ChainConfig{Demod: DemodSSB, OutputRate: DefaultOutputRate, LowHz: -2700, HighHz: -300, AGC: AGCFast},
			signal: addIQ(toneAt(0, n, goldenFS, offset-1200, 0.1, 0, nil),
				toneAt(0, n, goldenFS, offset+1500, 0.1, 0, nil)),
			toneHz: 1200, minEnergy: 0.9, minRMS: 0.1,
		},
		{
			// CW: the carrier 800 Hz above the dial, in a narrow band
			// around the tone.
			name:   "cw",
			cfg:    ChainConfig{Demod: DemodSSB, OutputRate: DefaultOutputRate, LowHz: 650, HighHz: 950, AGC: AGCFast},
			signal: addIQ(toneAt(0, n, goldenFS, offset+800, 0.05, 0, nil), toneAt(0, n, goldenFS, offset+1500, 0.05, 0, nil)),
			toneHz: 800, minEnergy: 0.9, minRMS: 0.1,
		},
		{
			name:    "wfm",
			cfg:     ChainConfig{Demod: DemodWFM, OutputRate: 48000, LowHz: -90000, HighHz: 90000, AGC: AGCOff},
			minRate: WideChannelRate,
			signal:  fmSignal(n, goldenFS, offset, 1000, 50000), toneHz: 1000, minEnergy: 0.9, minRMS: 0.05,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			minRate := tc.minRate
			if minRate == 0 {
				minRate = NarrowChannelRate
			}

			audio, res, _ := runChain(t, tc.cfg, tc.signal, minRate, offset)

			rate := float64(tc.cfg.OutputRate)
			// 0.6 s minus the chain delays.
			if got := float64(len(audio)) / rate; got < 0.45 || got > 0.61 {
				t.Fatalf("got %.3f s of audio", got)
			}

			if !res.Open || res.LevelDB < -40 || res.LevelDB > 0 {
				t.Fatalf("level %g dB open %v", res.LevelDB, res.Open)
			}

			tail := audio[len(audio)/2:]
			if e := toneEnergy(tail, rate, tc.toneHz); e < tc.minEnergy {
				t.Fatalf("%g Hz holds %.2f of the audio energy", tc.toneHz, e)
			}

			if r := rms(tail); r < tc.minRMS || r > 1 {
				t.Fatalf("audio RMS %g", r)
			}
		})
	}
}

func TestChainSquelch(t *testing.T) {
	const offset = 100_000.0

	sq := -60.0
	iq := amSignal(goldenFS/5, goldenFS, offset, 1000, 0.5)
	_, res, c := runChain(t, ChainConfig{Demod: DemodAM, OutputRate: DefaultOutputRate, LowHz: -5000, HighHz: 5000, Squelch: &sq}, iq, NarrowChannelRate, offset)

	if !res.Open {
		t.Fatalf("squelch closed at %g dB", res.LevelDB)
	}

	// Squelch above the level: silence.
	high := -1.0
	if err := c.SetSquelch(&high); err != nil {
		t.Fatal(err)
	}

	block := make([]complex64, 480)
	for i := range block {
		block[i] = 0.2
	}

	res, err := c.Process(block)
	if err != nil || res.Open {
		t.Fatalf("squelch: %v open %v", err, res.Open)
	}

	for _, v := range res.Audio {
		if v != 0 {
			t.Fatal("squelched audio is not silence")
		}
	}

	bad := 3.0
	if err := c.SetSquelch(&bad); !errors.Is(err, ErrChain) {
		t.Fatal(err)
	}
}

// TestChainNR: a tone in white noise holds more of the audio energy with
// the noise filter on; it is switched on and off live.
func TestChainNR(t *testing.T) {
	const offset = 50_000.0

	rng := rand.New(rand.NewPCG(1, 2))
	iq := toneAt(0, goldenFS*6/10, goldenFS, offset+1000, 0.02, 0.4, rng)
	cfg := ChainConfig{Demod: DemodSSB, OutputRate: DefaultOutputRate, LowHz: 300, HighHz: 2700, AGC: AGCFast}

	off, _, _ := runChain(t, cfg, iq, NarrowChannelRate, offset)

	cfg.NR = NR{Enabled: true, ThresholdDB: 10}
	on, _, c := runChain(t, cfg, iq, NarrowChannelRate, offset)

	eOff := toneEnergy(off[len(off)/2:], DefaultOutputRate, 1000)
	eOn := toneEnergy(on[len(on)/2:], DefaultOutputRate, 1000)

	if eOn < eOff*1.2 {
		t.Fatalf("tone energy %.3f with NR, %.3f without", eOn, eOff)
	}

	for _, nr := range []NR{{Enabled: true, ThresholdDB: -5}, {}, {Enabled: true}} {
		if err := c.SetNR(nr); err != nil {
			t.Fatal(err)
		}

		if c.Config().NR != nr {
			t.Fatalf("nr %+v not applied", nr)
		}

		if _, err := c.Process(make([]complex64, 480)); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.SetNR(NR{Enabled: true, ThresholdDB: 30}); !errors.Is(err, ErrChain) {
		t.Fatal(err)
	}
}

// TestChainNRMidStream switches the noise filter on, then off, while a
// tone streams through: the audio keeps flowing (the filter's held block
// is dropped when it goes) and the tone survives each switch.
func TestChainNRMidStream(t *testing.T) {
	const offset = 70_000.0

	p, err := NewChannelPlan(goldenFS, NarrowChannelRate)
	if err != nil {
		t.Fatal(err)
	}

	chIQ, ch := channelOut(t, p, toneAt(0, goldenFS*9/10, goldenFS, offset+1000, 0.05, 0, nil), NarrowChannelRate, offset, 300, 2700)

	c, err := NewChain(ChainConfig{
		Demod: DemodSSB, ChannelRate: ch.Rate(), OutputRate: DefaultOutputRate, ResidualHz: ch.Residual(), LowHz: 300, HighHz: 2700, AGC: AGCFast,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	block := int(ch.Rate() / 50)
	thirds := [3][]float32{}

	for off := 0; off < len(chIQ); off += block {
		third := min(3*off/len(chIQ), 2)

		switch {
		case third == 1 && c.Config().NR == (NR{}):
			err = c.SetNR(NR{Enabled: true, ThresholdDB: 3})
		case third == 2 && c.Config().NR.Enabled:
			err = c.SetNR(NR{})
		}

		if err != nil {
			t.Fatal(err)
		}

		res, err := c.Process(chIQ[off:min(off+block, len(chIQ))])
		if err != nil {
			t.Fatal(err)
		}

		thirds[third] = append(thirds[third], res.Audio...)
	}

	for i, a := range thirds {
		// 0.3 s each, minus the delays and the dropped NR block.
		if got := float64(len(a)) / DefaultOutputRate; got < 0.2 || got > 0.33 {
			t.Fatalf("third %d: %.3f s of audio", i, got)
		}

		if e := toneEnergy(a[len(a)/2:], DefaultOutputRate, 1000); e < 0.8 || rms(a[len(a)/2:]) < 0.1 {
			t.Fatalf("third %d: tone energy %.2f, RMS %.3f", i, e, rms(a[len(a)/2:]))
		}
	}
}

func TestChainConfigValidation(t *testing.T) {
	bad := 5.0

	for _, c := range []ChainConfig{
		{Demod: DemodNFM, ChannelRate: 24000, OutputRate: 9000},
		{Demod: DemodNFM, ChannelRate: 1000, OutputRate: 12000},
		{Demod: DemodNFM, ChannelRate: 24000, OutputRate: 12000, Squelch: &bad},
		{Demod: DemodNFM, ChannelRate: 24000, OutputRate: 12000, AGC: "medium"},
		{Demod: "dmr", ChannelRate: 24000, OutputRate: 12000},
		{Demod: DemodWFM, ChannelRate: 240000, OutputRate: 12000},
		{Demod: DemodWFM, ChannelRate: 24000, OutputRate: 48000},
		{Demod: DemodSSB, ChannelRate: 24000, OutputRate: 12000, LowHz: 2700, HighHz: 300},
		{Demod: DemodAM, ChannelRate: 24000, OutputRate: 12000, NR: NR{Enabled: true, ThresholdDB: -21}},
		{Demod: DemodSSB, ChannelRate: 24000, OutputRate: 12000, LowHz: math.NaN(), HighHz: 300},
		{Demod: DemodNFM, ChannelRate: math.Inf(1), OutputRate: 12000},
		{Demod: DemodNFM, ChannelRate: 24000, OutputRate: 12000, ResidualHz: math.NaN()},
	} {
		if _, err := NewChain(c); !errors.Is(err, ErrChain) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
}

// TestDeemphasis: the one-pole filter is about 3 dB down at 1/(2πτ) (the
// discrete pole is a little steeper at 48 kHz), and the
// time constant changes live on a WFM chain.
func TestDeemphasis(t *testing.T) {
	gain := func(us int, freq float64) float64 {
		d := newDeemphasis(48000, us)
		in := make([]float32, 48000)

		for i := range in {
			in[i] = float32(math.Sin(2 * math.Pi * freq * float64(i) / 48000))
		}

		out := make([]float32, len(in))
		if _, err := d.Process(in, out); err != nil {
			t.Fatal(err)
		}

		return rms(out[24000:]) / rms(in[24000:])
	}

	for _, us := range []int{WFMDeemphasis50, WFMDeemphasis75} {
		corner := 1 / (2 * math.Pi * float64(us) * 1e-6)
		if g := gain(us, corner); math.Abs(g-math.Sqrt2/2) > 0.08 {
			t.Fatalf("%d µs: gain %.3f at %.0f Hz", us, g, corner)
		}
	}

	c, err := NewChain(ChainConfig{Demod: DemodWFM, ChannelRate: 240000, OutputRate: 48000, LowHz: -75000, HighHz: 75000, AGC: AGCOff})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.SetDeemphasis(WFMDeemphasis75); err != nil || c.Config().DeemphasisUS != 75 || c.deemph.alpha != newDeemphasis(48000, 75).alpha {
		t.Fatalf("live de-emphasis: %v", err)
	}

	if err := c.SetDeemphasis(60); !errors.Is(err, ErrChain) {
		t.Fatal(err)
	}
}
