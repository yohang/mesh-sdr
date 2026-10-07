// Package sendq is the per-connection send queue of the node media
// WebSocket (TECHNICAL_SPEC §6.8, ADR 0004): producers never block, JSON
// control messages are never dropped (1 MiB cap), audio drops its oldest
// frames (500 ms cap) and flags the next one with discontinuity, demod
// meters are coalesced per demodulator, FFT streams keep the latest two
// frames and halve their rate under sustained drops, and the whole queue is
// capped at 4 MiB. Pop serves JSON, then audio, then meters, then FFT
// (ADR 0004 decision 3) and numbers the binary frames per stream.
//
// The package is pure: no I/O, no goroutines, an injected clock.
package sendq

import (
	"errors"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// Errors.
var (
	// ErrSlowConsumer means the connection must be closed with 4413.
	ErrSlowConsumer = errors.New("sendq: slow consumer")
	// ErrInvalidFrame rejects a frame whose header or size is invalid.
	ErrInvalidFrame = errors.New("sendq: invalid frame")
)

func (f Frame) valid() bool {
	h := rxv1.FrameHeader{Type: f.Type, Codec: f.Codec, StreamID: f.StreamID, Flags: f.Flags}

	return h.Validate() == nil && rxv1.HeaderSize+len(f.Payload) <= rxv1.MaxFrameBytes
}

// Config holds the §6.8 limits.
type Config struct {
	MaxJSONBytes  int
	MaxTotalBytes int
	// AudioCap is the audio backlog kept (by duration) before the oldest
	// frames are dropped.
	AudioCap time.Duration
	// AudioOverflow closes the connection when audio drops go on with no
	// drop-free pause of AudioQuiet (ADR 0004 decision 3).
	AudioOverflow time.Duration
	AudioQuiet    time.Duration
	// FFTDepth is the number of pending frames kept per FFT stream.
	FFTDepth int
	// FFT fps policy: halve when ≥ FFTDropRatio of the frames were dropped
	// over FFTWindow, raise again after FFTRecover without drops.
	FFTWindow    time.Duration
	FFTDropRatio float64
	FFTRecover   time.Duration
}

// DefaultConfig returns the §6.8 values.
func DefaultConfig() Config {
	return Config{
		MaxJSONBytes:  1 << 20,
		MaxTotalBytes: 4 << 20,
		AudioCap:      500 * time.Millisecond,
		AudioOverflow: 10 * time.Second,
		AudioQuiet:    time.Second,
		FFTDepth:      2,
		FFTWindow:     5 * time.Second,
		FFTDropRatio:  0.25,
		FFTRecover:    30 * time.Second,
	}
}

// Frame is a binary frame before numbering. Payload is shared between
// connections and never modified.
type Frame struct {
	Type        rxv1.FrameType
	Codec       rxv1.Codec
	StreamID    uint16
	Flags       rxv1.Flags
	TimestampUS uint64
	Payload     []byte
	// Duration is the audio duration of an audio frame.
	Duration time.Duration
}

// Item is what the writer sends next: Text, or Header followed by Payload
// in one binary message.
type Item struct {
	Text    []byte
	Header  []byte
	Payload []byte
}

// Binary reports whether the item is a binary frame.
func (i Item) Binary() bool { return i.Text == nil }

// FPSUpdate builds the stream.update text sent when the queue changes the
// rate of an FFT stream.
type FPSUpdate func(streamID uint16, fps int) []byte

type meter struct {
	key  string
	data []byte
}

type fftStream struct {
	requested, fps int
	paused         bool
	pending        []Frame
	lastAccept     time.Time
	windowStart    time.Time
	offered, drops int
	lastDrop       time.Time
}

// Queue is one connection's send queue. It is safe for concurrent use.
type Queue struct {
	cfg    Config
	now    func() time.Time
	update FPSUpdate

	mu        sync.Mutex
	json      [][]byte
	jsonBytes int
	audio     []Frame
	audioDur  time.Duration
	meters    []meter
	ffts      map[uint16]*fftStream
	order     []uint16
	next      int
	seq       map[uint16]uint32
	gap       map[uint16]bool
	total     int
	err       error

	overflowSince, lastAudioDrop time.Time

	ready chan struct{}
}

// New returns an empty queue.
func New(cfg Config, now func() time.Time, update FPSUpdate) *Queue {
	return &Queue{
		cfg: cfg, now: now, update: update,
		ffts: map[uint16]*fftStream{}, seq: map[uint16]uint32{}, gap: map[uint16]bool{},
		ready: make(chan struct{}, 1),
	}
}

// Ready is signalled when items are queued.
func (q *Queue) Ready() <-chan struct{} { return q.ready }

func (q *Queue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// Err returns ErrSlowConsumer once a hard limit was exceeded.
func (q *Queue) Err() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.err
}

// Pending returns the queued bytes.
func (q *Queue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.total
}

func (q *Queue) fail() error {
	q.err = ErrSlowConsumer

	return q.err
}

// PushJSON queues a text message. It is never dropped: above the JSON or
// total cap the queue fails with ErrSlowConsumer.
func (q *Queue) PushJSON(b []byte) error {
	q.mu.Lock()
	defer q.signal()
	defer q.mu.Unlock()

	return q.pushJSONLocked(b)
}

func (q *Queue) pushJSONLocked(b []byte) error {
	if q.err != nil {
		return q.err
	}

	if q.jsonBytes+len(b) > q.cfg.MaxJSONBytes || q.total+len(b) > q.cfg.MaxTotalBytes {
		return q.fail()
	}

	q.json = append(q.json, b)
	q.jsonBytes += len(b)
	q.total += len(b)

	return nil
}

// PushMeter queues a demod.meter text, replacing the pending one of key.
func (q *Queue) PushMeter(key string, b []byte) {
	q.mu.Lock()
	defer q.signal()
	defer q.mu.Unlock()

	if q.err != nil {
		return
	}

	for i := range q.meters {
		if q.meters[i].key == key {
			q.total += len(b) - len(q.meters[i].data)
			q.meters[i].data = b

			return
		}
	}

	q.meters = append(q.meters, meter{key: key, data: b})
	q.total += len(b)
}

// PushAudio queues an audio frame, dropping the oldest audio beyond the
// cap. A drop sets discontinuity on the next frame of the stream.
// ErrSlowConsumer means the audio overflow lasted too long or the total
// cap was hit.
func (q *Queue) PushAudio(f Frame) error {
	if !f.valid() || !f.Type.IsAudio() {
		return ErrInvalidFrame
	}

	q.mu.Lock()
	defer q.signal()
	defer q.mu.Unlock()

	if q.err != nil {
		return q.err
	}

	q.audio = append(q.audio, f)
	q.audioDur += f.Duration
	q.total += len(f.Payload) + rxv1.HeaderSize
	dropped := false

	for q.audioDur > q.cfg.AudioCap && len(q.audio) > 1 {
		old := q.audio[0]
		q.audio = q.audio[1:]
		q.audioDur -= old.Duration
		q.total -= len(old.Payload) + rxv1.HeaderSize
		q.gap[old.StreamID] = true
		dropped = true
	}

	now := q.now()
	if dropped {
		if q.lastAudioDrop.IsZero() || now.Sub(q.lastAudioDrop) >= q.cfg.AudioQuiet {
			q.overflowSince = now
		}

		q.lastAudioDrop = now

		if now.Sub(q.overflowSince) > q.cfg.AudioOverflow {
			return q.fail()
		}
	}

	if q.total > q.cfg.MaxTotalBytes {
		return q.fail()
	}

	return nil
}

// ConfigureFFT sets the requested rate and the pause state of an FFT
// stream (stream.configure). The effective rate restarts from fps.
func (q *Queue) ConfigureFFT(streamID uint16, fps int, paused bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	s := q.fftLocked(streamID)
	s.requested, s.fps, s.paused = max(fps, 1), max(fps, 1), paused

	if paused {
		for _, f := range s.pending {
			q.total -= len(f.Payload) + rxv1.HeaderSize
		}

		s.pending = nil
	}
}

// FPS returns the effective rate of an FFT stream.
func (q *Queue) FPS(streamID uint16) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	if s, ok := q.ffts[streamID]; ok {
		return s.fps
	}

	return 0
}

func (q *Queue) fftLocked(id uint16) *fftStream {
	s, ok := q.ffts[id]
	if !ok {
		s = &fftStream{requested: 1, fps: 1}
		q.ffts[id] = s
		q.order = append(q.order, id)
	}

	return s
}

// PushFFT offers an FFT frame: frames above the stream's effective rate
// are skipped, and only the latest FFTDepth frames are kept.
func (q *Queue) PushFFT(f Frame) error {
	if !f.valid() || !f.Type.IsFFT() {
		return ErrInvalidFrame
	}

	q.mu.Lock()
	defer q.signal()
	defer q.mu.Unlock()

	if q.err != nil {
		return q.err
	}

	s := q.fftLocked(f.StreamID)
	if s.paused {
		return nil
	}

	now := q.now()
	period := time.Second / time.Duration(s.fps)

	if !s.lastAccept.IsZero() && now.Sub(s.lastAccept) < period*9/10 {
		return nil
	}

	s.lastAccept = now

	if s.windowStart.IsZero() {
		s.windowStart = now
	}

	s.offered++
	s.pending = append(s.pending, f)
	q.total += len(f.Payload) + rxv1.HeaderSize

	for len(s.pending) > q.cfg.FFTDepth {
		q.total -= len(s.pending[0].Payload) + rxv1.HeaderSize
		s.pending = s.pending[1:]
		s.drops++
		s.lastDrop = now
	}

	q.adaptLocked(f.StreamID, s, now)

	if q.total > q.cfg.MaxTotalBytes {
		return q.fail()
	}

	return nil
}

// adaptLocked applies the fps policy of §6.8.
func (q *Queue) adaptLocked(id uint16, s *fftStream, now time.Time) {
	change := 0

	if now.Sub(s.windowStart) >= q.cfg.FFTWindow {
		if s.offered > 0 && float64(s.drops)/float64(s.offered) >= q.cfg.FFTDropRatio && s.fps > 1 {
			change = max(1, s.fps/2)
		}

		s.windowStart, s.offered, s.drops = now, 0, 0
	}

	if change == 0 && s.fps < s.requested && (s.lastDrop.IsZero() || now.Sub(s.lastDrop) >= q.cfg.FFTRecover) &&
		now.Sub(s.windowStart) == 0 {
		change = min(s.requested, s.fps*2)
		s.lastDrop = now
	}

	if change == 0 || change == s.fps {
		return
	}

	s.fps = change

	if q.update != nil {
		if b := q.update(id, change); b != nil {
			_ = q.pushJSONLocked(b)
		}
	}
}

// RemoveStream forgets a stream and drops its pending frames.
func (q *Queue) RemoveStream(id uint16) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if s, ok := q.ffts[id]; ok {
		for _, f := range s.pending {
			q.total -= len(f.Payload) + rxv1.HeaderSize
		}

		delete(q.ffts, id)

		for i, o := range q.order {
			if o == id {
				q.order = append(q.order[:i], q.order[i+1:]...)

				break
			}
		}
	}

	kept := q.audio[:0]

	for _, f := range q.audio {
		if f.StreamID == id {
			q.audioDur -= f.Duration
			q.total -= len(f.Payload) + rxv1.HeaderSize

			continue
		}

		kept = append(kept, f)
	}

	q.audio = kept

	delete(q.seq, id)
	delete(q.gap, id)
}

// Pop returns the next item in priority order: JSON, audio, meters, FFT
// (round robin over FFT streams).
func (q *Queue) Pop() (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	switch {
	case len(q.json) > 0:
		b := q.json[0]
		q.json[0] = nil
		q.json = q.json[1:]
		q.jsonBytes -= len(b)
		q.total -= len(b)

		return Item{Text: b}, true
	case len(q.audio) > 0:
		f := q.audio[0]
		q.audio = q.audio[1:]
		q.audioDur -= f.Duration
		q.total -= len(f.Payload) + rxv1.HeaderSize

		return q.binaryLocked(f), true
	case len(q.meters) > 0:
		m := q.meters[0]
		q.meters = q.meters[1:]
		q.total -= len(m.data)

		return Item{Text: m.data}, true
	}

	for range q.order {
		id := q.order[q.next%len(q.order)]
		q.next++

		s := q.ffts[id]
		if len(s.pending) == 0 {
			continue
		}

		f := s.pending[0]
		s.pending = s.pending[1:]
		q.total -= len(f.Payload) + rxv1.HeaderSize

		return q.binaryLocked(f), true
	}

	return Item{}, false
}

// binaryLocked numbers f in its stream and builds its header.
func (q *Queue) binaryLocked(f Frame) Item {
	flags := f.Flags
	if q.gap[f.StreamID] {
		flags |= rxv1.FlagDiscontinuity
		delete(q.gap, f.StreamID)
	}

	seq := q.seq[f.StreamID]
	q.seq[f.StreamID] = seq + 1

	// Frames were validated when pushed.
	h, _ := rxv1.AppendHeader(make([]byte, 0, rxv1.HeaderSize), rxv1.FrameHeader{
		Type: f.Type, Codec: f.Codec, StreamID: f.StreamID, Flags: flags, Seq: seq, TimestampUS: f.TimestampUS,
	}, len(f.Payload))

	return Item{Header: h, Payload: f.Payload}
}
