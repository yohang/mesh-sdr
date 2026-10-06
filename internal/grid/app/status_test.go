package app_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

func TestStatusEvaluate(t *testing.T) {
	timings := app.DefaultTimings() // heartbeat 10 s, offline after 60 s
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := app.NewStatus(nil, nil, app.NewTracker(), app.NewHistory(), timings, func() time.Time { return now }, discard)

	tok, _ := domain.NewEnrollmentToken()
	pending := domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("a"), domain.MustNodeURL("https://x:1"), now)
	pending.IssueEnrollmentKey(tok.Key(), time.Time{}, now)

	enrolled := domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("a"), domain.MustNodeURL("https://x:1"), now)
	enrolled.IssueEnrollmentKey(tok.Key(), time.Time{}, now)

	cert, _ := domain.NewCertInfo(make([]byte, 32), "01", now.Add(time.Hour))
	_ = enrolled.CompleteEnrollment(tok.Key(), enrolled.URL(), cert, now)

	ok := domain.Compatibility{Level: domain.CompatOK}
	live := app.LinkState{Connected: true, EverWelcomed: true, Compat: ok, WelcomedAt: now.Add(-time.Minute), LastHeartbeat: now.Add(-5 * time.Second), NTPSynced: true}

	with := func(f func(*app.LinkState)) app.LinkState {
		l := live
		f(&l)

		return l
	}

	tests := []struct {
		name   string
		node   *domain.Node
		link   app.LinkState
		known  bool
		status domain.Status
		hint   string
	}{
		{"pending node", pending, live, true, domain.StatusOffline, ""},
		{"never seen", enrolled, app.LinkState{}, false, domain.StatusOffline, ""},
		{"dial failures before any welcome", enrolled, app.LinkState{DialFailures: 2}, true, domain.StatusUnreachable, app.HintDialFailed},
		{"healthy", enrolled, live, true, domain.StatusOnline, ""},
		{"two missed heartbeats", enrolled, with(func(l *app.LinkState) { l.LastHeartbeat = now.Add(-26 * time.Second) }), true, domain.StatusDegraded, app.HintMissedHeartbeats},
		{"silent past offline_after", enrolled, with(func(l *app.LinkState) { l.LastHeartbeat = now.Add(-61 * time.Second) }), true, domain.StatusOffline, app.HintHeartbeatTimeout},
		{"clock offset", enrolled, with(func(l *app.LinkState) { l.ClockOffsetMS = -1500 }), true, domain.StatusDegraded, app.HintClockOffset},
		{"ntp unsynced", enrolled, with(func(l *app.LinkState) { l.NTPSynced = false }), true, domain.StatusDegraded, app.HintNTPUnsynced},
		{"older minor", enrolled, with(func(l *app.LinkState) {
			l.Compat = domain.Compatibility{Level: domain.CompatOlder, Hint: domain.HintUpgradeRecommended}
		}), true, domain.StatusDegraded, domain.HintUpgradeRecommended},
		{"incompatible", enrolled, with(func(l *app.LinkState) {
			l.Compat = domain.Compatibility{Level: domain.CompatIncompatible, Hint: domain.HintNodeTooOld}
		}), true, domain.StatusIncompatible, domain.HintNodeTooOld},
		{"channel lost", enrolled, with(func(l *app.LinkState) { l.Connected = false }), true, domain.StatusOffline, app.HintChannelLost},
	}

	for _, tt := range tests {
		status, hint := s.Evaluate(tt.node, tt.link, tt.known, now)
		if status != tt.status || hint != tt.hint {
			t.Errorf("%s: %s %q, want %s %q", tt.name, status, hint, tt.status, tt.hint)
		}
	}
}

func TestHeartbeatUpdatesNodeAndHistory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := enrolledNode(t, e)
	c, tr := newControl(e, "1.0.0")
	h := app.NewHistory()
	s := app.NewStatus(e.nodes, e.db, tr, h, app.DefaultTimings(), e.clock.now, discard)

	var transitions []domain.Status

	s.Listen(func(_ context.Context, _ domain.NodeID, st domain.Status, _ string) {
		transitions = append(transitions, st)
	})
	c.Handle(rxv1.TypeNodeHeartbeat, s.HeartbeatHandler())
	c.OnLinkChange(s.Refresh)

	boot := welcome(t, c, n.ID(), "1.0.0")

	payload, _ := json.Marshal(map[string]any{"seq": 1, "cpu": 0.5, "load": []float64{1, 1, 1}, "clock_offset_ms": 12, "ntp_synced": true,
		"mem": map[string]any{"total_bytes": 100, "available_bytes": 40}})

	if _, err := c.Apply(ctx, n.ID(), boot, false, []app.Event{{Seq: 1, Type: rxv1.TypeNodeHeartbeat, Payload: payload}}); err != nil {
		t.Fatal(err)
	}

	got, _ := e.nodes.Get(ctx, n.ID())
	if rt := got.Runtime(); rt.Status != domain.StatusOnline || rt.ClockOffsetMS == nil || *rt.ClockOffsetMS != 12 || rt.LastHeartbeatAt.IsZero() {
		t.Errorf("runtime = %+v", rt)
	}

	if samples := h.Samples(n.ID()); len(samples) != 1 || samples[0].CPU != 0.5 || samples[0].MemAvailableBytes != 40 {
		t.Errorf("history = %+v", samples)
	}

	// Silence: degraded, then offline.
	e.clock.advance(30 * time.Second)
	s.Sweep(ctx)
	e.clock.advance(31 * time.Second)
	s.Sweep(ctx)

	want := []domain.Status{domain.StatusOnline, domain.StatusDegraded, domain.StatusOffline}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}

	for i := range want {
		if transitions[i] != want[i] {
			t.Errorf("transitions = %v, want %v", transitions, want)
		}
	}
}
