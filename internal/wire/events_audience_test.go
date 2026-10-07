package wire

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	eventsapp "github.com/yohang/mesh-sdr/internal/events/app"
	eventsdomain "github.com/yohang/mesh-sdr/internal/events/domain"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// recordedEvents records the published events.
type recordedEvents struct {
	mu  sync.Mutex
	got []eventsapp.Event
}

func (r *recordedEvents) Publish(_ context.Context, ev eventsapp.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.got = append(r.got, ev)
}

func (r *recordedEvents) take() []eventsapp.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := r.got
	r.got = nil

	return out
}

type fixedGlobal struct{ v string }

func (f *fixedGlobal) ListenPolicy(context.Context) (string, error) { return f.v, nil }

// TestEventAudiences: device.status reaches the viewers who may listen to
// the device, node.status those who may listen to one of its devices, and
// registry states only operators and admins (ADR 0018).
func TestEventAudiences(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	now := time.Now()

	nodes, devices := gridsqlite.NewNodeRepository(a), gridsqlite.NewDeviceRepository(a)

	for _, n := range []string{"open", "closed"} {
		if err := nodes.Create(ctx, domain.NewNode(domain.MustNodeID(n), domain.MustNodeName(n), domain.MustNodeURL("https://"+n+":8074"), now)); err != nil {
			t.Fatal(err)
		}
	}

	for node, policy := range map[string]string{"open": "anonymous", "closed": ""} {
		d, err := domain.NewReportedDevice(domain.MustNodeID(node), domain.DeviceSpec{
			ID: shared.MustDeviceID(node + "-hf"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2,
			SampleRates: []int64{1}, ListenPolicy: policy,
		}, 0, now)
		if err != nil {
			t.Fatal(err)
		}

		if err := devices.Save(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	global := &fixedGlobal{v: "registered"}
	broker := eventsapp.NewBroker()
	cache := &policyCache{policies: gridapp.NewListenPolicies(devices, global), broker: broker, logger: quiet}
	rec := &recordedEvents{}
	ge := &gridEvents{
		b: rec, nodes: nodes, devices: gridapp.NewDevices(devices, nil, quiet), policies: cache, now: time.Now, logger: quiet,
		lastBeat: map[domain.NodeID]time.Time{}, presence: make(chan struct{}, 1),
	}

	anon, user, op := eventsdomain.Viewer{}, eventsdomain.Viewer{UserID: "u"}, eventsdomain.Viewer{UserID: "o", Staff: true}

	sees := func(ev eventsapp.Event, v eventsdomain.Viewer) bool { return ev.Audience == nil || ev.Audience(v) }

	for _, node := range []string{"open", "closed"} {
		ge.nodeDevices(ctx, domain.MustNodeID(node))
		ge.statusChanged(ctx, domain.MustNodeID(node), domain.StatusOnline, "")
	}

	for _, ev := range rec.take() {
		payload := ev.Payload
		open := false

		switch p := payload.(type) {
		case deviceStatusEvent:
			open = p.NodeID == "open"
		case nodeStatusEvent:
			open = p.NodeID == "open"
		}

		if sees(ev, anon) != open || !sees(ev, user) || !sees(ev, op) {
			t.Errorf("%s %+v: anonymous %v (want %v), user %v, operator %v", ev.Type, payload, sees(ev, anon), open, sees(ev, user), sees(ev, op))
		}
	}

	// Registry states (a node added, revoked, removed) are for the staff.
	ge.node(ctx, domain.MustNodeID("open")) // pending: enrolling
	ge.forgotten(ctx, mustDevice(t, devices, "open-hf"))

	for _, ev := range rec.take() {
		if sees(ev, anon) || sees(ev, user) || !sees(ev, op) {
			t.Errorf("%s %+v reaches non-staff viewers", ev.Type, ev.Payload)
		}
	}

	// The cache asks sockets to re-authorise only when policies changed.
	s := broker.Attach(anon, topicAuthz{policies: cache}, func(eventsapp.Event) {})

	cache.refresh(ctx)

	select {
	case <-s.Rechecks():
		t.Error("recheck without a policy change")
	default:
	}

	global.v = "anonymous"
	cache.refresh(ctx)

	select {
	case <-s.Rechecks():
	default:
		t.Error("no recheck after a policy change")
	}
}

func mustDevice(t *testing.T, repo *gridsqlite.DeviceRepository, id string) *domain.Device {
	t.Helper()

	d, err := repo.Get(context.Background(), shared.MustDeviceID(id))
	if err != nil {
		t.Fatal(err)
	}

	return d
}
