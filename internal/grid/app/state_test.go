package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

type fakeDesired struct{ st ctl.StateApply }

func (f *fakeDesired) Desired(context.Context, domain.NodeID) (ctl.StateApply, error) {
	return f.st, nil
}

type fakeSender struct {
	sent []ctl.StateApply
	down bool
}

func (f *fakeSender) SendState(_ context.Context, _ domain.NodeID, st ctl.StateApply) error {
	if f.down {
		return domain.ErrNodeUnavailable
	}

	f.sent = append(f.sent, st)

	return nil
}

func state(listen string) ctl.StateApply {
	return ctl.StateApply{
		Presets: map[string]ctl.Preset{"p": {Name: "P", CenterFreq: 14_074_000, SampRate: 2_048_000}},
		Devices: map[string]ctl.DesiredDevice{"hf": {Presets: []string{"p"}, ActivePresetID: "p"}},
		Policy:  ctl.StatePolicy{ListenPolicy: listen},
	}
}

func TestRevision(t *testing.T) {
	a, err := app.Revision(state("anonymous"))
	if err != nil || a <= 0 {
		t.Fatalf("revision = %d, %v", a, err)
	}

	withRev := state("anonymous")
	withRev.Revision = 42

	if b, _ := app.Revision(withRev); b != a {
		t.Errorf("the revision field changes the revision: %d %d", a, b)
	}

	if c, _ := app.Revision(state("registered")); c == a {
		t.Error("different states share a revision")
	}
}

func TestStates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	attic := enrolledNode(t, e)
	c, tracker := newControl(e, "1.0.0")
	desired, sender := &fakeDesired{st: state("anonymous")}, &fakeSender{}
	s := app.NewStates(desired, nil, tracker, discard)
	s.SetSender(sender)

	// Not connected: nothing is sent.
	if err := s.Publish(ctx, attic.ID()); err != nil || len(sender.sent) != 0 {
		t.Fatalf("publish offline = %v, %v", sender.sent, err)
	}

	welcome(t, c, attic.ID(), "1.0.0")

	rev, _ := app.Revision(desired.st)

	// The node already applied this revision: nothing to send.
	if err := s.Welcomed(ctx, attic.ID(), rev); err != nil || len(sender.sent) != 0 {
		t.Errorf("welcomed with the current revision = %v, %v", sender.sent, err)
	}

	desired.st = state("registered")

	if err := s.Publish(ctx, attic.ID()); err != nil || len(sender.sent) != 1 || sender.sent[0].Revision == rev {
		t.Fatalf("publish a change = %v, %v", sender.sent, err)
	}

	// Unchanged: not sent again.
	if n := s.PublishAll(ctx); n != 0 || len(sender.sent) != 1 {
		t.Errorf("publish unchanged = %d, %v", n, sender.sent)
	}

	// The node refuses a device: the node is degraded.
	s.Applied(ctx, attic.ID(), ctl.StateApplied{Revision: sender.sent[0].Revision, Errors: []ctl.StateError{{DeviceID: "hf", Code: "preset_incompatible"}}})

	link, _ := tracker.State(attic.ID())
	st := app.NewStatus(e.nodes, e.db, tracker, app.NewHistory(), app.DefaultTimings(), e.clock.now, discard)

	if got, hint := st.Evaluate(attic, link, true, e.clock.now()); got != domain.StatusDegraded || hint != app.HintStateApplyFailed {
		t.Errorf("status = %s %s", got, hint)
	}

	// A new welcome sends the state again (the node did not record a
	// refused revision); the node stays degraded until it accepts it.
	welcome(t, c, attic.ID(), "1.0.0")

	if err := s.Welcomed(ctx, attic.ID(), 7); err != nil || len(sender.sent) != 2 {
		t.Errorf("welcomed with an old revision = %v, %v", sender.sent, err)
	}

	if link, _ := tracker.State(attic.ID()); len(link.StateErrors) != 1 {
		t.Errorf("refusals lost on reconnect: %+v", link)
	}

	s.Applied(ctx, attic.ID(), ctl.StateApplied{Revision: sender.sent[1].Revision})

	if link, _ := tracker.State(attic.ID()); len(link.StateErrors) != 0 {
		t.Errorf("refusals kept after an accepted state: %+v", link)
	}

	// Link changes push only after MarkChanged.
	desired.st = state("anonymous")

	if err := s.PublishChanged(ctx, attic.ID()); err != nil || len(sender.sent) != 2 {
		t.Errorf("unmarked link change pushed: %v, %v", sender.sent, err)
	}

	s.MarkChanged(attic.ID())

	if err := s.PublishChanged(ctx, attic.ID()); err != nil || len(sender.sent) != 3 {
		t.Errorf("marked link change = %v, %v", sender.sent, err)
	}

	// A state too large for a control message is refused.
	big := state("anonymous")
	big.Presets["p"] = ctl.Preset{Name: strings.Repeat("x", app.MaxStateBytes)}
	desired.st = big

	if err := s.Publish(ctx, attic.ID()); !errors.Is(err, app.ErrStateTooLarge) {
		t.Errorf("too large: %v", err)
	}

	// Reported once per revision.
	if err := s.Publish(ctx, attic.ID()); err != nil {
		t.Errorf("too large again: %v", err)
	}

	sender.down = true
	desired.st = state("anonymous")

	if err := s.Publish(ctx, attic.ID()); err != nil {
		t.Errorf("channel closed meanwhile: %v", err)
	}
}

type listenerSpy struct {
	reported, stale, removed []string
}

func (l *listenerSpy) DeviceReported(_ context.Context, d *domain.Device) error {
	l.reported = append(l.reported, d.ID().String())

	return nil
}

func (l *listenerSpy) DevicesStale(_ context.Context, ids []domain.DeviceID) error {
	for _, id := range ids {
		l.stale = append(l.stale, id.String())
	}

	return nil
}

func (l *listenerSpy) DevicesRemoved(_ context.Context, ids []domain.DeviceID) error {
	for _, id := range ids {
		l.removed = append(l.removed, id.String())
	}

	return nil
}

func TestDeviceListener(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := app.NewDevices(sqlite.NewDeviceRepository(e.db), e.audit, discard)
	spy := &listenerSpy{}
	s.SetListener(spy, e.db)

	attic := enrolledNode(t, e)

	report := func(devices ...ctl.Device) {
		t.Helper()

		if err := e.db.WithinTx(ctx, func(ctx context.Context) error {
			return s.Sync(ctx, attic, ctl.Capabilities{Devices: devices}, e.clock.now())
		}); err != nil {
			t.Fatal(err)
		}
	}

	report(device("hf", "rtl_sdr"), device("vhf", "rtl_sdr"))
	report(device("hf", "rtl_sdr"))

	if !slices.Equal(spy.reported, []string{"hf", "vhf", "hf"}) || !slices.Equal(spy.stale, []string{"vhf"}) {
		t.Errorf("reported %v, stale %v", spy.reported, spy.stale)
	}

	if err := s.Forget(ctx, app.ActorUser, "vhf"); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(spy.removed, []string{"vhf"}) {
		t.Errorf("removed %v", spy.removed)
	}

	// Deleting a node runs its hooks in the deletion transaction.
	var deleted []string

	e.svc.OnDelete(func(_ context.Context, id domain.NodeID) error {
		deleted = append(deleted, id.String())

		return nil
	})

	garden := domain.NewNode(domain.MustNodeID("garden"), domain.MustNodeName("g"), domain.MustNodeURL("https://g:1"), e.clock.now())
	if err := e.nodes.Create(ctx, garden); err != nil {
		t.Fatal(err)
	}

	if err := e.svc.Delete(ctx, app.ActorUser, "garden"); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(deleted, []string{"garden"}) {
		t.Errorf("delete hooks ran for %v", deleted)
	}
}
