package dsp_test

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/dsp/csdr"
	"github.com/yohang/mesh-sdr/internal/dsp/dsptest"
)

// feed sends audio at rate in blocks of 20 ms and collects the events.
func feed(t *testing.T, r *dsp.ImageReceiver, audio []float32, rate int) (starts, rows int, ends []*dsp.Image) {
	t.Helper()

	block := rate / 50

	for off := 0; off < len(audio); off += block {
		evs, err := r.Feed(audio[off:min(off+block, len(audio))], rate)
		if err != nil {
			t.Fatal(err)
		}

		for _, e := range evs {
			switch e.Kind {
			case dsp.ImageStart:
				starts++
			case dsp.ImageRow:
				if len(e.Image.Row(e.Row)) != e.Image.Width*e.Image.Channels {
					t.Fatalf("row %d of %d bytes", e.Row, len(e.Image.Row(e.Row)))
				}

				rows++
			case dsp.ImageEnd:
				ends = append(ends, e.Image)
			}
		}
	}

	return starts, rows, ends
}

func redBlue(x, _ int) (uint8, uint8, uint8) {
	if x < 160 {
		return 220, 30, 30
	}

	return 30, 30, 220
}

// A Robot 36 picture sent at 48 kHz is resampled, decoded and ends
// complete; its rows are RGB.
func TestSSTVReceiverComplete(t *testing.T) {
	r, err := dsp.NewSSTVReceiver()
	if err != nil {
		t.Fatal(err)
	}

	starts, rows, ends := feed(t, r, dsptest.SSTV(dsptest.Robot36, 48000, 240, redBlue), 48000)

	if r.Close() != nil {
		t.Error("an image is still in progress")
	}

	if starts != 1 || rows != 240 || len(ends) != 1 {
		t.Fatalf("%d starts, %d rows, %d ends", starts, rows, len(ends))
	}

	m := ends[0]
	if m.VIS != dsptest.Robot36 || m.Width != 320 || m.Height != 240 || m.Channels != 3 || m.Lines != 240 || !m.Complete {
		t.Fatalf("image %+v", *m)
	}

	row := m.Row(120)
	if left, right := row[40*3:40*3+3], row[280*3:280*3+3]; left[0] < 150 || left[2] > 100 || right[2] < 150 || right[0] > 100 {
		t.Errorf("row 120: left RGB %v, right RGB %v", left, right)
	}
}

// A Martin 1 picture cut short is returned by Close, incomplete, with the
// lines received.
func TestSSTVReceiverCut(t *testing.T) {
	r, err := dsp.NewSSTVReceiver()
	if err != nil {
		t.Fatal(err)
	}

	_, _, ends := feed(t, r, dsptest.SSTV(dsptest.Martin1, csdr.SSTVRate, 40, redBlue), csdr.SSTVRate)
	if len(ends) != 0 {
		t.Fatalf("%d ends", len(ends))
	}

	m := r.Close()
	if m == nil || m.VIS != dsptest.Martin1 || m.Height != 256 || m.Complete || m.Lines < 38 || m.Lines > 41 || len(m.Received()) != m.Lines*320*3 {
		t.Fatalf("image %+v", m)
	}
}

func bands(_, y int) uint8 {
	if y < 20 {
		return 255
	}

	return 0
}

// A FAX page ends complete at the stop tone with the lines received.
func TestFAXReceiver(t *testing.T) {
	r, err := dsp.NewFAXReceiver(csdr.FAXOptions{LPM: 120, MaxLines: 300}, 0)
	if err != nil {
		t.Fatal(err)
	}

	starts, rows, ends := feed(t, r, dsptest.FAX(csdr.FAXRate, 120, 40, bands), csdr.FAXRate)

	if r.Close() != nil {
		t.Error("a page is still in progress")
	}

	if starts != 1 || rows != 40 || len(ends) != 1 {
		t.Fatalf("%d starts, %d rows, %d ends", starts, rows, len(ends))
	}

	if m := ends[0]; m.IOC != 576 || m.Width != 1812 || m.Height != 300 || m.Channels != 1 || m.Lines != 40 || !m.Complete || m.Row(5)[900] < 200 {
		t.Fatalf("page %+v", *m)
	}
}

// A FAX page longer than its longest length ends complete there.
func TestFAXReceiverMaxLines(t *testing.T) {
	r, err := dsp.NewFAXReceiver(csdr.FAXOptions{LPM: 120, MaxLines: 30}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	_, rows, ends := feed(t, r, dsptest.FAX(csdr.FAXRate, 120, 40, bands), csdr.FAXRate)

	if rows != 30 || len(ends) != 1 || ends[0].Lines != 30 || !ends[0].Complete {
		t.Fatalf("%d rows, ends %v", rows, ends)
	}
}

// A FAX page beyond the pixel cap ends there, incomplete, and its rows
// left are dropped.
func TestFAXReceiverCap(t *testing.T) {
	r, err := dsp.NewFAXReceiver(csdr.FAXOptions{LPM: 120, MaxLines: 300}, 10*1812)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	starts, rows, ends := feed(t, r, dsptest.FAX(csdr.FAXRate, 120, 20, bands), csdr.FAXRate)

	if starts != 1 || rows != 10 || len(ends) != 1 {
		t.Fatalf("%d starts, %d rows, %d ends", starts, rows, len(ends))
	}

	if m := ends[0]; m.Lines != 10 || m.Complete || cap(m.Pix) != 10*1812 {
		t.Fatalf("page %+v", *m)
	}
}
