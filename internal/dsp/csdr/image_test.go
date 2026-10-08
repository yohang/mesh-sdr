package csdr

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/yohang/mesh-sdr/internal/dsp/dsptest"
)

// decodeAll feeds audio to d in small blocks and returns its stream.
func decodeAll(t *testing.T, d *ImageDecoder, audio []float32) []byte {
	t.Helper()

	var stream []byte

	out := make([]byte, 2<<20)

	for off := 0; off < len(audio); off += 1000 {
		n, err := d.Process(audio[off:min(off+1000, len(audio))], out)
		if err != nil {
			t.Fatal(err)
		}

		stream = append(stream, out[:n]...)
	}

	return stream
}

// bmpHeader checks the BMP header at the start of stream and returns the
// image width, height, bits per pixel and data offset.
func bmpHeader(t *testing.T, stream []byte) (w, h, bpp, off int) {
	t.Helper()

	if len(stream) < 54 || !bytes.HasPrefix(stream, []byte("BM")) {
		t.Fatalf("no BMP header in %d bytes", len(stream))
	}

	w = int(int32(binary.LittleEndian.Uint32(stream[18:])))
	h = -int(int32(binary.LittleEndian.Uint32(stream[22:])))
	bpp = int(binary.LittleEndian.Uint16(stream[28:]))
	off = int(binary.LittleEndian.Uint32(stream[10:]))

	return w, h, bpp, off
}

// The SSTV decoder finds the VIS code of a Robot 36 picture and decodes
// its colours: red on the left, blue on the right.
func TestSSTVDecoder(t *testing.T) {
	d, err := NewSSTVDecoder(SSTVRate)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	audio := dsptest.SSTV(dsptest.Robot36, SSTVRate, 240, func(x, _ int) (uint8, uint8, uint8) {
		if x < 160 {
			return 220, 30, 30
		}

		return 30, 30, 220
	})

	stream := decodeAll(t, d, audio)
	w, h, bpp, off := bmpHeader(t, stream)

	if stream[6] != dsptest.Robot36 || w != 320 || h != 240 || bpp != 24 || off != 54 {
		t.Fatalf("header: vis %d, %d×%d, %d bpp, offset %d", stream[6], w, h, bpp, off)
	}

	if got, want := len(stream), off+w*h*3; got != want {
		t.Fatalf("stream of %d bytes, want %d", got, want)
	}

	row := stream[off+120*w*3:]
	left, right := row[40*3:40*3+3], row[280*3:280*3+3] // BGR

	if left[2] < 150 || left[0] > 100 || right[0] < 150 || right[2] > 100 {
		t.Errorf("row 120: left BGR %v, right BGR %v", left, right)
	}
}

// The SSTV decoder survives a PD-290 header: its rows are wider than the
// decoder's chroma buffer (the shim gives it room).
func TestSSTVDecoderPD290(t *testing.T) {
	d, err := NewSSTVDecoder(SSTVRate)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	audio := dsptest.SSTV(94, SSTVRate, 0, nil)
	audio = append(audio, make([]float32, 8*SSTVRate)...)

	stream := decodeAll(t, d, audio)
	if w, _, _, _ := bmpHeader(t, stream); w != 800 || len(stream) < 54+4*800*3 {
		t.Fatalf("width %d, %d bytes", w, len(stream))
	}

	// The overflow stays within the first half of the slack.
	if !d.canaryIntact() {
		t.Error("the SSTV decoder wrote past half its slack: re-check sstvSlack")
	}
}

// The FAX decoder detects the start tone and phasing, decodes the page and
// pads it after the stop tone.
func TestFAXDecoder(t *testing.T) {
	d, err := NewFAXDecoder(FAXRate, FAXOptions{LPM: 120, MaxLines: 200})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	audio := dsptest.FAX(FAXRate, 120, 40, func(_, y int) uint8 {
		if y < 20 {
			return 255
		}

		return 0
	})

	stream := decodeAll(t, d, audio)
	w, h, bpp, off := bmpHeader(t, stream)

	if stream[6] != 144 || w != 1812 || h != 200 || bpp != 8 || off != 54+1024 {
		t.Fatalf("header: ioc/4 %d, %d×%d, %d bpp, offset %d", stream[6], w, h, bpp, off)
	}

	rows := stream[off:]
	end := bytes.Index(rows, []byte("END-PAGE!"))

	if end < 0 || end%w != 0 {
		t.Fatalf("no end of page in %d bytes (%d)", len(rows), end)
	}

	lines := end / w
	if lines < 30 || lines > 50 {
		t.Fatalf("%d lines before the end of page", lines)
	}

	mean := func(y int) int {
		s := 0
		for _, v := range rows[y*w : (y+1)*w] {
			s += int(v)
		}

		return s / w
	}

	t.Logf("%d lines; row means %d %d %d", lines, mean(5), mean(lines/2), mean(lines-5))

	if mean(5) < 180 || mean(lines-5) > 75 {
		t.Errorf("row 5 mean %d, row %d mean %d", mean(5), lines-5, mean(lines-5))
	}
}
