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
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// newPolicyEnv is an authz environment whose node has one device per
// listen policy override: "inherit" (none), "open" (anonymous) and
// "closed" (registered).
func newPolicyEnv(t *testing.T, global string, upgrades, mints app.RateLimiter) *authzEnv {
	t.Helper()

	e := newEnv(t)
	n := enrolledNode(t, e)

	inherit, open, closed := device("inherit", "rtl_sdr"), device("open", "rtl_sdr"), device("closed", "rtl_sdr")
	open.ListenPolicy, closed.ListenPolicy = app.ListenAnonymous, app.ListenRegistered

	devices := app.NewDevices(sqlite.NewDeviceRepository(e.db), e.audit, discard)
	if err := devices.Sync(context.Background(), n, ctl.Capabilities{Devices: []ctl.Device{inherit, open, closed}}, e.clock.now()); err != nil {
		t.Fatal(err)
	}

	return newAuthzEnvOn(t, e, n, global, upgrades, mints)
}

// TestMediaAccessRightsMatrix checks the rights of anonymous visitors,
// listeners and admins for every global listen policy and device override
// (ACC-012, ACC-013): who may listen where, and that only signed-in
// operators and admins get the control permissions (preset, retune).
func TestMediaAccessRightsMatrix(t *testing.T) {
	user, _ := shared.NewUUIDv7(time.Now())

	subjects := map[string]fakeSubject{
		"anonymous": {},
		"listener":  {user: user, session: user, role: app.RoleListener},
		"admin":     {user: user, session: user, role: app.RoleAdmin},
	}

	listenOnly := []string{"listen", "demod"}
	full := []string{"listen", "demod", "preset", "retune"}

	cases := []struct {
		global, subject string
		want            map[string][]string // device → perms; nil: refused
		err             error
	}{
		{app.ListenAnonymous, "anonymous", map[string][]string{"inherit": listenOnly, "open": listenOnly}, nil},
		{app.ListenAnonymous, "listener", map[string][]string{"inherit": listenOnly, "open": listenOnly, "closed": listenOnly}, nil},
		{app.ListenAnonymous, "admin", map[string][]string{"inherit": full, "open": full, "closed": full}, nil},
		{app.ListenRegistered, "anonymous", map[string][]string{"open": listenOnly}, nil},
		{app.ListenRegistered, "listener", map[string][]string{"inherit": listenOnly, "open": listenOnly, "closed": listenOnly}, nil},
		{app.ListenRegistered, "admin", map[string][]string{"inherit": full, "open": full, "closed": full}, nil},
		// A global policy that cannot be read fails closed to registered.
		{"", "anonymous", map[string][]string{"open": listenOnly}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.global+"/"+tc.subject, func(t *testing.T) {
			a := newPolicyEnv(t, tc.global, limiter{allow: true}, limiter{allow: true})

			if _, err := a.authorize(subjects[tc.subject]); err != nil {
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
		})
	}
}

// TestMediaAccessAnonymousRefusedEverywhere: with every device registered,
// an anonymous visitor is asked to sign in (401) and gets no token.
func TestMediaAccessAnonymousRefusedEverywhere(t *testing.T) {
	e := newEnv(t)
	n := enrolledNode(t, e)

	closed := device("closed", "rtl_sdr")
	closed.ListenPolicy = app.ListenRegistered

	devices := app.NewDevices(sqlite.NewDeviceRepository(e.db), e.audit, discard)
	if err := devices.Sync(context.Background(), n, ctl.Capabilities{Devices: []ctl.Device{device("inherit", "rtl_sdr"), closed}}, e.clock.now()); err != nil {
		t.Fatal(err)
	}

	for _, global := range []string{app.ListenRegistered, "unreadable"} {
		a := newAuthzEnvOn(t, e, n, global, limiter{allow: true}, limiter{allow: true})

		_, err := a.authorize(fakeSubject{})
		if !errors.Is(err, domain.ErrListenLoginNeeded) || a.issuer.last.ConnectionID != "" {
			t.Fatalf("global %q: err = %v, claims = %+v", global, err, a.issuer.last)
		}
	}
}

func newAuthzEnvOn(t *testing.T, e *env, n *domain.Node, global string, upgrades, mints app.RateLimiter) *authzEnv {
	t.Helper()

	conns := sqlite.NewConnectionRepository(e.db)
	tr := app.NewTracker()
	issuer := &issuerSpy{}

	access, err := app.NewMediaAccess(app.MediaAccessOptions{
		Nodes: e.nodes, Devices: sqlite.NewDeviceRepository(e.db), Tracker: tr,
		Presence: app.NewPresence(conns, sqlite.NewDeviceRepository(e.db), tr, app.DefaultTimings(), e.clock.now, discard),
		Issuer:   issuer, Policy: authzPolicy(global), HubURL: hubURL,
		Upgrades: upgrades, Mints: mints, Now: e.clock.now, Logger: discard,
	})
	if err != nil {
		t.Fatal(err)
	}

	a := &authzEnv{env: e, access: access, issuer: issuer, tracker: tr, conns: conns, node: n}
	a.online()

	return a
}

// TestMediaAccessRateLimits: an exhausted upgrade (per address) or mint
// (per address for anonymous visitors, per session otherwise) bucket
// refuses the connection with a retry delay and issues no token.
func TestMediaAccessRateLimits(t *testing.T) {
	cases := []struct {
		name            string
		upgrades, mints bool
	}{
		{"upgrades", false, true},
		{"mints", true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newPolicyEnv(t, app.ListenAnonymous, limiter{allow: tc.upgrades}, limiter{allow: tc.mints})

			_, err := a.authorize(fakeSubject{})

			wait, ok := app.IsRateLimited(err)
			if !ok || wait <= 0 || !errors.Is(err, domain.ErrTokenRateLimited) {
				t.Fatalf("err = %v", err)
			}

			if a.issuer.last.ConnectionID != "" {
				t.Fatalf("a token was issued: %+v", a.issuer.last)
			}
		})
	}
}
