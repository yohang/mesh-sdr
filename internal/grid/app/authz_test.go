package app_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	accesstoken "github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

type fakeSubject struct {
	user, session shared.UUID
	role          string            // global role, "" when anonymous
	grants        map[string]string // device → role
}

func (s fakeSubject) IsAnonymous() bool      { return s.role == "" }
func (s fakeSubject) UserID() shared.UUID    { return s.user }
func (s fakeSubject) SessionID() shared.UUID { return s.session }
func (s fakeSubject) Role() string           { return s.role }

func (s fakeSubject) Roles() []string {
	if s.role == "" {
		return nil
	}

	return []string{s.role}
}

func (s fakeSubject) RoleRank() int {
	return map[string]int{"": 0, "listener": 10, "operator": 20, "admin": 30}[s.role]
}

func (s fakeSubject) HasOnDevice(role, device string) bool {
	rank := map[string]int{"listener": 10, "operator": 20, "admin": 30}

	return rank[s.role] >= rank[role] || (s.grants[device] != "" && rank[s.grants[device]] >= rank[role])
}

type issuerSpy struct{ last accesstoken.Claims }

func (i *issuerSpy) Issue(_ context.Context, c accesstoken.Claims) (string, error) {
	i.last = c

	return "signed", nil
}

type limiter struct{ allow bool }

func (l limiter) Allow(string, time.Time) (bool, time.Duration) { return l.allow, 2 * time.Second }

type authzEnv struct {
	*env
	access   *app.MediaAccess
	issuer   *issuerSpy
	tracker  *app.Tracker
	conns    *sqlite.ConnectionRepository
	upgrades *limiter
	node     *domain.Node
}

const hubURL = "https://sdr.example.org"

func newAuthzEnv(t *testing.T, policy string) *authzEnv {
	t.Helper()

	e := newEnv(t)
	n := enrolledNode(t, e)
	ctx := context.Background()

	devices := app.NewDevices(sqlite.NewDeviceRepository(e.db), e.audit, discard)

	hf, vhf := device("hf", "rtl_sdr"), device("vhf", "rtl_sdr")
	vhf.ListenPolicy, vhf.OperatorCanRetune = "registered", true
	off := device("off", "rtl_sdr")
	off.Enabled = false

	if err := devices.Sync(ctx, n, ctl.Capabilities{Devices: []ctl.Device{hf, vhf, off}}, e.clock.now()); err != nil {
		t.Fatal(err)
	}

	conns := sqlite.NewConnectionRepository(e.db)
	tr := app.NewTracker()
	issuer := &issuerSpy{}
	up := &limiter{allow: true}

	access, err := app.NewMediaAccess(app.MediaAccessOptions{
		Nodes: e.nodes, Devices: sqlite.NewDeviceRepository(e.db), Tracker: tr,
		Presence: app.NewPresence(conns, sqlite.NewDeviceRepository(e.db), tr, app.DefaultTimings(), e.clock.now, discard),
		Issuer:   issuer, Policy: authzPolicy(policy), HubURL: hubURL,
		Upgrades: up, Mints: limiter{allow: true}, Now: e.clock.now, Logger: discard,
	})
	if err != nil {
		t.Fatal(err)
	}

	return &authzEnv{env: e, access: access, issuer: issuer, tracker: tr, conns: conns, upgrades: up, node: n}
}

type authzPolicy string

func (p authzPolicy) ListenPolicy(context.Context) string { return string(p) }

func (a *authzEnv) online() {
	a.tracker.Welcomed(a.node.ID(), shared.UUID{}, domain.Compatibility{Level: domain.CompatOK}, a.clock.now())
}

func (a *authzEnv) authorize(s app.Subject) (app.Grant, error) {
	return a.access.Authorize(context.Background(), app.AuthzRequest{NodeID: "attic", Subject: s, Origin: hubURL, IP: "192.0.2.1"})
}

func scopes(c accesstoken.Claims) map[string][]string {
	out := map[string][]string{}
	for _, s := range c.Scopes {
		out[s.Device] = s.Perms
	}

	return out
}

func TestMediaAccessNode(t *testing.T) {
	a := newAuthzEnv(t, "anonymous")
	anon := fakeSubject{}

	if _, err := a.access.Authorize(context.Background(), app.AuthzRequest{NodeID: "nope", Subject: anon, Origin: hubURL}); !errors.Is(err, domain.ErrNodeNotFound) {
		t.Fatalf("unknown node: %v", err)
	}

	if _, err := a.authorize(anon); !errors.Is(err, domain.ErrNodeOffline) {
		t.Fatalf("no control channel: %v", err)
	}

	a.tracker.Welcomed(a.node.ID(), shared.UUID{}, domain.Compatibility{Level: domain.CompatIncompatible}, a.clock.now())

	if _, err := a.authorize(anon); !errors.Is(err, domain.ErrNodeIncompatible) {
		t.Fatalf("incompatible: %v", err)
	}

	a.online()

	if _, err := a.access.Authorize(context.Background(), app.AuthzRequest{NodeID: "attic", Subject: anon, Origin: "https://evil.example.org"}); !errors.Is(err, domain.ErrOriginDenied) {
		t.Fatalf("foreign origin: %v", err)
	}

	a.upgrades.allow = false

	_, err := a.authorize(anon)
	if wait, ok := app.IsRateLimited(err); !ok || wait != 2*time.Second {
		t.Fatalf("rate limited: %v", err)
	}

	a.upgrades.allow = true

	// A disabled node is unknown to the gateway.
	n, _ := a.nodes.Get(context.Background(), a.node.ID())
	disabled := true
	v := n.Version()

	if err := n.Update(domain.NodePatch{Disabled: &disabled}, v, a.clock.now()); err != nil {
		t.Fatal(err)
	}

	if err := a.nodes.Save(context.Background(), n, v); err != nil {
		t.Fatal(err)
	}

	if _, err := a.authorize(anon); !errors.Is(err, domain.ErrNodeNotFound) {
		t.Fatalf("disabled node: %v", err)
	}
}

func TestMediaAccessScopes(t *testing.T) {
	a := newAuthzEnv(t, "anonymous")
	a.online()

	user, _ := shared.NewUUIDv7(time.Now())
	session, _ := shared.NewUUIDv7(time.Now())

	cases := []struct {
		name string
		s    fakeSubject
		want map[string][]string
	}{
		{"anonymous", fakeSubject{}, map[string][]string{"hf": {"listen", "demod"}}},
		{"listener", fakeSubject{user: user, session: session, role: "listener"},
			map[string][]string{"hf": {"listen", "demod"}, "vhf": {"listen", "demod"}}},
		{"operator of vhf", fakeSubject{user: user, session: session, role: "listener", grants: map[string]string{"vhf": "operator"}},
			map[string][]string{"hf": {"listen", "demod"}, "vhf": {"listen", "demod", "preset", "retune"}}},
		{"global operator", fakeSubject{user: user, session: session, role: "operator"},
			map[string][]string{"hf": {"listen", "demod", "preset"}, "vhf": {"listen", "demod", "preset", "retune"}}},
		{"admin", fakeSubject{user: user, session: session, role: "admin"},
			map[string][]string{"hf": {"listen", "demod", "preset", "retune"}, "vhf": {"listen", "demod", "preset", "retune"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := a.authorize(tc.s)
			if err != nil {
				t.Fatal(err)
			}

			got := scopes(a.issuer.last)
			if len(got) != len(tc.want) {
				t.Fatalf("scopes = %v, want %v", got, tc.want)
			}

			for d, perms := range tc.want {
				if !slices.Equal(got[d], perms) {
					t.Errorf("%s: perms = %v, want %v", d, got[d], perms)
				}
			}

			c := a.issuer.last
			if c.Issuer != hubURL || c.Audience != accesstoken.Audience("attic") || c.ConnectionID != g.CID || g.Upstream != "x:1" ||
				!c.ExpiresAt.Equal(a.clock.now().Add(app.AccessTokenTTL)) {
				t.Fatalf("claims = %+v, grant = %+v", c, g)
			}

			if tc.s.IsAnonymous() {
				if c.Subject != accesstoken.AnonymousSubject || c.SessionID != "" || c.Limits.MaxDemods != 1 {
					t.Fatalf("anonymous claims = %+v", c)
				}
			} else if c.Subject != user.String() || c.SessionID != accesstoken.SessionRef(session.String()) {
				t.Fatalf("user claims = %+v", c)
			}

			// The connection row exists before the node reports it.
			cid, _ := shared.ParseUUID(g.CID)

			row, err := a.conns.Get(context.Background(), cid)
			if err != nil || row.Info().NodeID != "attic" || row.Info().IP != "192.0.2.1" {
				t.Fatalf("connection row = %+v, %v", row, err)
			}
		})
	}
}

func TestMediaAccessRegisteredPolicy(t *testing.T) {
	a := newAuthzEnv(t, "registered")
	a.online()

	if _, err := a.authorize(fakeSubject{}); !errors.Is(err, domain.ErrListenLoginNeeded) {
		t.Fatalf("anonymous on a registered station: %v", err)
	}

	user, _ := shared.NewUUIDv7(time.Now())

	if _, err := a.authorize(fakeSubject{user: user, session: user, role: "listener"}); err != nil {
		t.Fatalf("listener: %v", err)
	}
}
