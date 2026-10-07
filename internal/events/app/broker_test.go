package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/events/app"
	"github.com/yohang/mesh-sdr/internal/events/domain"
)

// authz allows every topic but the ones in deny.
type authz struct {
	mu   sync.Mutex
	deny map[string]bool
	err  error
}

func (a *authz) AuthorizeTopic(_ context.Context, t domain.Topic) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.err != nil {
		return a.err
	}

	if a.deny[t.String()] {
		return domain.ErrTopicForbidden
	}

	return nil
}

type recorder struct {
	mu  sync.Mutex
	got []app.Event
}

func (r *recorder) sink(ev app.Event) {
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
	b := app.NewBroker()
	az := &authz{deny: map[string]bool{"admin.connections": true}}
	rec := &recorder{}
	s := b.Attach(domain.Viewer{}, az, rec.sink)

	if _, bad, err := s.Subscribe(ctx, []string{"nodes", "admin.connections"}); !errors.Is(err, domain.ErrTopicForbidden) || bad != "admin.connections" {
		t.Fatalf("forbidden sub = %q, %v", bad, err)
	}

	if _, bad, err := s.Subscribe(ctx, []string{"nodes", "bogus"}); !errors.Is(err, domain.ErrInvalidTopic) || bad != "bogus" {
		t.Fatalf("invalid sub = %q, %v", bad, err)
	}

	if len(s.Topics()) != 0 {
		t.Fatalf("a refused sub subscribed %v", s.Topics())
	}

	if _, _, err := s.Subscribe(ctx, []string{"nodes", "devices"}); err != nil {
		t.Fatal(err)
	}

	b.Publish(ctx, app.Event{Topic: domain.MustTopic("nodes"), Type: "node.status"})
	b.Publish(ctx, app.Event{Topic: domain.MustTopic("presence"), Type: "presence.count"})

	if got := rec.types(); len(got) != 1 || got[0] != "node.status@nodes" {
		t.Fatalf("delivered %v", got)
	}

	s.Unsubscribe([]string{"nodes", "bogus"})

	if got := s.Topics(); len(got) != 1 || got[0].String() != "devices" {
		t.Fatalf("topics after unsub = %v", got)
	}

	s.Detach()

	if b.Subscribers() != 0 {
		t.Fatal("detached subscriber still attached")
	}
}

func TestMaxTopics(t *testing.T) {
	ctx := context.Background()
	s := app.NewBroker().Attach(domain.Viewer{}, &authz{}, func(app.Event) {})

	topics := make([]string, 0, domain.MaxTopics)
	for i := range domain.MaxTopics {
		topics = append(topics, "decodes:device=d"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}

	if _, _, err := s.Subscribe(ctx, topics); err != nil {
		t.Fatal(err)
	}

	// Subscribing again to a subscribed topic adds nothing.
	if _, _, err := s.Subscribe(ctx, topics[:1]); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Subscribe(ctx, []string{"nodes"}); !errors.Is(err, domain.ErrTooManyTopics) {
		t.Fatalf("33rd topic = %v", err)
	}
}

func TestAudience(t *testing.T) {
	ctx := context.Background()
	b := app.NewBroker()
	alice, bob := &recorder{}, &recorder{}

	for v, r := range map[domain.Viewer]*recorder{{UserID: "alice"}: alice, {UserID: "bob"}: bob} {
		s := b.Attach(v, &authz{}, r.sink)
		if _, _, err := s.Subscribe(ctx, []string{"notifications"}); err != nil {
			t.Fatal(err)
		}
	}

	b.Publish(ctx, app.Event{
		Topic: domain.MustTopic("notifications"), Type: "files.new",
		Audience: func(v domain.Viewer) bool { return v.UserID == "alice" },
	})

	if len(alice.types()) != 1 || len(bob.types()) != 0 {
		t.Fatalf("alice %v, bob %v", alice.types(), bob.types())
	}
}

func TestRecheckAndEnd(t *testing.T) {
	ctx := context.Background()
	b := app.NewBroker()
	az := &authz{deny: map[string]bool{}}
	s := b.Attach(domain.Viewer{UserID: "u1", SessionRef: "s1"}, az, func(app.Event) {})
	anon := b.Attach(domain.Viewer{}, az, func(app.Event) {})

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
	a := app.NewAdmission(app.Limits{PerSession: 2, PerAddress: 3, Total: 4, UpgradesPerMinute: 5})

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

	if err := admit("s1", "192.0.2.1"); !errors.Is(err, domain.ErrTooManyConnections) {
		t.Fatalf("third socket of a session = %v", err)
	}

	if err := admit("", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}

	if err := admit("", "192.0.2.1"); !errors.Is(err, domain.ErrUpgradeRate) && !errors.Is(err, domain.ErrTooManyConnections) {
		t.Fatalf("fourth socket of an address = %v", err)
	}

	if err := admit("", "192.0.2.2"); err != nil {
		t.Fatal(err)
	}

	if err := admit("", "192.0.2.3"); !errors.Is(err, domain.ErrHubFull) {
		t.Fatalf("socket beyond the hub cap = %v", err)
	}

	releases[0]()
	releases[0]() // idempotent

	if a.Open() != 3 {
		t.Fatalf("open = %d, want 3", a.Open())
	}

	// Upgrades per minute and address: the fifth attempt of 192.0.2.1 was
	// the last token.
	if _, wait, err := a.Admit("", "192.0.2.1", now); !errors.Is(err, domain.ErrUpgradeRate) || wait <= 0 {
		t.Fatalf("upgrade beyond the rate = %v, wait %v", err, wait)
	}

	if _, _, err := a.Admit("s9", "192.0.2.1", now.Add(time.Minute)); err != nil {
		t.Fatalf("after a minute: %v", err)
	}
}
