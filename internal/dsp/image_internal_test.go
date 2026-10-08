package dsp

import (
	"encoding/binary"
	"testing"
)

// sstvHeader is the BMP header SstvDecoder writes for a VIS code.
func sstvHeader(vis, w, h int) []byte {
	b := make([]byte, 54)
	b[0], b[1], b[6], b[7] = 'B', 'M', byte(vis), 0x73
	binary.LittleEndian.PutUint32(b[10:], 54)
	binary.LittleEndian.PutUint32(b[14:], 40)
	binary.LittleEndian.PutUint32(b[18:], uint32(w))
	binary.LittleEndian.PutUint32(b[22:], uint32(int32(-h)))
	binary.LittleEndian.PutUint16(b[26:], 1)
	binary.LittleEndian.PutUint16(b[28:], 24)

	return b
}

// An SSTV header without rows (the decoder lost the start sync of a
// Scottie image) ends that image; the next header starts the next one.
func TestImageOrphanHeader(t *testing.T) {
	r := &ImageReceiver{}

	r.pending = append(r.pending, sstvHeader(60, 320, 256)...)
	r.pending = append(r.pending, sstvHeader(44, 320, 256)...)

	for range 3 {
		row := make([]byte, 320*3)
		row[0] = 9
		r.pending = append(r.pending, row...)
	}

	r.parse()

	var kinds []ImageEventKind
	for _, e := range r.events {
		kinds = append(kinds, e.Kind)
	}

	want := []ImageEventKind{ImageStart, ImageEnd, ImageStart, ImageRow, ImageRow, ImageRow}
	if len(kinds) != len(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}

	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events %v, want %v", kinds, want)
		}
	}

	if first := r.events[1].Image; first.VIS != 60 || first.Lines != 0 || first.Complete {
		t.Errorf("orphan image %+v", *first)
	}

	if r.cur == nil || r.cur.VIS != 44 || r.cur.Lines != 3 || len(r.pending) != 0 {
		t.Errorf("current image %+v, %d bytes left", r.cur, len(r.pending))
	}
}
