// Package session is a throwaway rx.v1 media-WS server and measuring client
// used to compare WebSocket libraries under back-pressure.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/spikes/spk-07-ws/sendq"
	"github.com/yohang/mesh-sdr/spikes/spk-07-ws/transport"
)

// Stream ids used by the fake node.
const (
	FFTStreamID   uint16 = 1
	AudioStreamID uint16 = 2
)

// Config describes the fake DSP output.
type Config struct {
	FFTSize      int           // bins (u8 dB)
	FFTFPS       int           // nominal fps
	AudioFrame   time.Duration // audio frame duration
	AudioPayload int           // bytes per audio frame (ADPCM nibbles)
	WriteTimeout time.Duration // per-message write timeout
	Policy       sendq.Policy
}

// DefaultConfig: 4096-bin FFT at 25 fps (~103 KB/s) and IMA ADPCM 48 kHz
// mono in 20 ms frames (~25 KB/s).
func DefaultConfig() Config {
	return Config{
		FFTSize:      4096,
		FFTFPS:       25,
		AudioFrame:   20 * time.Millisecond,
		AudioPayload: 480,
		WriteTimeout: 30 * time.Second,
		Policy:       sendq.DefaultPolicy(),
	}
}

// ServerStats aggregates every session of a server.
type ServerStats struct {
	Sessions       atomic.Int64
	Closed4413     atomic.Int64
	FPSHalvings    atomic.Int64
	FPSRaises      atomic.Int64
	WriteErrors    atomic.Int64
	FramesWritten  atomic.Int64
	BytesWritten   atomic.Int64
	FFTEnqueued    atomic.Int64
	FFTDropped     atomic.Int64
	AudioEnqueued  atomic.Int64
	AudioDropped   atomic.Int64
	AudioDropRuns  atomic.Int64
	PeakQueueBytes atomic.Int64
}

// Server is a fake node: one shared DSP producer fanned out to sessions.
type Server struct {
	lib   transport.Lib
	cfg   Config
	Stats *ServerStats

	mu       sync.Mutex
	sessions map[*session]struct{}
}

// NewServer builds a server for lib.
func NewServer(lib transport.Lib, cfg Config) *Server {
	return &Server{lib: lib, cfg: cfg, Stats: &ServerStats{}, sessions: map[*session]struct{}{}}
}

// Run produces FFT and audio frames until ctx is done. Each frame is encoded
// once and its buffer shared by every session queue (fan-out).
func (s *Server) Run(ctx context.Context) {
	fftTick := time.NewTicker(time.Second / time.Duration(s.cfg.FFTFPS))
	audioTick := time.NewTicker(s.cfg.AudioFrame)
	defer fftTick.Stop()
	defer audioTick.Stop()
	bins := make([]byte, s.cfg.FFTSize)
	for i := range bins {
		bins[i] = byte(i)
	}
	fftPayload := rxv1.AppendFFTU8(nil, rxv1.DefaultFFTU8Scale(), bins)
	audioPayload := rxv1.AppendADPCM(nil, rxv1.ADPCMState{}, make([]byte, s.cfg.AudioPayload))
	var fftSeq, audioSeq uint32
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-fftTick.C:
			fftSeq++
			h := rxv1.FrameHeader{Type: rxv1.FrameFFT, Codec: rxv1.CodecFFTU8DB, StreamID: FFTStreamID, Seq: fftSeq, TimestampUS: uint64(now.UnixMicro())}
			frame, _ := rxv1.AppendFrame(nil, h, fftPayload)
			s.each(func(ss *session) { ss.pushFFT(frame) })
		case now := <-audioTick.C:
			audioSeq++
			h := rxv1.FrameHeader{Type: rxv1.FrameAudio, Codec: rxv1.CodecADPCMIMA, StreamID: AudioStreamID, Seq: audioSeq, TimestampUS: uint64(now.UnixMicro())}
			frame, _ := rxv1.AppendFrame(nil, h, audioPayload)
			s.each(func(ss *session) { ss.pushAudio(frame) })
		}
	}
}

func (s *Server) each(f func(*session)) {
	s.mu.Lock()
	list := make([]*session, 0, len(s.sessions))
	for ss := range s.sessions {
		list = append(list, ss)
	}
	s.mu.Unlock()
	for _, ss := range list {
		f(ss)
	}
}

// ServeHTTP is the /ws endpoint.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !transport.RequireSubprotocol(w, r, rxv1.Subprotocol) {
		return
	}
	c, err := s.lib.Accept(w, r, rxv1.MaxInboundTextBytes)
	if err != nil {
		return
	}
	if c.Subprotocol() != rxv1.Subprotocol { // defensive: cannot happen after the pre-check
		_ = c.Close(rxv1.ClosePolicyViolation, "subprotocol")
		return
	}
	s.Stats.Sessions.Add(1)
	ss := &session{srv: s, conn: c, q: sendq.New(s.cfg.Policy, nil), decim: 1}
	ss.q.OpenAudio(AudioStreamID, s.cfg.AudioFrame)
	ss.q.OpenFFT(FFTStreamID)
	ss.run(r.Context())
}

type session struct {
	srv  *Server
	conn transport.Conn
	q    *sendq.Queue

	mu       sync.Mutex
	started  bool
	decim    int    // FFT decimation: 1, 2, 4… (fps halving)
	fftCount uint64 // shared frames seen
	fftSeq   uint32 // per-connection FFT seq after decimation
	lastDrop time.Time
}

func (ss *session) pushFFT(shared []byte) {
	ss.mu.Lock()
	if !ss.started {
		ss.mu.Unlock()
		return
	}
	ss.fftCount++
	if ss.fftCount%uint64(ss.decim) != 0 {
		ss.mu.Unlock()
		return
	}
	ss.fftSeq++
	seq := ss.fftSeq
	ss.mu.Unlock()
	// Per-connection seq: copy the frame and rewrite the header (24 bytes).
	f := append([]byte(nil), shared...)
	h, payload, _ := rxv1.ParseFrame(f)
	h.Seq = seq
	f, _ = rxv1.AppendFrame(f[:0:0], h, payload)
	if err := ss.q.PushFFT(FFTStreamID, f); err != nil {
		ss.q.Close()
	}
}

func (ss *session) pushAudio(shared []byte) {
	ss.mu.Lock()
	started := ss.started
	ss.mu.Unlock()
	if !started {
		return
	}
	_ = ss.q.PushAudio(AudioStreamID, shared) // a failure is seen by the writer
}

func (ss *session) pushJSON(typ rxv1.MessageType, payload any) {
	env, err := rxv1.NewEnvelope(typ, rxv1.CorrelationID{}, time.Now().UnixMilli(), payload)
	if err != nil {
		panic(err)
	}
	b, _ := json.Marshal(env)
	_ = ss.q.PushJSON(b)
}

func (ss *session) run(parent context.Context) {
	srv := ss.srv
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	srv.mu.Lock()
	srv.sessions[ss] = struct{}{}
	srv.mu.Unlock()
	defer func() {
		srv.mu.Lock()
		delete(srv.sessions, ss)
		srv.mu.Unlock()
		st := ss.q.Stats()
		srv.Stats.FFTEnqueued.Add(int64(st.FFTEnqueued))
		srv.Stats.FFTDropped.Add(int64(st.FFTDropped))
		srv.Stats.AudioEnqueued.Add(int64(st.AudioEnqueued))
		srv.Stats.AudioDropped.Add(int64(st.AudioDropped))
		srv.Stats.AudioDropRuns.Add(int64(st.AudioDropRuns))
		for {
			old := srv.Stats.PeakQueueBytes.Load()
			if int64(st.PeakBytes) <= old || srv.Stats.PeakQueueBytes.CompareAndSwap(old, int64(st.PeakBytes)) {
				break
			}
		}
	}()

	// Handshake: session.hello within 5 s (§6.2), else 4408. A timer +
	// Close is used instead of a Read deadline: with coder/websocket an
	// expired Read context closes the connection without a close code, and
	// with gorilla a read timeout leaves the connection unusable.
	timedOut := make(chan struct{})
	hsTimer := time.AfterFunc(5*time.Second, func() {
		close(timedOut)
		_ = ss.conn.Close(rxv1.CloseHandshakeTimeout, "handshake timeout")
	})
	bin, msg, err := ss.conn.Read(ctx)
	if !hsTimer.Stop() {
		<-timedOut
		return
	}
	if err != nil {
		_ = ss.conn.CloseNow()
		return
	}
	if bin {
		_ = ss.conn.Close(rxv1.CloseUnsupportedData, "binary frame")
		return
	}
	env, err := rxv1.DecodeEnvelope(msg)
	if err != nil || env.Type() != rxv1.TypeSessionHello {
		_ = ss.conn.Close(rxv1.CloseProtocolViolations, "expected session.hello")
		return
	}
	ss.pushJSON(rxv1.TypeSessionWelcome, map[string]any{"cid": "c1", "server": map[string]string{"protocol": rxv1.Subprotocol}})
	ss.pushJSON(rxv1.TypeStreamOpen, map[string]any{"stream_id": FFTStreamID, "kind": "fft", "codec": "u8-db", "fft": map[string]int{"size": srv.cfg.FFTSize}})
	ss.pushJSON(rxv1.TypeStreamOpen, map[string]any{"stream_id": AudioStreamID, "kind": "audio", "codec": "adpcm-ima", "sample_rate": 48000})
	ss.mu.Lock()
	ss.started = true
	ss.mu.Unlock()

	// Reader: client binary → 1003; any read error ends the session.
	go func() {
		defer cancel()
		for {
			bin, _, err := ss.conn.Read(ctx)
			if err != nil {
				return
			}
			if bin {
				_ = ss.conn.Close(rxv1.CloseUnsupportedData, "binary frame")
				return
			}
		}
	}()

	// FPS adaptation (§6.8): ≥ 25 % FFT dropped over 5 s → halve; 30 s
	// without drops → double back.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				enq, drop := ss.q.TakeFFTWindow(FFTStreamID)
				ss.mu.Lock()
				fps := srv.cfg.FFTFPS / ss.decim
				changed := false
				switch {
				case enq > 0 && drop*4 >= enq && fps > 1:
					ss.decim *= 2
					ss.lastDrop = now
					changed = true
					srv.Stats.FPSHalvings.Add(1)
				case drop > 0:
					ss.lastDrop = now
				case ss.decim > 1 && now.Sub(ss.lastDrop) >= 30*time.Second:
					ss.decim /= 2
					ss.lastDrop = now
					changed = true
					srv.Stats.FPSRaises.Add(1)
				}
				fps = srv.cfg.FFTFPS / ss.decim
				ss.mu.Unlock()
				if changed {
					ss.pushJSON(rxv1.TypeStreamUpdate, map[string]any{"stream_id": FFTStreamID, "fps": fps})
				}
			}
		}
	}()

	// Slow-consumer watcher: closes with 4413 even while the writer is
	// blocked in a write on a full TCP buffer.
	go func() {
		select {
		case <-ctx.Done():
		case <-ss.q.Failed():
			if errors.Is(ss.q.Err(), sendq.ErrSlowConsumer) {
				srv.Stats.Closed4413.Add(1)
				_ = ss.conn.Close(rxv1.CloseSlowConsumer, "slow consumer")
			}
			cancel()
		}
	}()

	// Writer: the only goroutine that writes data frames.
	for {
		it, err := ss.q.Next(ctx)
		if err != nil {
			<-ctx.Done() // watcher or reader ends the connection
			_ = ss.conn.CloseNow()
			return
		}
		wctx, wcancel := context.WithTimeout(ctx, srv.cfg.WriteTimeout)
		if it.Kind == sendq.Text {
			err = ss.conn.WriteText(wctx, it.Data)
		} else {
			err = ss.conn.WriteBinary(wctx, it.Data)
		}
		wcancel()
		if err != nil {
			if ss.q.Err() == nil {
				srv.Stats.WriteErrors.Add(1)
			}
			ss.q.Close()
			<-ctx.Done()
			_ = ss.conn.CloseNow()
			return
		}
		srv.Stats.FramesWritten.Add(1)
		srv.Stats.BytesWritten.Add(int64(len(it.Data)))
	}
}
