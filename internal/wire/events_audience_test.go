package wire

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/presets"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// recordedEvents records the published events.
type recordedEvents struct {
	mu  sync.Mutex
	got []events.Event
}

func (r *recordedEvents) Publish(_ context.Context, ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.got = append(r.got, ev)
}

func (r *recordedEvents) take() []events.Event {
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
	broker := events.NewBroker()
	cache := &policyCache{policies: gridapp.NewListenPolicies(devices, global), broker: broker, logger: quiet}
	rec := &recordedEvents{}
	ge := &gridEvents{
		b: rec, nodes: nodes, devices: gridapp.NewDevices(devices, nil, quiet), policies: cache, now: time.Now, logger: quiet,
		lastBeat: map[domain.NodeID]time.Time{}, presence: make(chan struct{}, 1),
	}

	anon, user, op := events.Viewer{}, events.Viewer{UserID: "u"}, events.Viewer{UserID: "o", Staff: true}

	sees := func(ev events.Event, v events.Viewer) bool { return ev.Audience == nil || ev.Audience(v) }

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
	s := broker.Attach(anon, topicAuthz{policies: cache}, func(events.Event) {})

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

// fakeListeners is a fixed listener count by device and node.
type fakeListeners struct {
	by    map[string]int
	nodes map[domain.NodeID]int
}

func (f *fakeListeners) Listeners(context.Context) (int, error) {
	n := 0
	for _, c := range f.by {
		n += c
	}

	return n, nil
}

func (f *fakeListeners) ListenersByDevice(context.Context) (map[string]int, error) {
	return maps.Clone(f.by), nil
}

func (f *fakeListeners) NodeListeners(_ context.Context, id domain.NodeID) (int, error) {
	return f.nodes[id], nil
}

// jsonKeys returns the keys of v marshalled as a JSON object.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}

	return slices.Sorted(maps.Keys(m))
}

// TestEventPublicPayloads: what listeners learn from node.status and
// device.status is the public subset (§6.6): node availability, listeners
// and the CPU, temperature and battery of an up node (RX-035, GRID-021);
// device state, listeners, active preset and centre (UI-021). Listener
// count changes publish device.status for the changed devices only, to
// their audience.
func TestEventPublicPayloads(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	now := time.Now()

	nodes, devices := gridsqlite.NewNodeRepository(a), gridsqlite.NewDeviceRepository(a)

	if err := nodes.Create(ctx, domain.NewNode(domain.MustNodeID("open"), domain.MustNodeName("open"), domain.MustNodeURL("https://open:8074"), now)); err != nil {
		t.Fatal(err)
	}

	spec, err := presets.NewSpec(presets.Draft{Name: "2 m", CenterFreq: 145_000_000, SampRate: 2_048_000})
	if err != nil {
		t.Fatal(err)
	}

	preset, _ := presets.NewPreset(shared.MustParseUUID("0192c3a4-5b6c-7d8e-9f01-23456789abcd"), spec, 0, now)
	if err := presets.NewPresets(a).Create(ctx, preset); err != nil {
		t.Fatal(err)
	}

	for i, id := range []string{"open-hf", "open-vhf"} {
		policy := map[string]string{"open-hf": "anonymous", "open-vhf": "registered"}[id]

		d, err := domain.NewReportedDevice(domain.MustNodeID("open"), domain.DeviceSpec{
			ID: shared.MustDeviceID(id), Name: id, Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2,
			SampleRates: []int64{1}, ListenPolicy: policy,
		}, i, now)
		if err != nil {
			t.Fatal(err)
		}

		center := int64(145_000_000)
		d.ApplyState(domain.StateRunning, "", &center, preset.ID(), now)

		if err := devices.Save(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	history := gridapp.NewHistory()
	temp := 48.5
	history.Add(domain.MustNodeID("open"), gridapp.LoadSample{At: now, CPU: 0.3, Load1: 2, TempC: &temp, MemTotalBytes: 8, MemAvailableBytes: 4})

	broker := events.NewBroker()
	cache := &policyCache{policies: gridapp.NewListenPolicies(devices, &fixedGlobal{v: "registered"}), broker: broker, logger: quiet}
	rec := &recordedEvents{}
	counts := &fakeListeners{by: map[string]int{"open-hf": 2}, nodes: map[domain.NodeID]int{domain.MustNodeID("open"): 2}}
	ge := &gridEvents{
		b: rec, nodes: nodes, devices: gridapp.NewDevices(devices, nil, quiet), policies: cache, now: time.Now, logger: quiet,
		lastBeat: map[domain.NodeID]time.Time{}, presence: make(chan struct{}, 1), listeners: counts, history: history,
	}

	anon := events.Viewer{}
	sees := func(ev events.Event, v events.Viewer) bool { return ev.Audience == nil || ev.Audience(v) }

	ge.statusChanged(ctx, domain.MustNodeID("open"), domain.StatusOnline, "")

	got := rec.take()
	if len(got) != 3 {
		t.Fatalf("events = %+v", got)
	}

	node, ok := got[0].Payload.(nodeStatusEvent)
	if !ok || !sees(got[0], anon) || node.Status != "online" || node.Listeners == nil || *node.Listeners != 2 ||
		node.CPU == nil || *node.CPU != 0.3 || node.TempC == nil || *node.TempC != temp || node.Battery != nil {
		t.Errorf("node.status = %+v", got[0].Payload)
	}

	if keys := jsonKeys(t, node); !slices.Equal(keys, []string{"cpu", "listeners", "node_id", "status", "temp_c"}) {
		t.Errorf("node.status keys = %v", keys)
	}

	for _, ev := range got[1:] {
		d := ev.Payload.(deviceStatusEvent)
		if d.State != "running" || d.CenterHz != 145_000_000 || d.ActivePresetID != preset.ID().String() {
			t.Errorf("device.status = %+v", d)
		}

		if want := counts.by[d.DeviceID]; d.Listeners != want {
			t.Errorf("%s listeners = %d, want %d", d.DeviceID, d.Listeners, want)
		}

		// The registered-only device stays hidden from anonymous viewers.
		if sees(ev, anon) != (d.DeviceID == "open-hf") {
			t.Errorf("%s reaches anonymous viewers: %v", d.DeviceID, sees(ev, anon))
		}
	}

	// An offline node carries no telemetry.
	ge.statusChanged(ctx, domain.MustNodeID("open"), domain.StatusOffline, "")

	if node := rec.take()[0].Payload.(nodeStatusEvent); node.Status != "offline" || node.Listeners != nil || node.CPU != nil || node.TempC != nil {
		t.Errorf("offline node.status = %+v", node)
	}

	// Listener changes: only the changed devices.
	last := ge.deviceListeners(ctx, map[string]int{})
	if got := rec.take(); len(got) != 1 || got[0].Payload.(deviceStatusEvent).DeviceID != "open-hf" {
		t.Errorf("first count events = %+v", got)
	}

	counts.by = map[string]int{"open-vhf": 1}
	ge.deviceListeners(ctx, last)

	got = rec.take()
	if len(got) != 2 {
		t.Fatalf("change events = %+v", got)
	}

	for _, ev := range got {
		d := ev.Payload.(deviceStatusEvent)
		if want := counts.by[d.DeviceID]; d.Listeners != want {
			t.Errorf("%s listeners = %d, want %d", d.DeviceID, d.Listeners, want)
		}

		if sees(ev, anon) != (d.DeviceID == "open-hf") {
			t.Errorf("%s reaches anonymous viewers: %v", d.DeviceID, sees(ev, anon))
		}
	}
}
