package wire

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// roleIdentity is an events identity with a fixed role: none (anonymous
// or listener), operator or admin.
type roleIdentity struct{ operator, admin bool }

func (r roleIdentity) Authorize(_ context.Context, role identitydomain.Role) error {
	switch {
	case role == identitydomain.RoleAdmin && r.admin, role == identitydomain.RoleOperator && (r.operator || r.admin):
		return nil
	default:
		return identitydomain.ErrForbidden
	}
}

func (roleIdentity) Principal(context.Context) identitydomain.Principal {
	return identitydomain.Principal{}
}

func (roleIdentity) SessionRef(context.Context) string { return "" }

func (roleIdentity) CheckSession(context.Context, *http.Request) (time.Time, error) {
	return time.Time{}, nil
}

// allowAll authorises every topic: a subscriber that got past the topic
// check must still be filtered by the audience of the event.
type allowAll struct{}

func (allowAll) AuthorizeTopic(context.Context, events.Topic) error { return nil }

// inbox collects the events of a subscriber.
type inbox struct {
	mu  sync.Mutex
	got []events.Event
}

func (i *inbox) sink(ev events.Event) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.got = append(i.got, ev)
}

func (i *inbox) take() []events.Event {
	i.mu.Lock()
	defer i.mu.Unlock()

	out := i.got
	i.got = nil

	return out
}

// TestDeviceLogRelay: the device log records a node pushes reach the
// admins subscribed to the log of the device, never the other viewers,
// operators included (SRC-005), and only for the node's own devices.
func TestDeviceLogRelay(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	now := time.Now()

	nodes, devices := gridsqlite.NewNodeRepository(a), gridsqlite.NewDeviceRepository(a)

	for _, n := range []string{"attic", "garden"} {
		if err := nodes.Create(ctx, domain.NewNode(domain.MustNodeID(n), domain.MustNodeName(n), domain.MustNodeURL("https://"+n+":8074"), now)); err != nil {
			t.Fatal(err)
		}
	}

	d, err := domain.NewReportedDevice(domain.MustNodeID("attic"), domain.DeviceSpec{
		ID: shared.MustDeviceID("hf"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2, SampleRates: []int64{1},
		ListenPolicy: "anonymous",
	}, 0, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := devices.Save(ctx, d); err != nil {
		t.Fatal(err)
	}

	broker := events.NewBroker()
	logs := gridapp.NewDeviceLogs(devices, quiet)
	ge := &gridEvents{b: broker, logger: quiet}
	logs.OnRecords(ge.deviceLog)

	const topic = "admin.device_log:device=hf"

	viewers := []struct {
		name     string
		viewer   events.Viewer
		identity roleIdentity
		admin    bool
	}{
		{name: "anonymous", viewer: events.Viewer{}},
		{name: "listener", viewer: events.Viewer{UserID: "l"}},
		{name: "operator", viewer: events.Viewer{UserID: "o", Staff: true}, identity: roleIdentity{operator: true}},
		{name: "admin", viewer: events.Viewer{UserID: "a", Staff: true, Admin: true}, identity: roleIdentity{admin: true}, admin: true},
	}

	type subscriber struct {
		name  string
		admin bool
		box   *inbox
	}

	var subs []subscriber

	for _, v := range viewers {
		// The topic check: only admins may subscribe.
		box := &inbox{}
		s := broker.Attach(v.viewer, topicAuthz{id: v.identity}, box.sink)

		_, _, err := s.Subscribe(ctx, []string{topic})
		if v.admin != (err == nil) || (err != nil && !errors.Is(err, events.ErrTopicForbidden)) {
			t.Errorf("%s subscribe = %v", v.name, err)
		}

		if err == nil {
			subs = append(subs, subscriber{name: v.name, admin: v.admin, box: box})
		}

		// The audience: a non-admin subscriber past the check gets nothing.
		bypass := &inbox{}
		if _, _, err := broker.Attach(v.viewer, allowAll{}, bypass.sink).Subscribe(ctx, []string{topic}); err != nil {
			t.Fatal(err)
		}

		subs = append(subs, subscriber{name: v.name + " (unchecked)", admin: v.admin, box: bypass})
	}

	rec := func(text string) ctl.LogRecord {
		return ctl.LogRecord{Time: now.UnixMilli(), Source: ctl.LogSourceConnector, Class: "info", Text: text}
	}

	tests := []struct {
		name     string
		node     string
		msg      ctl.DeviceLog
		relayed  int
		reset    bool
		recorded []string
	}{
		{name: "backlog", node: "attic", msg: ctl.DeviceLog{DeviceID: "hf", Reset: true, Records: []ctl.LogRecord{rec("one"), rec("two")}},
			relayed: 2, reset: true, recorded: []string{"one", "two"}},
		{name: "live", node: "attic", msg: ctl.DeviceLog{DeviceID: "hf", Records: []ctl.LogRecord{rec("three\x1b[0m")}},
			relayed: 1, recorded: []string{"one", "two", "three[0m"}},
		{name: "another node's device", node: "garden", msg: ctl.DeviceLog{DeviceID: "hf", Records: []ctl.LogRecord{rec("forged")}},
			relayed: -1, recorded: []string{"one", "two", "three[0m"}},
		{name: "unknown device", node: "attic", msg: ctl.DeviceLog{DeviceID: "nope", Records: []ctl.LogRecord{rec("x")}},
			relayed: -1, recorded: []string{"one", "two", "three[0m"}},
		{name: "new backlog replaces", node: "attic", msg: ctl.DeviceLog{DeviceID: "hf", Reset: true, Records: []ctl.LogRecord{rec("four")}},
			relayed: 1, reset: true, recorded: []string{"four"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs.Receive(ctx, domain.MustNodeID(tt.node), tt.msg)

			for _, s := range subs {
				got := s.box.take()

				if !s.admin || tt.relayed < 0 {
					if len(got) != 0 {
						t.Errorf("%s received %+v", s.name, got)
					}

					continue
				}

				if len(got) != 1 {
					t.Fatalf("%s received %d events", s.name, len(got))
				}

				p, ok := got[0].Payload.(deviceLogEvent)
				if !ok || got[0].Type != "device.log" || p.DeviceID != "hf" || p.Reset != tt.reset || len(p.Records) != tt.relayed {
					t.Errorf("%s received %s %+v", s.name, got[0].Type, got[0].Payload)
				}
			}

			var texts []string
			for _, r := range logs.Records(shared.MustDeviceID("hf")) {
				texts = append(texts, r.Text)
			}

			if len(texts) != len(tt.recorded) {
				t.Fatalf("records = %q, want %q", texts, tt.recorded)
			}

			for i := range texts {
				if texts[i] != tt.recorded[i] {
					t.Errorf("records = %q, want %q", texts, tt.recorded)
				}
			}
		})
	}

	// The hub keeps the last DeviceLogSize records.
	many := make([]ctl.LogRecord, 0, gridapp.DeviceLogSize+20)
	for range gridapp.DeviceLogSize + 20 {
		many = append(many, rec("x"))
	}

	logs.Receive(ctx, domain.MustNodeID("attic"), ctl.DeviceLog{DeviceID: "hf", Records: many})

	if n := len(logs.Records(shared.MustDeviceID("hf"))); n != gridapp.DeviceLogSize {
		t.Errorf("records held = %d", n)
	}
}
