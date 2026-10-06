package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// DefaultHeartbeatInterval is used until the hub sends another one.
const DefaultHeartbeatInterval = 10 * time.Second

// Prober reports the node's capabilities and health samples.
type Prober interface {
	// Capabilities probes the node (seq is set by the agent).
	Capabilities(ctx context.Context) ctl.Capabilities
	// Heartbeat samples the node health (seq, clock offset and NTP state
	// are set by the agent).
	Heartbeat(ctx context.Context) ctl.Heartbeat
	// NTPSynced reports whether the system clock is synchronised.
	NTPSynced() bool
}

// Options configures an Agent.
type Options struct {
	NodeID  string
	Version string
	Buffer  *Buffer
	Prober  Prober
	Now     func() time.Time
	Logger  *slog.Logger
}

// Agent numbers node events, answers hub hellos and produces heartbeats.
type Agent struct {
	o    Options
	boot shared.UUID
	seq  atomic.Int64

	clockOffsetMS atomic.Int64

	mu       sync.Mutex
	interval time.Duration
	changed  chan struct{}
	capsHash string
}

// New returns an agent with a fresh boot id.
func New(o Options) (*Agent, error) {
	boot, err := shared.NewUUIDv7(o.Now())
	if err != nil {
		return nil, fmt.Errorf("boot id: %w", err)
	}

	return &Agent{o: o, boot: boot, interval: DefaultHeartbeatInterval, changed: make(chan struct{}, 1)}, nil
}

// BootID returns the id of this node process.
func (a *Agent) BootID() shared.UUID { return a.boot }

// Buffer returns the event buffer.
func (a *Agent) Buffer() *Buffer { return a.o.Buffer }

// Emit numbers the payload built by build and buffers it.
func (a *Agent) Emit(typ rxv1.MessageType, key string, class Class, build func(seq int64) any) {
	seq := a.seq.Add(1)
	payload := build(seq)

	raw, err := json.Marshal(payload)
	if err != nil {
		a.o.Logger.Error("encode node event", slog.String("type", string(typ)), slog.Any("error", err))

		return
	}

	a.o.Buffer.Push(Event{Seq: seq, Type: typ, Payload: json.RawMessage(raw), Key: key, Class: class, Size: len(raw)})
}

// Welcome answers a hub hello received at receivedAt and adopts its
// heartbeat interval. rtt, when known, corrects the clock offset (§4.5).
func (a *Agent) Welcome(h ctl.Hello, receivedAt time.Time) ctl.Welcome {
	a.SetClock(h.ServerTime, receivedAt, 0)

	if h.HeartbeatIntervalMS > 0 {
		a.mu.Lock()
		a.interval = time.Duration(h.HeartbeatIntervalMS) * time.Millisecond
		a.mu.Unlock()

		select {
		case a.changed <- struct{}{}:
		default:
		}
	}

	a.mu.Lock()
	hash := a.capsHash
	a.mu.Unlock()

	return ctl.Welcome{
		NodeID: a.o.NodeID, BootID: a.boot.String(), Version: a.o.Version,
		Protocols: []string{rxv1.ControlSubprotocol, rxv1.Subprotocol}, CapabilitiesHash: hash,
	}
}

// SetClock records the offset between the hub time serverTimeMS, received
// at receivedAt, and the local clock, corrected by rtt/2.
func (a *Agent) SetClock(serverTimeMS int64, receivedAt time.Time, rtt time.Duration) {
	a.clockOffsetMS.Store(serverTimeMS + rtt.Milliseconds()/2 - receivedAt.UnixMilli())
}

// ClockOffsetMS returns the last measured hub − node clock offset.
func (a *Agent) ClockOffsetMS() int64 { return a.clockOffsetMS.Load() }

// EmitCapabilities probes and buffers node.capabilities.
func (a *Agent) EmitCapabilities(ctx context.Context) {
	caps := a.o.Prober.Capabilities(ctx)

	raw, _ := json.Marshal(caps)
	sum := sha256.Sum256(raw)

	a.mu.Lock()
	a.capsHash = hex.EncodeToString(sum[:8])
	a.mu.Unlock()

	a.Emit(rxv1.TypeNodeCapabilities, "capabilities", ClassState, func(seq int64) any {
		caps.Seq = seq

		return caps
	})
}

// EmitDropped reports the events dropped since the last report, if any.
func (a *Agent) EmitDropped() {
	count, kinds := a.o.Buffer.TakeDropped()
	if count == 0 {
		return
	}

	a.o.Logger.Warn("node events were dropped while the hub was unreachable", slog.Int64("count", count))
	a.Emit(rxv1.TypeNodeEventsDropped, "", ClassState, func(seq int64) any {
		return ctl.EventsDropped{Seq: seq, Count: count, Kinds: kinds}
	})
}

// EmitHeartbeat samples and buffers node.heartbeat (coalesced).
func (a *Agent) EmitHeartbeat(ctx context.Context) {
	hb := a.o.Prober.Heartbeat(ctx)
	hb.ClockOffsetMS = a.ClockOffsetMS()
	hb.NTPSynced = a.o.Prober.NTPSynced()

	a.Emit(rxv1.TypeNodeHeartbeat, "heartbeat", ClassState, func(seq int64) any {
		hb.Seq = seq

		return hb
	})
}

// Run emits a heartbeat every interval until ctx is done.
func (a *Agent) Run(ctx context.Context) {
	for {
		a.mu.Lock()
		interval := a.interval
		a.mu.Unlock()

		t := time.NewTimer(interval)

		select {
		case <-ctx.Done():
			t.Stop()

			return
		case <-a.changed:
			t.Stop()
		case <-t.C:
			a.EmitHeartbeat(ctx)
		}
	}
}
