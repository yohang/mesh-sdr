package dsp_test

import (
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/dsp/dsptest"
)

// decodeAll runs iq through d in blocks and returns the text.
func decodeAll(t *testing.T, d *dsp.TextDecoder, iq []complex64) string {
	t.Helper()

	var out strings.Builder

	for len(iq) > 0 {
		n := min(len(iq), 1000)

		text, err := d.Process(iq[:n])
		if err != nil {
			t.Fatal(err)
		}

		out.Write(text)
		iq = iq[n:]
	}

	return out.String()
}

// TestTextDecoders decodes synthesised signals of every native text mode
// (DEC-006 to DEC-012) at an offset, with noise, through the secondary
// selector and the OpenWebRX+ chains.
func TestTextDecoders(t *testing.T) {
	const (
		rate   = dsp.TextRate
		offset = 1200.0
	)

	tests := []struct {
		name string
		cfg  dsp.TextConfig
		iq   []complex64
		want string
	}{
		{
			name: "bpsk31",
			cfg:  dsp.TextConfig{Kind: dsp.TextPSK, Baud: 31.25, BandwidthHz: 31.25, OffsetHz: offset},
			iq:   dsptest.PSK("CQ CQ de F4TEST pse k\n", 31.25, rate, offset),
			want: "CQ de F4TEST pse k",
		},
		{
			name: "bpsk63",
			cfg:  dsp.TextConfig{Kind: dsp.TextPSK, Baud: 62.5, BandwidthHz: 62.5, OffsetHz: offset},
			iq:   dsptest.PSK("CQ CQ de F4TEST pse k\n", 62.5, rate, offset),
			want: "CQ de F4TEST pse k",
		},
		{
			name: "rtty170",
			cfg:  dsp.TextConfig{Kind: dsp.TextRTTY, Baud: 45.45, BandwidthHz: 170, OffsetHz: offset},
			iq:   dsptest.RTTY("RYRYRY CQ CQ DE F4TEST 599 K", 45.45, 170, false, rate, offset),
			want: "CQ DE F4TEST 599 K",
		},
		{
			name: "rtty450 inverted",
			cfg:  dsp.TextConfig{Kind: dsp.TextRTTY, Baud: 50, BandwidthHz: 450, Invert: true, OffsetHz: offset},
			iq:   dsptest.RTTY("RYRYRY CQ CQ DE F4TEST 599 K", 50, 450, true, rate, offset),
			want: "CQ DE F4TEST 599 K",
		},
		{
			name: "rtty85 inverted",
			cfg:  dsp.TextConfig{Kind: dsp.TextRTTY, Baud: 50, BandwidthHz: 85, Invert: true, OffsetHz: offset},
			iq:   dsptest.RTTY("RYRYRY CQ CQ DE F4TEST 599 K", 50, 85, true, rate, offset),
			want: "CQ DE F4TEST 599 K",
		},
		{
			name: "sitorb",
			cfg:  dsp.TextConfig{Kind: dsp.TextSITORB, Baud: 100, BandwidthHz: 210, OffsetHz: offset},
			iq:   dsptest.SITORB("ZCZC EA01 GALE WARNING 1200 UTC NNNN", 170, rate, offset),
			want: "GALE WARNING 1200 UTC",
		},
		{
			name: "cw",
			cfg:  dsp.TextConfig{Kind: dsp.TextCW, BandwidthHz: 75, OffsetHz: offset},
			iq:   dsptest.CW("CQ CQ DE F4TEST F4TEST K", 20, rate, offset),
			want: "DE F4TEST F4TEST",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := dsp.NewTextDecoder(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			iq := append([]complex64(nil), tc.iq...)
			dsptest.AddNoise(iq, 0.02, 1)

			// A signal elsewhere in the band is filtered out by the
			// secondary selector.
			other := dsptest.CW("EEEEEEEEEEEEEEEEEEEE", 25, rate, offset-600)
			for i := range iq {
				iq[i] += other[i%len(other)]
			}

			if got := decodeAll(t, d, iq); !strings.Contains(got, tc.want) {
				t.Fatalf("decoded %q, want %q in it", got, tc.want)
			}
		})
	}
}

// TestTextDecoderOffset: a decoder tuned away from the signal decodes
// nothing; moved onto it (DEC-005), it decodes.
func TestTextDecoderOffset(t *testing.T) {
	iq := dsptest.RTTY("RYRYRY CQ CQ DE F4TEST 599 K", 45.45, 170, false, dsp.TextRate, 2000)

	d, err := dsp.NewTextDecoder(dsp.TextConfig{Kind: dsp.TextRTTY, Baud: 45.45, BandwidthHz: 170, OffsetHz: 800})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if got := decodeAll(t, d, iq); strings.Contains(got, "F4TEST") {
		t.Fatalf("decoded %q away from the signal", got)
	}

	if err := d.SetOffset(2000); err != nil {
		t.Fatal(err)
	}

	if got := decodeAll(t, d, iq); !strings.Contains(got, "CQ DE F4TEST") {
		t.Fatalf("decoded %q at the signal", got)
	}
}

// TestCWShowSymbols: cw_showcw prints the dots and dashes too (DEC-012).
func TestCWShowSymbols(t *testing.T) {
	iq := dsptest.CW("TEST TEST TEST", 20, dsp.TextRate, 800)

	d, err := dsp.NewTextDecoder(dsp.TextConfig{Kind: dsp.TextCW, BandwidthHz: 75, OffsetHz: 800, ShowCW: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if got := decodeAll(t, d, iq); !strings.Contains(got, "...S-T") {
		t.Fatalf("decoded %q, want the symbols of TEST", got)
	}
}

func TestTextConfigValidate(t *testing.T) {
	bad := []dsp.TextConfig{
		{Kind: "fax", Baud: 50, BandwidthHz: 100},
		{Kind: dsp.TextPSK, Baud: 0, BandwidthHz: 31.25},
		{Kind: dsp.TextRTTY, Baud: 45.45, BandwidthHz: 0},
		{Kind: dsp.TextRTTY, Baud: 45.45, BandwidthHz: 170, OffsetHz: 7000},
		{Kind: dsp.TextCW, BandwidthHz: 75, OffsetHz: -6001},
	}

	for _, c := range bad {
		if _, err := dsp.NewTextDecoder(c); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

// TestIQResampler: a tone at a channel rate keeps its frequency at
// TextRate.
func TestIQResampler(t *testing.T) {
	const in = 24094.117647

	r := dsp.NewIQResampler(dsp.TextRate)
	defer r.Close()

	tone := dsptest.CW("TTTTTTTTTT", 5, in, 1000)

	var out []complex64

	for i := 0; i < len(tone); i += 777 {
		b, err := r.Process(tone[i:min(i+777, len(tone))], in)
		if err != nil {
			t.Fatal(err)
		}

		out = append(out, b...)
	}

	want := float64(len(tone)) * dsp.TextRate / in
	if got := float64(len(out)); got < want*0.98 || got > want*1.01 {
		t.Fatalf("got %v samples, want about %v", got, want)
	}
}
