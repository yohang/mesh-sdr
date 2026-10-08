package http_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/radio/app"
)

// secondaryFrames pops the queued frames of the connection and returns the
// secondary FFT ones.
func secondaryFrames(t *testing.T, e *decoderEnv) []rxv1.FrameHeader {
	t.Helper()

	var out []rxv1.FrameHeader

	for {
		it, ok := e.p.q.Pop()
		if !ok {
			return out
		}

		if !it.Binary() {
			continue
		}

		h, _, err := rxv1.ParseFrame(append(slices.Clone(it.Header), it.Payload...))
		if err != nil {
			t.Fatal(err)
		}

		if h.Type == rxv1.FrameSecondaryFFT {
			out = append(out, h)
		}
	}
}

// TestTextDecoderSet covers a text decoder (DEC-004, DEC-005): its default
// offset and bandwidth, the secondary FFT stream opened paused and sent
// only while the listener shows it, the offset moved without a new
// session, the dial changes, the partial lines and the frequency of the
// stored ones.
func TestTextDecoderSet(t *testing.T) {
	e := newDecoderEnv(t, true, 0, time.Minute)

	e.check(rxv1.TypeDeviceAttach, map[string]any{"device_id": "vhf"}, "")
	id := e.check(rxv1.TypeDemodCreate, map[string]any{"device_id": "vhf", "mode": "nfm", "offset_hz": 10_000}, "").result.(media.DemodCreated).DemodID

	// NFM is not an underlying mode of RTTY: USB, and the middle of its
	// pass band (300..2700 Hz) as offset.
	started := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "rtty170"}, "").result.(media.DecoderStarted)
	if started.Applied.Mode != "usb" || started.OffsetHz == nil || *started.OffsetHz != 1500 || started.BandwidthHz != 170 || started.SecondaryFFTStreamID == nil {
		t.Fatalf("decoder.set result %+v", started)
	}

	fft := *started.SecondaryFFTStreamID

	opens := sentOf[media.StreamOpen](e.p, rxv1.TypeStreamOpen)
	open := opens[len(opens)-1]

	if open.StreamID != fft || open.Kind != media.KindFFT2 || !open.Paused || open.DemodID != id || open.FFT.Size != 2048 || open.FFT.StartHz != -6000 || open.FFT.SpanHz != 12000 {
		t.Fatalf("stream.open %+v %+v", open, open.FFT)
	}

	r := e.run.last()
	frame := app.SpectrumFrame{Payload: make([]byte, 8+2048), TimestampUS: 1}

	// Paused: nothing is computed nor sent.
	r.ev.Spectrum(frame)

	if _, fps := r.state(); fps != 0 || len(secondaryFrames(t, e)) != 0 {
		t.Fatal("secondary fft sent while paused")
	}

	e.check(rxv1.TypeStreamConfigure, map[string]any{"stream_id": fft, "paused": false}, "")

	if _, fps := r.state(); fps != 9 {
		t.Fatalf("secondary fft at %d fps", fps)
	}

	r.ev.Spectrum(frame)

	if f := secondaryFrames(t, e); len(f) != 1 || f[0].StreamID != fft {
		t.Fatalf("secondary frames %+v", f)
	}

	e.check(rxv1.TypeStreamConfigure, map[string]any{"stream_id": fft, "fps": 99}, rxv1.CodeOutOfRange)

	// The listener's rate is the rate the node computes.
	e.check(rxv1.TypeStreamConfigure, map[string]any{"stream_id": fft, "fps": 3}, "")

	if _, fps := r.state(); fps != 3 {
		t.Fatalf("secondary fft at %d fps, want 3", fps)
	}

	// A click: the same session moves; an offset whose band leaves the
	// selector is refused.
	moved := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "rtty170", "offset_hz": 2125}, "").result.(media.DecoderStarted)
	if moved.DecoderSessionID != started.DecoderSessionID || *moved.OffsetHz != 2125 || len(e.run.runs) != 1 {
		t.Fatalf("offset change %+v", moved)
	}

	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "rtty170", "offset_hz": 5900}, rxv1.CodeOutOfRange)

	// The dial moves: the decoder resets at the same offset.
	e.check(rxv1.TypeDemodSet, map[string]any{"demod_id": id, "offset_hz": 11_000}, "")

	if offs, _ := r.state(); !slices.Equal(offs, []float64{1500, 2125, 2125}) {
		t.Fatalf("retunes %v", offs)
	}

	// A partial line reaches the listener only; the whole line is stored
	// at the dial frequency plus the offset.
	r.ev.Decode(app.DecodeRecord{Time: time.UnixMilli(1_800_000_000_000), Schema: "text.v1", Text: "CQ CQ", Payload: json.RawMessage(`{"text":"CQ CQ"}`), Partial: true})
	r.ev.Decode(app.DecodeRecord{Time: time.UnixMilli(1_800_000_000_000), Schema: "text.v1", Text: "CQ CQ DE F4TEST", Payload: json.RawMessage(`{"text":"CQ CQ DE F4TEST"}`)})

	decodes := sentOf[media.Decode](e.p, rxv1.TypeDecode)
	if len(decodes) != 2 || !decodes[0].Partial || decodes[1].Partial || decodes[1].FreqHz != 144_125_000+11_000+2125 {
		t.Fatalf("decodes %+v", decodes)
	}

	e.pub.mu.Lock()
	if len(e.pub.got) != 1 || e.pub.got[0].Text != "CQ CQ DE F4TEST" || e.pub.got[0].Family != "textmodes" || e.pub.got[0].FreqHz != decodes[1].FreqHz {
		t.Errorf("published %+v", e.pub.got)
	}
	e.pub.mu.Unlock()

	// Another text mode keeps the offset; the old stream closes.
	psk := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "bpsk31"}, "").result.(media.DecoderStarted)
	if *psk.OffsetHz != 2125 || psk.BandwidthHz != 31.25 || *psk.SecondaryFFTStreamID == fft || !r.isClosed() {
		t.Fatalf("bpsk31 %+v", psk)
	}

	if closes := sentOf[media.StreamClose](e.p, rxv1.TypeStreamClose); len(closes) != 1 || closes[0].StreamID != fft {
		t.Fatalf("stream.close %+v", closes)
	}

	e.check(rxv1.TypeStreamConfigure, map[string]any{"stream_id": fft, "paused": false}, rxv1.CodeNotFound)

	// A line of the stopped session is not queued, nor one of the new
	// session while its stream is paused.
	r.ev.Spectrum(frame)
	e.run.last().ev.Spectrum(frame)

	if f := secondaryFrames(t, e); len(f) != 0 {
		t.Fatalf("frames after the stop %+v", f)
	}

	// The sideband flips: the offset moves to the middle of the new pass
	// band.
	e.check(rxv1.TypeDemodSet, map[string]any{"demod_id": id, "mode": "lsb"}, "")
	e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "rtty170"}, "")
	e.check(rxv1.TypeDemodSet, map[string]any{"demod_id": id, "mode": "usb"}, "")

	if offs, _ := e.run.last().state(); offs[0] != 1500 {
		t.Fatalf("offset after the sideband flip %v", offs)
	}

	// A decoder without a secondary selector has no offset nor stream.
	sel := e.check(rxv1.TypeDecoderSet, map[string]any{"demod_id": id, "decoder": "selcall", "offset_hz": 1000}, "").result.(media.DecoderStarted)
	if sel.OffsetHz != nil || sel.SecondaryFFTStreamID != nil || sel.BandwidthHz != 0 {
		t.Fatalf("selcall %+v", sel)
	}
}
