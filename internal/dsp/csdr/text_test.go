package csdr

import "testing"

// random fills b with ±1 from a xorshift generator.
func random(b []float32, x *uint64) {
	for i := range b {
		*x ^= *x << 13
		*x ^= *x >> 7
		*x ^= *x << 17

		b[i] = 1
		if *x&1 == 0 {
			b[i] = -1
		}
	}
}

// TestTimingRecoveryBounds: parameters whose correction could pass half a
// symbol are refused; random input never takes the module out of its
// input.
func TestTimingRecoveryBounds(t *testing.T) {
	if _, err := NewTimingRecovery(240, 28.2, 10); err == nil {
		t.Fatal("timing recovery 240, 28.2, 10 accepted")
	}

	if _, err := NewComplexTimingRecovery(240, 2, 2); err == nil {
		t.Fatal("complex timing recovery 240, 2, 2 accepted")
	}

	tr, err := NewTimingRecovery(240, 14, 1.0/14)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	x := uint64(1)
	in := make([]float32, 4096)
	out := make([]float32, 4096)

	for range 200 {
		random(in, &x)

		if _, err := tr.Process(in, out); err != nil {
			t.Fatal(err)
		}

		if p := tr.Pending(); p > 2*len(in) {
			t.Fatalf("carry grows: %d", p)
		}
	}
}

// TestByteStageOutput: a byte stage refuses an output shorter than its
// input, and a closed one fails.
func TestByteStageOutput(t *testing.T) {
	r, err := NewRTTY(false)
	if err != nil {
		t.Fatal(err)
	}

	x := uint64(7)
	in := make([]float32, 1024)
	random(in, &x)

	if _, err := r.Process(in, make([]byte, 10)); err == nil {
		t.Fatal("short output accepted")
	}

	if _, err := r.Process(in, make([]byte, len(in))); err != nil {
		t.Fatal(err)
	}

	r.Close()

	if _, err := r.Process(in, make([]byte, len(in))); err == nil {
		t.Fatal("closed stage processed")
	}

	cw, err := NewCW(12000, true)
	if err != nil {
		t.Fatal(err)
	}

	cw.Close()

	if err := cw.Reset(); err == nil {
		t.Fatal("closed cw reset")
	}
}
