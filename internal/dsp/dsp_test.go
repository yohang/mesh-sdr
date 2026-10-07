package dsp

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/cmplx"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// toneAt returns n samples of a complex tone at freq Hz (rate fs), starting
// at sample index start, with optional noise.
func toneAt(start, n int, fs, freq, amp, noise float64, rng *rand.Rand) []complex64 {
	out := make([]complex64, n)
	for i := range out {
		v := cmplx.Rect(amp, 2*math.Pi*freq*float64(start+i)/fs)
		if noise > 0 {
			v += complex(rng.NormFloat64()*noise, rng.NormFloat64()*noise)
		}

		out[i] = complex64(v)
	}

	return out
}

func TestRingReadersAndGaps(t *testing.T) {
	r := NewRing[complex64](4, 8)
	fast := r.NewReader()
	slow := r.NewReader()
	ctx := context.Background()

	for i := range 6 {
		r.Push(uint64(i*8), 8, t0, make([]complex64, 8))

		meta, data, gap, err := fast.Read(ctx)
		if err != nil || gap != nil || meta.Index != uint64(i*8) || len(data) != 8 {
			t.Fatalf("fast read %d: %+v %v %v", i, meta, gap, err)
		}
	}

	// The slow reader lost two blocks (16 samples): one gap marker.
	meta, _, gap, err := slow.Read(ctx)
	if err != nil || meta.Index != 16 || gap != nil {
		t.Fatalf("first slow read: %+v %+v %v", meta, gap, err)
	}

	// A hole in the source indexes is a gap too.
	r.Push(100, 8, t0, make([]complex64, 8))

	for range 3 {
		if _, _, _, err := slow.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}

	_, _, gap, _ = slow.Read(ctx)
	if gap == nil || gap.From != 48 || gap.To != 100 || slow.Gaps != 1 {
		t.Fatalf("gap = %+v, gaps %d", gap, slow.Gaps)
	}

	cctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()

	if _, _, _, err := fast.Read(cctx); !errors.Is(err, context.DeadlineExceeded) {
		_, _, _, err = fast.Read(cctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	}

	r.Close()

	if _, _, _, err := slow.Read(ctx); !errors.Is(err, ErrRingClosed) {
		t.Fatalf("err = %v", err)
	}
}

func TestSpectrumToneLevelAndBin(t *testing.T) {
	const fs = 2_400_000

	cfg := DefaultSpectrum(fs)

	avg, every, err := cfg.Plan()
	if err != nil || avg != 93 || every != 2867 {
		t.Fatalf("plan avg=%d every=%d err=%v", avg, every, err)
	}

	s, err := NewSpectrum(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rng := rand.New(rand.NewPCG(1, 2))
	// Bin-centred tone 512 bins above the centre, full scale.
	freq := 512.0 * fs / float64(cfg.Size)

	var lines []Line

	for b := range 60 {
		iq := toneAt(b*16384, 16384, fs, freq, 1, 1e-4, rng)
		if err := s.Push(uint64(b*16384), t0.Add(time.Duration(b*16384)*time.Second/fs), iq, func(l Line) {
			lines = append(lines, Line{Index: l.Index, Time: l.Time, DB: append([]float32(nil), l.DB...)})
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 60 × 16384 samples = 0.41 s at 9 fps: 3 lines.
	if len(lines) < 3 || len(lines) > 4 {
		t.Fatalf("got %d lines", len(lines))
	}

	l := lines[len(lines)-1]
	peak := 0

	for i, v := range l.DB {
		if v > l.DB[peak] {
			peak = i
		}
	}

	if peak != cfg.Size/2+512 {
		t.Fatalf("peak at %d", peak)
	}

	if math.Abs(float64(l.DB[peak])) > 1 {
		t.Fatalf("full-scale tone reads %g dBFS", l.DB[peak])
	}

	if l.DB[100] > -60 {
		t.Fatalf("floor %g dBFS", l.DB[100])
	}

	if !l.Time.After(lines[0].Time) || lines[1].Index <= lines[0].Index {
		t.Fatal("line positions not increasing")
	}

	payload := EncodeFFTU8(nil, rxv1.DefaultFFTU8Scale(), l.DB)
	scale, bins, err := rxv1.ParseFFTU8(payload)

	// The default scale tops out at −22.5 dBFS: the tone saturates, the
	// floor maps to its level.
	if err != nil || len(bins) != cfg.Size || bins[peak] != 255 || math.Abs(float64(scale.DB(bins[100])-l.DB[100])) > 0.25 {
		t.Fatalf("u8 payload: %v %d %d", err, len(bins), bins[peak])
	}
}

func TestSpectrumConfigValidation(t *testing.T) {
	for _, c := range []SpectrumConfig{
		{SampleRate: 0, Size: 4096, FPS: 9},
		{SampleRate: 1000, Size: 65536, FPS: 9},
		{SampleRate: 1000, Size: 4096, FPS: 0},
		{SampleRate: 1000, Size: 4096, FPS: 9, VOverlap: 1},
	} {
		if _, err := NewSpectrum(c); !errors.Is(err, ErrSpectrumConfig) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
}

func TestChannelPlan(t *testing.T) {
	for _, fs := range []int{250_000, 1_024_000, 2_048_000, 2_400_000, 10_000_000} {
		p, err := NewChannelPlan(fs, NFMChannelRate)
		if err != nil {
			t.Fatal(err)
		}

		if p.N != p.D*p.M || p.V%p.D != 0 || p.L%p.D != 0 || p.ChannelRate() < NFMChannelRate || p.MaxTaps() < int(blackmanTransition*float64(fs)/Transition) {
			t.Fatalf("fs %d: bad plan %+v", fs, p)
		}
	}

	if _, err := NewChannelPlan(12000, NFMChannelRate); !errors.Is(err, ErrChannelizer) {
		t.Fatal(err)
	}
}

// channelOut runs iq through a channelizer and one channel.
func channelOut(t *testing.T, p ChannelPlan, iq []complex64, offset, low, high float64) ([]complex64, *Channel) {
	t.Helper()

	c := NewChannelizer(p)

	ch, err := p.NewChannel(offset, low, high)
	if err != nil {
		t.Fatal(err)
	}

	var out []complex64

	for off := 0; off < len(iq); off += 5000 {
		c.Push(uint64(off), iq[off:min(off+5000, len(iq))], func(_ uint64, bins []complex128) {
			out = ch.Process(bins, out)
		})
	}

	return out, ch
}

func TestChannelizerSelectsAndFilters(t *testing.T) {
	const fs = 2_400_000

	p, err := NewChannelPlan(fs, NFMChannelRate)
	if err != nil {
		t.Fatal(err)
	}

	// Wanted tone 1 kHz above a channel at +300.2 kHz, interferer 20 kHz
	// away, outside the ±5 kHz band.
	const offset = 300_200.0

	n := 40 * p.L
	iq := toneAt(0, n, fs, offset+1000, 0.5, 0, nil)
	jam := toneAt(0, n, fs, offset+20000, 0.5, 0, nil)

	for i := range iq {
		iq[i] += jam[i]
	}

	out, ch := channelOut(t, p, iq, offset, -5000, 5000)

	if want := (n/p.L - 1) * p.L / p.D; len(out) < want {
		t.Fatalf("got %d samples, want ≥ %d", len(out), want)
	}

	// Remove the residual; the wanted tone remains at +1 kHz with its
	// amplitude, continuous across blocks.
	fch := p.ChannelRate()
	settle := 2 * p.L / p.D
	var phaseErr, ampErr float64

	for i := settle; i < len(out); i++ {
		v := complex128(out[i]) * cmplx.Exp(complex(0, -2*math.Pi*ch.Residual()*float64(i)/fch))
		ampErr = max(ampErr, math.Abs(cmplx.Abs(v)-0.5))

		if i > settle {
			prev := complex128(out[i-1]) * cmplx.Exp(complex(0, -2*math.Pi*ch.Residual()*float64(i-1)/fch))
			step := cmplx.Phase(v / prev)
			phaseErr = max(phaseErr, math.Abs(step-2*math.Pi*1000/fch))
		}
	}

	if ampErr > 0.02 || phaseErr > 0.02 {
		t.Fatalf("amplitude error %g, phase step error %g", ampErr, phaseErr)
	}
}

func TestChannelRejectsBadBand(t *testing.T) {
	p, _ := NewChannelPlan(2_400_000, NFMChannelRate)

	if _, err := p.NewChannel(0, 5000, -5000); !errors.Is(err, ErrChannelizer) {
		t.Fatal(err)
	}

	if _, err := p.NewChannel(0, -15000, 15000); !errors.Is(err, ErrChannelizer) {
		t.Fatal(err)
	}

	if _, err := p.NewChannel(1_300_000, -5000, 5000); !errors.Is(err, ErrChannelizer) {
		t.Fatal(err)
	}
}

// fmSignal is an NFM carrier at offset Hz modulated by a tone (deviation dev).
func fmSignal(n int, fs, offset, toneHz, dev float64) []complex64 {
	out := make([]complex64, n)
	phase := 0.0

	for i := range out {
		inst := offset + dev*math.Sin(2*math.Pi*toneHz*float64(i)/fs)
		phase += 2 * math.Pi * inst / fs
		out[i] = complex64(cmplx.Rect(0.3, phase))
	}

	return out
}

// toneEnergy returns the relative energy of freq in audio (Goertzel).
func toneEnergy(audio []float32, rate, freq float64) float64 {
	k := 2 * math.Cos(2*math.Pi*freq/rate)

	var s1, s2, total float64
	for _, v := range audio {
		s := float64(v) + k*s1 - s2
		s2, s1 = s1, s
		total += float64(v) * float64(v)
	}

	p := s1*s1 + s2*s2 - k*s1*s2
	if total == 0 {
		return 0
	}

	return p / (total * float64(len(audio)) / 2)
}

func TestNFMDemodulatesTone(t *testing.T) {
	const fs = 2_400_000

	p, _ := NewChannelPlan(fs, NFMChannelRate)

	const offset = -150_000.0

	iq := fmSignal(fs/2, fs, offset, 1000, 2500)
	chIQ, ch := channelOut(t, p, iq, offset, -6000, 6000)

	sq := -60.0

	c, err := NewNFM(NFMConfig{ChannelRate: p.ChannelRate(), OutputRate: DefaultOutputRate, ResidualHz: ch.Residual(), Squelch: &sq})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var audio []float32

	var res Result
	for off := 0; off < len(chIQ); off += 480 {
		res, err = c.Process(chIQ[off:min(off+480, len(chIQ))])
		if err != nil {
			t.Fatal(err)
		}

		audio = append(audio, res.Audio...)
	}

	if !res.Open || res.LevelDB < -15 || res.LevelDB > -5 {
		t.Fatalf("level %g dB open %v", res.LevelDB, res.Open)
	}

	// ~0.5 s at 12 kHz, minus the chain delays.
	if len(audio) < 5000 || len(audio) > 6100 {
		t.Fatalf("got %d audio samples", len(audio))
	}

	tail := audio[len(audio)/2:]
	if e := toneEnergy(tail, DefaultOutputRate, 1000); e < 0.5 {
		t.Fatalf("1 kHz holds %.2f of the audio energy", e)
	}

	// Squelch above the level: silence.
	high := -1.0
	if err := c.SetSquelch(&high); err != nil {
		t.Fatal(err)
	}

	res, err = c.Process(chIQ[:480])
	if err != nil || res.Open {
		t.Fatalf("squelch: %v open %v", err, res.Open)
	}

	for _, v := range res.Audio {
		if v != 0 {
			t.Fatal("squelched audio is not silence")
		}
	}
}

func TestNFMConfigValidation(t *testing.T) {
	bad := 5.0

	for _, c := range []NFMConfig{
		{ChannelRate: 24000, OutputRate: 9000},
		{ChannelRate: 1000, OutputRate: 12000},
		{ChannelRate: 24000, OutputRate: 12000, Squelch: &bad},
		{ChannelRate: 24000, OutputRate: 12000, AGC: "medium"},
	} {
		if _, err := NewNFM(c); !errors.Is(err, ErrChain) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
}

func TestADPCMRoundTrip(t *testing.T) {
	samples := make([]int16, 480)
	for i := range samples {
		samples[i] = int16(8000 * math.Sin(2*math.Pi*700*float64(i)/12000))
	}

	var enc ADPCMEncoder

	pred, idx := enc.State()
	data := enc.Encode(nil, samples)
	dec := DecodeADPCM(pred, idx, data)

	// After the first 40 samples (the predictor converges from 0).
	var sig, noise float64
	for i := 40; i < len(samples); i++ {
		sig += float64(samples[i]) * float64(samples[i])
		d := float64(samples[i]) - float64(dec[i])
		noise += d * d
	}

	if snr := 10 * math.Log10(sig/noise); snr < 20 {
		t.Fatalf("ADPCM SNR %.1f dB", snr)
	}
}

func TestFramerCodecs(t *testing.T) {
	if _, err := NewFramer(rxv1.CodecOpus, 12000); !errors.Is(err, ErrChain) {
		t.Fatal(err)
	}

	audio := make([]float32, 1000)
	for i := range audio {
		audio[i] = 0.5 * float32(math.Sin(float64(i)/5))
	}

	for _, codec := range []rxv1.Codec{rxv1.CodecPCMS16LE, rxv1.CodecADPCMIMA} {
		f, err := NewFramer(codec, 12000)
		if err != nil {
			t.Fatal(err)
		}

		var frames []AudioFrame

		f.Push(audio, t0, false, func(fr AudioFrame) { frames = append(frames, fr) })
		f.Push(audio[:200], t0.Add(time.Second), true, func(fr AudioFrame) { frames = append(frames, fr) })

		if len(frames) != 5 {
			t.Fatalf("%s: %d frames", codec, len(frames))
		}

		if !frames[0].Reset || frames[1].Reset || frames[0].Samples != 240 || frames[0].Duration(12000) != FrameDuration {
			t.Fatalf("%s: first frames %+v", codec, frames[:2])
		}

		if !frames[4].Squelched || frames[3].Squelched {
			t.Fatalf("%s: squelch flags", codec)
		}

		if !frames[1].Time.Equal(t0.Add(FrameDuration)) {
			t.Fatalf("%s: frame time %v", codec, frames[1].Time)
		}

		switch codec {
		case rxv1.CodecPCMS16LE:
			if len(frames[0].Payload) != 480 || int16(binary.LittleEndian.Uint16(frames[0].Payload[2:])) != toS16(audio[1]) {
				t.Fatal("pcm payload")
			}
		case rxv1.CodecADPCMIMA:
			st, nib, err := rxv1.ParseADPCM(frames[1].Payload)
			if err != nil || len(nib) != 120 {
				t.Fatalf("adpcm payload: %v", err)
			}

			dec := DecodeADPCM(st.Predictor, st.StepIndex, nib)
			if math.Abs(float64(dec[100])-float64(toS16(audio[340]))) > 2000 {
				t.Fatalf("adpcm frame 1 does not decode on its own: %d vs %d", dec[100], toS16(audio[340]))
			}
		}
	}
}
