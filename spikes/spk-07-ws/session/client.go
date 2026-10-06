package session

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/spikes/spk-07-ws/transport"
)

// ClientConfig shapes a measuring client.
type ClientConfig struct {
	URL       string
	RateBps   int           // 0 = unlimited; otherwise read throughput cap (bytes/s)
	StallAt   time.Duration // > 0: stop reading entirely after this delay
	Duration  time.Duration
	RcvBuf    int
	AudioStep time.Duration // nominal audio frame duration
}

// StreamStats are per-stream client observations.
type StreamStats struct {
	Received       int
	GapEvents      int // seq jumps
	Missing        int // frames missing according to seq
	FlaggedGaps    int // gap events whose frame had the discontinuity flag
	Discontinuity  int // frames carrying the discontinuity flag
	LatenciesMS    []float64
	LongestGapMS   float64 // longest seq gap × frame duration (audio)
	LastSeq        uint32
	seen           bool
	ParseErrors    int
	MaxInterArrive time.Duration
	lastArrive     time.Time
}

// ClientResult is what one client observed.
type ClientResult struct {
	Audio, FFT    StreamStats
	StreamUpdates int
	LastFPS       int
	CloseCode     int
	Err           error
	Bytes         int64
}

// RunClient connects with lib, sends session.hello and reads until
// Duration elapses or the server closes.
func RunClient(ctx context.Context, lib transport.Lib, cfg ClientConfig) ClientResult {
	var res ClientResult
	ctx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()
	c, _, err := lib.Dial(ctx, cfg.URL, []string{rxv1.Subprotocol}, cfg.RcvBuf)
	if err != nil {
		res.Err = err
		return res
	}
	defer func() { _ = c.CloseNow() }()
	hello, _ := rxv1.NewEnvelope(rxv1.TypeSessionHello, rxv1.CorrelationID{}, time.Now().UnixMilli(),
		map[string]any{"client": map[string]string{"name": "spk-07", "version": "0"}})
	b, _ := json.Marshal(hello)
	if err := c.WriteText(ctx, b); err != nil {
		res.Err = err
		return res
	}

	start := time.Now()
	for {
		if cfg.StallAt > 0 && time.Since(start) > cfg.StallAt {
			<-ctx.Done() // stop reading: the server must cope
			break
		}
		bin, msg, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				res.CloseCode = lib.CloseCode(err)
				res.Err = err
			}
			break
		}
		res.Bytes += int64(len(msg))
		now := time.Now()
		if !bin {
			env, err := rxv1.DecodeEnvelope(msg)
			if err == nil && env.Type() == rxv1.TypeStreamUpdate {
				res.StreamUpdates++
				var p struct {
					FPS int `json:"fps"`
				}
				_ = env.DecodePayload(&p, false)
				res.LastFPS = p.FPS
			}
		} else {
			h, _, err := rxv1.ParseFrame(msg)
			st := &res.FFT
			if h.Type.IsAudio() {
				st = &res.Audio
			}
			if err != nil {
				st.ParseErrors++
				continue
			}
			st.Received++
			if h.Flags.Has(rxv1.FlagDiscontinuity) {
				st.Discontinuity++
			}
			if st.seen {
				if gap := rxv1.SeqGap(st.LastSeq, h.Seq); gap > 0 && gap < 1<<31 {
					st.GapEvents++
					st.Missing += int(gap)
					if h.Flags.Has(rxv1.FlagDiscontinuity) {
						st.FlaggedGaps++
					}
					if g := float64(gap) * float64(cfg.AudioStep) / float64(time.Millisecond); h.Type.IsAudio() && g > st.LongestGapMS {
						st.LongestGapMS = g
					}
				}
				if d := now.Sub(st.lastArrive); d > st.MaxInterArrive {
					st.MaxInterArrive = d
				}
			}
			st.seen, st.LastSeq, st.lastArrive = true, h.Seq, now
			st.LatenciesMS = append(st.LatenciesMS, float64(now.UnixMicro()-int64(h.TimestampUS))/1000)
		}
		if cfg.RateBps > 0 {
			want := time.Duration(float64(res.Bytes) / float64(cfg.RateBps) * float64(time.Second))
			if el := time.Since(start); want > el {
				select {
				case <-time.After(want - el):
				case <-ctx.Done():
				}
			}
		}
	}
	return res
}

// Percentile returns the p-th percentile (0–100) of xs.
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	i := int(p / 100 * float64(len(s)-1))
	return s[i]
}
