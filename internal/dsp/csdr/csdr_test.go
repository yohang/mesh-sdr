package csdr

import (
	"math"
	"math/cmplx"
	"sync"
	"testing"
)

func tone(n int, cycles float64, amp float64) []complex64 {
	out := make([]complex64, n)
	for i := range out {
		out[i] = complex64(cmplx.Rect(amp, 2*math.Pi*cycles*float64(i)))
	}

	return out
}

func TestFFTPeakAndCarry(t *testing.T) {
	const size = 1024

	fft, err := NewFFT(size, size, Blackman)
	if err != nil {
		t.Fatal(err)
	}
	defer fft.Close()

	pow, err := NewLogAveragePower(size, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pow.Close()

	// 100 bins above the centre.
	in := tone(4*size, 100.0/size, 1)
	spec := make([]complex64, 8*size)
	line := make([]float32, 8*size)

	// Feed in odd chunks: the carry keeps what a module cannot use yet.
	var n int

	for off := 0; off < len(in); off += 700 {
		end := min(off+700, len(in))

		k, err := fft.Process(in[off:end], spec)
		if err != nil {
			t.Fatal(err)
		}

		m, err := pow.Process(spec[:k], line[n:])
		if err != nil {
			t.Fatal(err)
		}

		n += m
	}

	if n < size {
		t.Fatalf("got %d bins, want at least one line of %d", n, size)
	}

	peak := 0
	for i := 1; i < size; i++ {
		if line[i] > line[peak] {
			peak = i
		}
	}

	// FFT order: positive frequencies first.
	if peak != 100 {
		t.Fatalf("peak at bin %d, want 100", peak)
	}
}

func TestFMDemodConstantFrequency(t *testing.T) {
	d, err := NewFMDemod()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	in := tone(4096, 0.05, 1)
	out := make([]float32, len(in))

	n, err := d.Process(in, out)
	if err != nil || n != len(in) {
		t.Fatalf("n=%d err=%v", n, err)
	}

	// Phase step 2π·0.05 → 0.1 after scaling ±π to ±1.
	for i := 10; i < n; i++ {
		if math.Abs(float64(out[i])-0.1) > 1e-3 {
			t.Fatalf("sample %d = %g, want 0.1", i, out[i])
		}
	}
}

func TestShiftMovesTone(t *testing.T) {
	s, err := NewShift(-0.1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	in := tone(2048, 0.1, 1)
	out := make([]complex64, len(in))

	n, err := s.Process(in, out)
	if err != nil || n != len(in) {
		t.Fatalf("n=%d err=%v", n, err)
	}

	// Shifted to DC: constant phase.
	ref := cmplx.Phase(complex128(out[1]))
	for i := 2; i < n; i++ {
		if math.Abs(cmplx.Phase(complex128(out[i]))-ref) > 1e-2 {
			t.Fatalf("sample %d not at DC", i)
		}
	}

	if err := s.SetRate(0); err != nil {
		t.Fatal(err)
	}
}

func TestResamplerRate(t *testing.T) {
	r, err := NewResampler(24000, 12000)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	in := make([]float32, 24000)
	out := make([]float32, 24000)
	total := 0

	for off := 0; off < len(in); off += 480 {
		n, err := r.Process(in[off:off+480], out[total:])
		if err != nil {
			t.Fatal(err)
		}

		total += n
	}

	if total < 11800 || total > 12000 {
		t.Fatalf("got %d samples for 1 s at 12 kHz", total)
	}
}

func TestAGCAndDeemphasisBuild(t *testing.T) {
	a, err := NewAGC(AGC{Reference: 0.8, Attack: 0.1, Decay: 0.001, MaxGain: 3, Hang: 200})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	in := make([]float32, 1000)
	for i := range in {
		in[i] = 0.1 * float32(math.Sin(float64(i)/10))
	}

	out := make([]float32, 1000)

	n, err := a.Process(in, out)
	if err != nil {
		t.Fatal(err)
	}

	// 100 samples of look-ahead stay in the carry.
	if n != 900 || a.Pending() != 100 {
		t.Fatalf("n=%d pending=%d", n, a.Pending())
	}

	d, err := NewNFMDeemphasis(24000)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Process(in, out); err != nil {
		t.Fatal(err)
	}

	l, err := NewLimit(1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if _, err := NewAGC(AGC{}); err == nil {
		t.Fatal("invalid AGC accepted")
	}
}

func TestConcurrentFFTPlans(t *testing.T) {
	var wg sync.WaitGroup

	for range 16 {
		wg.Go(func() {
			for range 20 {
				f, err := NewFFT(2048, 1000, Hamming)
				if err != nil {
					t.Error(err)

					return
				}

				f.Close()
			}
		})
	}

	wg.Wait()
}

func TestClosedStage(t *testing.T) {
	l, err := NewLimit(1)
	if err != nil {
		t.Fatal(err)
	}

	l.Close()
	l.Close()

	if _, err := l.Process([]float32{1}, make([]float32, 1)); err != ErrClosed {
		t.Fatalf("err = %v", err)
	}
}

func TestAMDemodRealPartDCBlock(t *testing.T) {
	am, err := NewAMDemod()
	if err != nil {
		t.Fatal(err)
	}
	defer am.Close()

	rp, err := NewRealPart()
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()

	dc, err := NewDCBlock()
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()

	in := tone(8192, 0.01, 0.5)
	out := make([]float32, len(in))

	if n, err := am.Process(in, out); err != nil || n != len(in) || math.Abs(float64(out[100])-0.5) > 1e-4 {
		t.Fatalf("am: n=%d err=%v out=%g", n, err, out[100])
	}

	if n, err := rp.Process(in, out); err != nil || n != len(in) || math.Abs(float64(out[100])-0.5*math.Cos(2*math.Pi)) > 1e-3 {
		t.Fatalf("real part: n=%d err=%v out=%g", n, err, out[100])
	}

	// The magnitude is a constant: DC block brings it to zero.
	if _, err := am.Process(in, out); err != nil {
		t.Fatal(err)
	}

	blocked := make([]float32, len(out))
	if _, err := dc.Process(out, blocked); err != nil {
		t.Fatal(err)
	}

	if v := math.Abs(float64(blocked[len(blocked)-1])); v > 0.01 {
		t.Fatalf("dc block left %g", v)
	}
}

func TestBandPassKeepsBand(t *testing.T) {
	if _, err := NewBandPass(0.2, 0.1, 0.01); err == nil {
		t.Fatal("inverted band accepted")
	}

	run := func(cycles float64) float64 {
		bp, err := NewBandPass(0.01, 0.1, 0.01)
		if err != nil {
			t.Fatal(err)
		}
		defer bp.Close()

		in := tone(16384, cycles, 1)
		out := make([]complex64, len(in))
		n := 0

		for off := 0; off < len(in); off += 1000 {
			k, err := bp.Process(in[off:min(off+1000, len(in))], out[n:])
			if err != nil {
				t.Fatal(err)
			}

			n += k
		}

		if n < len(in)/2 {
			t.Fatalf("got %d samples", n)
		}

		var p float64
		for _, v := range out[n/2 : n] {
			p += float64(real(v)*real(v) + imag(v)*imag(v))
		}

		return p / float64(n-n/2)
	}

	if p := run(0.05); p < 0.8 || p > 1.2 {
		t.Fatalf("in-band power %g", p)
	}

	// The mirror frequency is outside the complex band.
	if p := run(-0.05); p > 1e-3 {
		t.Fatalf("out-of-band power %g", p)
	}
}

func TestNoiseFilterAndWFMDeemphasis(t *testing.T) {
	nf, err := NewNoiseFilter(256, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer nf.Close()

	in := make([]float32, 2048)
	for i := range in {
		in[i] = 0.5 * float32(math.Sin(2*math.Pi*0.05*float64(i)))
	}

	out := make([]float32, len(in))

	n, err := nf.Process(in, out)
	if err != nil || n == 0 || n%128 != 0 {
		t.Fatalf("noise filter: n=%d err=%v", n, err)
	}

	if err := nf.SetThreshold(10); err != nil {
		t.Fatal(err)
	}

	if _, err := NewNoiseFilter(8, 0); err == nil {
		t.Fatal("tiny noise filter accepted")
	}

	d, err := NewWFMDeemphasis(48000, 50e-6)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if n, err := d.Process(in, out); err != nil || n != len(in) {
		t.Fatalf("wfm deemphasis: n=%d err=%v", n, err)
	}
}
