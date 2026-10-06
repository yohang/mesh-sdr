// Package sendq is a library-agnostic per-connection send queue implementing
// the TECHNICAL_SPEC §6.8 back-pressure and drop policy:
//
//   - JSON control messages: never dropped, capped at 1 MiB pending → slow consumer;
//   - demod.meter: coalesced per demod (latest wins);
//   - FFT: latest-wins, at most 2 pending frames per stream;
//   - audio: highest binary priority, capped at 500 ms per stream, drop
//     oldest on overflow and flag the next frame as a discontinuity; a
//     sustained overflow for > 10 s → slow consumer (see PushAudio for the
//     interpretation of "backlog above cap");
//   - per-connection total of 4 MiB pending → slow consumer.
//
// Producers (DSP fan-out) never block: Push* only takes a mutex. One writer
// goroutine per connection drains the queue with Next and writes to the
// WebSocket with whatever library is in use.
//
// Dequeue priority: JSON, audio, meters, FFT. JSON goes first because
// stream.open must reach the client before the first binary frame of a
// stream (§6.5); see the ADR for the open question.
package sendq

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// ErrSlowConsumer means a §6.8 cap was exceeded: the connection must be
// closed with 4413.
var ErrSlowConsumer = errors.New("sendq: slow consumer")

// ErrClosed is returned by Next after Close.
var ErrClosed = errors.New("sendq: closed")

// Policy holds the §6.8 limits. DefaultPolicy returns the spec values.
type Policy struct {
	MaxJSONBytes        int
	MaxTotalBytes       int
	FFTPendingPerStream int
	AudioMaxBacklog     time.Duration
	AudioStuckTimeout   time.Duration
	AudioOverflowGap    time.Duration // drop-free time that ends an overflow period (spike interpretation)
}

// DefaultPolicy returns the TECHNICAL_SPEC §6.8 values.
func DefaultPolicy() Policy {
	return Policy{
		MaxJSONBytes:        1 << 20,
		MaxTotalBytes:       4 << 20,
		FFTPendingPerStream: 2,
		AudioMaxBacklog:     500 * time.Millisecond,
		AudioStuckTimeout:   10 * time.Second,
		AudioOverflowGap:    time.Second,
	}
}

// Kind tells the writer which WebSocket message type to use.
type Kind uint8

// Message kinds.
const (
	Text Kind = iota
	Binary
)

// Item is one message ready to write.
type Item struct {
	Kind Kind
	Data []byte
}

type audioStream struct {
	frames     [][]byte
	maxFrames  int
	markNext   bool      // set discontinuity on the next dequeued frame
	dropSince  time.Time // start of the current sustained-overflow period
	lastDrop   time.Time
	enqueued   uint64
	dropped    uint64
	dropEvents uint64
}

type fftStream struct {
	frames   [][]byte
	enqueued uint64
	dropped  uint64
	// window counters for fps adaptation
	winEnq, winDrop uint64
}

// Stats is a snapshot of queue counters.
type Stats struct {
	JSONEnqueued   uint64
	MeterCoalesced uint64
	FFTEnqueued    uint64
	FFTDropped     uint64
	AudioEnqueued  uint64
	AudioDropped   uint64
	AudioDropRuns  uint64
	PeakBytes      int
	CopiesOnMark   uint64
}

// Queue is safe for concurrent producers and one consumer.
type Queue struct {
	policy Policy
	now    func() time.Time

	mu        sync.Mutex
	notify    chan struct{}
	failed    chan struct{}
	err       error
	json      [][]byte
	jsonBytes int
	meters    map[string][]byte
	meterKeys []string
	audio     map[uint16]*audioStream
	audioIDs  []uint16
	fft       map[uint16]*fftStream
	fftIDs    []uint16
	total     int
	rrAudio   int
	rrFFT     int
	stats     Stats
}

// New builds a queue. now is injectable for tests (nil = time.Now).
func New(p Policy, now func() time.Time) *Queue {
	if now == nil {
		now = time.Now
	}
	return &Queue{
		policy: p,
		now:    now,
		notify: make(chan struct{}, 1),
		failed: make(chan struct{}),
		meters: map[string][]byte{},
		audio:  map[uint16]*audioStream{},
		fft:    map[uint16]*fftStream{},
	}
}

// OpenAudio registers an audio stream whose frames last frameDur.
func (q *Queue) OpenAudio(id uint16, frameDur time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := int(q.policy.AudioMaxBacklog / frameDur)
	if n < 1 {
		n = 1
	}
	q.audio[id] = &audioStream{maxFrames: n}
	q.audioIDs = append(q.audioIDs, id)
}

// OpenFFT registers an FFT stream.
func (q *Queue) OpenFFT(id uint16) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.fft[id] = &fftStream{}
	q.fftIDs = append(q.fftIDs, id)
}

func (q *Queue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *Queue) fail(err error) error {
	if q.err == nil {
		q.err = err
		close(q.failed)
		q.signal()
	}
	return q.err
}

func (q *Queue) grow(n int) error {
	q.total += n
	if q.total > q.stats.PeakBytes {
		q.stats.PeakBytes = q.total
	}
	if q.total > q.policy.MaxTotalBytes {
		return q.fail(ErrSlowConsumer)
	}
	return nil
}

// PushJSON enqueues a never-dropped JSON message.
func (q *Queue) PushJSON(b []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	q.json = append(q.json, b)
	q.jsonBytes += len(b)
	q.stats.JSONEnqueued++
	if q.jsonBytes > q.policy.MaxJSONBytes {
		return q.fail(ErrSlowConsumer)
	}
	if err := q.grow(len(b)); err != nil {
		return err
	}
	q.signal()
	return nil
}

// PushMeter enqueues a demod.meter, replacing any pending one for demodID.
func (q *Queue) PushMeter(demodID string, b []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	if old, ok := q.meters[demodID]; ok {
		q.total -= len(old)
		q.stats.MeterCoalesced++
	} else {
		q.meterKeys = append(q.meterKeys, demodID)
	}
	q.meters[demodID] = b
	if err := q.grow(len(b)); err != nil {
		return err
	}
	q.signal()
	return nil
}

// PushFFT enqueues an encoded FFT frame; the oldest pending frame of the
// stream is dropped beyond FFTPendingPerStream. frame is not modified.
func (q *Queue) PushFFT(id uint16, frame []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	s := q.fft[id]
	s.enqueued++
	s.winEnq++
	q.stats.FFTEnqueued++
	if len(s.frames) >= q.policy.FFTPendingPerStream {
		q.total -= len(s.frames[0])
		s.frames[0] = nil
		s.frames = s.frames[1:]
		s.dropped++
		s.winDrop++
		q.stats.FFTDropped++
	}
	s.frames = append(s.frames, frame)
	if err := q.grow(len(frame)); err != nil {
		return err
	}
	q.signal()
	return nil
}

// PushAudio enqueues an encoded audio frame. On overflow the oldest frame is
// dropped and the next dequeued frame gets the discontinuity flag (on a copy:
// frame buffers may be shared by every connection of a fan-out).
func (q *Queue) PushAudio(id uint16, frame []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	s := q.audio[id]
	s.enqueued++
	q.stats.AudioEnqueued++
	if len(s.frames) >= s.maxFrames {
		q.total -= len(s.frames[0])
		s.frames[0] = nil
		s.frames = s.frames[1:]
		if !s.markNext {
			s.dropEvents++
			q.stats.AudioDropRuns++
		}
		s.dropped++
		q.stats.AudioDropped++
		s.markNext = true
		// "Backlog above cap for > 10 s" cannot be observed literally with
		// drop-oldest (the backlog never exceeds the cap). Interpretation
		// used here: overflow drops keep happening, with no drop-free gap of
		// AudioOverflowGap, for longer than AudioStuckTimeout.
		now := q.now()
		if s.dropSince.IsZero() || now.Sub(s.lastDrop) > q.policy.AudioOverflowGap {
			s.dropSince = now
		}
		s.lastDrop = now
		if now.Sub(s.dropSince) > q.policy.AudioStuckTimeout {
			return q.fail(ErrSlowConsumer)
		}
	}
	s.frames = append(s.frames, frame)
	if err := q.grow(len(frame)); err != nil {
		return err
	}
	q.signal()
	return nil
}

// Next blocks until an item is available, the queue failed, or ctx is done.
func (q *Queue) Next(ctx context.Context) (Item, error) {
	for {
		q.mu.Lock()
		if q.err != nil {
			err := q.err
			q.mu.Unlock()
			return Item{}, err
		}
		if it, ok := q.pop(); ok {
			q.mu.Unlock()
			return it, nil
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return Item{}, ctx.Err()
		case <-q.notify:
		}
	}
}

func (q *Queue) pop() (Item, bool) {
	if len(q.json) > 0 {
		b := q.json[0]
		q.json[0] = nil
		q.json = q.json[1:]
		q.jsonBytes -= len(b)
		q.total -= len(b)
		return Item{Kind: Text, Data: b}, true
	}
	for i := range q.audioIDs {
		idx := (q.rrAudio + i) % len(q.audioIDs)
		s := q.audio[q.audioIDs[idx]]
		if len(s.frames) == 0 {
			continue
		}
		q.rrAudio = idx + 1
		b := s.frames[0]
		s.frames[0] = nil
		s.frames = s.frames[1:]
		q.total -= len(b)
		if s.markNext {
			b = append([]byte(nil), b...)
			_ = rxv1.SetFlags(b, rxv1.FlagDiscontinuity)
			s.markNext = false
			q.stats.CopiesOnMark++
		}
		return Item{Kind: Binary, Data: b}, true
	}
	if len(q.meterKeys) > 0 {
		k := q.meterKeys[0]
		q.meterKeys = q.meterKeys[1:]
		b := q.meters[k]
		delete(q.meters, k)
		q.total -= len(b)
		return Item{Kind: Text, Data: b}, true
	}
	for i := range q.fftIDs {
		idx := (q.rrFFT + i) % len(q.fftIDs)
		s := q.fft[q.fftIDs[idx]]
		if len(s.frames) == 0 {
			continue
		}
		q.rrFFT = idx + 1
		b := s.frames[0]
		s.frames[0] = nil
		s.frames = s.frames[1:]
		q.total -= len(b)
		return Item{Kind: Binary, Data: b}, true
	}
	return Item{}, false
}

// TakeFFTWindow returns and resets the enqueued/dropped counters of an FFT
// stream since the previous call, for the 25 %/5 s fps adaptation rule.
func (q *Queue) TakeFFTWindow(id uint16) (enqueued, dropped uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.fft[id]
	enqueued, dropped = s.winEnq, s.winDrop
	s.winEnq, s.winDrop = 0, 0
	return enqueued, dropped
}

// Stats returns a snapshot of the counters.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stats
}

// Pending returns the pending byte count.
func (q *Queue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.total
}

// Close fails the queue with ErrClosed (if not already failed).
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = q.fail(ErrClosed)
}

// Failed is closed when the queue fails (slow consumer or Close), so that a
// watcher can close the connection even while the writer is blocked.
func (q *Queue) Failed() <-chan struct{} { return q.failed }

// Err returns the failure, if any.
func (q *Queue) Err() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.err
}
