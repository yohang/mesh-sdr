package events_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/events"
)

// authz allows every topic but the ones in deny.
type authz struct {
	mu   sync.Mutex
	deny map[string]bool
	err  error
}

func (a *authz) AuthorizeTopic(_ context.Context, t events.Topic) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.err != nil {
		return a.err
	}

	if a.deny[t.String()] {
		return events.ErrTopicForbidden
	}

	return nil
}

type recorder struct {
	mu  sync.Mutex
	got []events.Event
}

func (r *recorder) sink(ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.got = append(r.got, ev)
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, len(r.got))
	for i, ev := range r.got {
		out[i] = ev.Type + "@" + ev.Topic.String()
	}

	return out
}

func TestSubscribeAllOrNothing(t *testing.T) {
	ctx := context.Background()
	b := events.NewBroker()
	az := &authz{deny: map[string]bool{"admin.connections": true}}
	rec := &recorder{}
	s := b.Attach(events.Viewer{}, az, rec.sink)

	if _, bad, err := s.Subscribe(ctx, []string{"nodes", "admin.connections"}); !errors.Is(err, events.ErrTopicForbidden) || bad != "admin.connections" {
		t.Fatalf("forbidden sub = %q, %v", bad, err)
	}

	if _, bad, err := s.Subscribe(ctx, []string{"nodes", "bogus"}); !errors.Is(err, events.ErrInvalidTopic) || bad != "bogus" {
		t.Fatalf("invalid sub = %q, %v", bad, err)
	}

	if len(s.Topics()) != 0 {
		t.Fatalf("a refused sub subscribed %v", s.Topics())
	}

	if _, _, err := s.Subscribe(ctx, []string{"nodes", "devices"}); err != nil {
		t.Fatal(err)
	}

	b.Publish(ctx, events.Event{Topic: events.MustTopic("nodes"), Type: "node.status"})
	b.Publish(ctx, events.Event{Topic: events.MustTopic("presence"), Type: "presence.count"})

	if got := rec.types(); len(got) != 1 || got[0] != "node.status@nodes" {
		t.Fatalf("delivered %v", got)
	}

	s.Unsubscribe([]string{"nodes", "bogus"})

	if got := s.Topics(); len(got) != 1 || got[0].String() != "devices" {
		t.Fatalf("topics after unsub = %v", got)
	}

	s.Detach()
	b.Publish(ctx, events.Event{Topic: events.MustTopic("devices"), Type: "device.status"})

	if got := rec.types(); len(got) != 1 {
		t.Fatalf("detached subscriber received %v", got)
	}
}

func TestMaxTopics(t *testing.T) {
	ctx := context.Background()
	s := events.NewBroker().Attach(events.Viewer{}, &authz{}, func(events.Event) {})

	topics := make([]string, 0, events.MaxTopics)
	for i := range events.MaxTopics {
		topics = append(topics, "decodes:device=d"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}

	if _, _, err := s.Subscribe(ctx, topics); err != nil {
		t.Fatal(err)
	}

	// Subscribing again to a subscribed topic adds nothing.
	if _, _, err := s.Subscribe(ctx, topics[:1]); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Subscribe(ctx, []string{"nodes"}); !errors.Is(err, events.ErrTooManyTopics) {
		t.Fatalf("33rd topic = %v", err)
	}
}

func TestAudience(t *testing.T) {
	ctx := context.Background()
	b := events.NewBroker()
	alice, bob := &recorder{}, &recorder{}

	for v, r := range map[events.Viewer]*recorder{{UserID: "alice"}: alice, {UserID: "bob"}: bob} {
		s := b.Attach(v, &authz{}, r.sink)
		if _, _, err := s.Subscribe(ctx, []string{"notifications"}); err != nil {
			t.Fatal(err)
		}
	}

	b.Publish(ctx, events.Event{
		Topic: events.MustTopic("notifications"), Type: "files.new",
		Audience: func(v events.Viewer) bool { return v.UserID == "alice" },
	})

	if len(alice.types()) != 1 || len(bob.types()) != 0 {
		t.Fatalf("alice %v, bob %v", alice.types(), bob.types())
	}
}

func TestRecheckAndEnd(t *testing.T) {
	ctx := context.Background()
	b := events.NewBroker()
	az := &authz{deny: map[string]bool{}}
	s := b.Attach(events.Viewer{UserID: "u1", SessionRef: "s1"}, az, func(events.Event) {})
	anon := b.Attach(events.Viewer{}, az, func(events.Event) {})

	if _, _, err := s.Subscribe(ctx, []string{"nodes", "decodes:device=hf"}); err != nil {
		t.Fatal(err)
	}

	b.RecheckAll()

	select {
	case <-s.Rechecks():
	case <-time.After(time.Second):
		t.Fatal("no recheck signal")
	}

	az.mu.Lock()
	az.deny["decodes:device=hf"] = true
	az.mu.Unlock()

	dropped, err := s.Recheck(ctx)
	if err != nil || len(dropped) != 1 || dropped[0].String() != "decodes:device=hf" {
		t.Fatalf("recheck = %v, %v", dropped, err)
	}

	// A failing check drops nothing.
	az.mu.Lock()
	az.err = errors.New("db down")
	az.mu.Unlock()

	if dropped, err := s.Recheck(ctx); err == nil || len(dropped) != 0 || len(s.Topics()) != 1 {
		t.Fatalf("recheck on error = %v, %v, topics %v", dropped, err, s.Topics())
	}

	b.EndSessions([]string{"other"}, []string{"u2"})

	select {
	case <-s.Ended():
		t.Fatal("another session ended this one")
	default:
	}

	b.EndSessions([]string{"s1"}, nil)

	select {
	case <-s.Ended():
	default:
		t.Fatal("the session was not ended")
	}

	b.EndSessions(nil, []string{"u1"}) // twice: no panic

	select {
	case <-anon.Ended():
		t.Fatal("an anonymous subscriber was ended")
	default:
	}
}

func TestAdmission(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := events.NewAdmission(events.Limits{PerSession: 2, PerAddress: 3, Total: 4, UpgradesPerMinute: 5})

	var releases []func()

	admit := func(session, addr string) error {
		t.Helper()

		r, _, err := a.Admit(session, addr, now)
		if err == nil {
			releases = append(releases, r)
		}

		return err
	}

	for range 2 {
		if err := admit("s1", "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}

	if err := admit("s1", "192.0.2.1"); !errors.Is(err, events.ErrTooManyConnections) {
		t.Fatalf("third socket of a session = %v", err)
	}

	if err := admit("", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}

	if err := admit("", "192.0.2.1"); !errors.Is(err, events.ErrUpgradeRate) && !errors.Is(err, events.ErrTooManyConnections) {
		t.Fatalf("fourth socket of an address = %v", err)
	}

	if err := admit("", "192.0.2.2"); err != nil {
		t.Fatal(err)
	}

	if err := admit("", "192.0.2.3"); !errors.Is(err, events.ErrHubFull) {
		t.Fatalf("socket beyond the hub cap = %v", err)
	}

	releases[0]()
	releases[0]() // idempotent

	if a.Open() != 3 {
		t.Fatalf("open = %d, want 3", a.Open())
	}

	// Upgrades per minute and address: the fifth attempt of 192.0.2.1 was
	// the last token.
	if _, wait, err := a.Admit("", "192.0.2.1", now); !errors.Is(err, events.ErrUpgradeRate) || wait <= 0 {
		t.Fatalf("upgrade beyond the rate = %v, wait %v", err, wait)
	}

	if _, _, err := a.Admit("s9", "192.0.2.1", now.Add(time.Minute)); err != nil {
		t.Fatalf("after a minute: %v", err)
	}
}

// countingAuthz counts the authorisations.
type countingAuthz struct{ n int }

func (a *countingAuthz) AuthorizeTopic(context.Context, events.Topic) error {
	a.n++

	return nil
}

// TestSubscribeDedupAndLimitFirst: duplicates are authorised once, and an
// oversized request is refused before any authorisation.
func TestSubscribeDedupAndLimitFirst(t *testing.T) {
	ctx := context.Background()
	az := &countingAuthz{}
	s := events.NewBroker().Attach(events.Viewer{}, az, func(events.Event) {})

	topics, _, err := s.Subscribe(ctx, []string{"nodes", "nodes", "devices", "nodes"})
	if err != nil || len(topics) != 2 || az.n != 2 {
		t.Fatalf("dedup = %v, %v, %d authorisations", topics, err, az.n)
	}

	big := make([]string, 0, 1000)
	for i := range 1000 {
		big = append(big, "decodes:device=d"+strconv.Itoa(i))
	}

	az.n = 0

	if _, _, err := s.Subscribe(ctx, big); !errors.Is(err, events.ErrTooManyTopics) || az.n != 0 {
		t.Fatalf("oversized sub = %v after %d authorisations", err, az.n)
	}

	// Duplicates of 30 topics fit with the 2 already held.
	many := make([]string, 0, 60)
	for i := range 30 {
		id := "decodes:device=x" + strconv.Itoa(i)
		many = append(many, id, id)
	}

	if _, _, err := s.Subscribe(ctx, many); err != nil {
		t.Fatalf("30 new topics with duplicates: %v", err)
	}

	if _, _, err := s.Subscribe(ctx, []string{"presence"}); !errors.Is(err, events.ErrTooManyTopics) {
		t.Fatalf("33rd topic = %v", err)
	}
}

func TestAddressKey(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.1":            "192.0.2.1",
		"::ffff:192.0.2.1":     "192.0.2.1",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1": "2001:db8:1:2::/64",
		"not an address":       "not an address",
	} {
		if got := events.AddressKey(in); got != want {
			t.Errorf("AddressKey(%q) = %q, want %q", in, got, want)
		}
	}

	// Two addresses of one /64 share the per-address cap.
	a := events.NewAdmission(events.Limits{PerSession: 10, PerAddress: 1, Total: 10, UpgradesPerMinute: 100})
	now := time.Unix(1_800_000_000, 0)

	if _, _, err := a.Admit("", "2001:db8:1:2::1", now); err != nil {
		t.Fatal(err)
	}

	if _, _, err := a.Admit("", "2001:db8:1:2::2", now); !errors.Is(err, events.ErrTooManyConnections) {
		t.Errorf("second address of the /64 = %v", err)
	}
}
