package wire

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/bookmarks"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/http/api"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// listenGlobal is a global listen policy setting, or a read error.
type listenGlobal struct {
	v   string
	err error
}

func (g listenGlobal) ListenPolicy(context.Context) (string, error) { return g.v, g.err }

// listenCaller is the identity of an anonymous visitor or a signed-in
// listener.
type listenCaller struct{ p identitydomain.Principal }

func (c listenCaller) Principal(context.Context) identitydomain.Principal { return c.p }

func (c listenCaller) Authorize(_ context.Context, role identitydomain.Role) error {
	switch {
	case c.p.IsAnonymous():
		return identitydomain.ErrUnauthenticated
	case !c.p.Has(role):
		return identitydomain.ErrForbidden
	}

	return nil
}

func (listenCaller) SessionRef(context.Context) string { return "" }

func (listenCaller) CheckSession(context.Context, *http.Request) (time.Time, error) {
	return time.Time{}, nil
}

// signedInListener returns the principal of a listener's session.
func signedInListener(t *testing.T) identitydomain.Principal {
	t.Helper()

	uid, err := identitydomain.NewUserID(shared.MustParseUUID("01900000-0000-7000-8000-0000000000a1"))
	if err != nil {
		t.Fatal(err)
	}

	sid, err := identitydomain.NewSessionID(shared.MustParseUUID("01900000-0000-7000-8000-0000000000b1"))
	if err != nil {
		t.Fatal(err)
	}

	name, err := identitydomain.NewUsername("ada")
	if err != nil {
		t.Fatal(err)
	}

	u, err := identitydomain.RehydrateUser(identitydomain.UserState{ID: uid, Username: name, Enabled: true, Origin: identitydomain.OriginDB, Version: 1})
	if err != nil {
		t.Fatal(err)
	}

	s, err := identitydomain.RehydrateSession(identitydomain.SessionState{ID: sid, UserID: uid, Provider: identitydomain.ProviderLocal})
	if err != nil {
		t.Fatal(err)
	}

	return identitydomain.UserPrincipal(u, s)
}

// TestListenPolicySingleSource: decodes, files, events, bookmarks and the
// feature summary all follow grid/app.ListenPolicies. Visitors get the
// devices whose effective policy is anonymous (the device override, else
// the global policy); signed-in listeners every enabled device; a global
// policy that is missing, invalid or unreadable counts as registered.
// Signed-in users also see the files of every device.
func TestListenPolicySingleSource(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	now := time.Now()

	nodes, repo := gridsqlite.NewNodeRepository(a), gridsqlite.NewDeviceRepository(a)
	if err := nodes.Create(ctx, domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("attic"), domain.MustNodeURL("https://attic:8074"), now)); err != nil {
		t.Fatal(err)
	}

	type device struct {
		override string
		enabled  bool
	}

	devices := map[string]device{
		"open":    {override: domain.ListenAnonymous, enabled: true},
		"closed":  {override: domain.ListenRegistered, enabled: true},
		"inherit": {enabled: true},
		"off":     {override: domain.ListenAnonymous},
	}

	for id, dv := range devices {
		d, err := domain.NewReportedDevice(domain.MustNodeID("attic"), domain.DeviceSpec{
			ID: shared.MustDeviceID(id), Name: id, Type: "rtl_sdr", Enabled: dv.enabled, FreqMin: 1, FreqMax: 2,
			SampleRates: []int64{1}, ListenPolicy: dv.override,
		}, 0, now)
		if err != nil {
			t.Fatal(err)
		}

		if err := repo.Save(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	callers := map[string]listenCaller{"anonymous": {p: identitydomain.Anonymous()}, "listener": {p: signedInListener(t)}}

	globals := map[string]listenGlobal{
		"anonymous":  {v: domain.ListenAnonymous},
		"registered": {v: domain.ListenRegistered},
		"missing":    {},
		"garbage":    {v: "everyone"},
		"unreadable": {v: domain.ListenAnonymous, err: errors.New("settings down")},
	}

	everything, err := bookmarks.NewRange(nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	for gname, global := range globals {
		effectiveGlobal := global.v
		if global.err != nil || (global.v != domain.ListenAnonymous && global.v != domain.ListenRegistered) {
			effectiveGlobal = domain.ListenRegistered
		}

		for cname, caller := range callers {
			t.Run(gname+"/"+cname, func(t *testing.T) {
				anonymous := cname == "anonymous"
				listen := gridapp.NewListenPolicies(repo, global, quiet)
				features := gridapp.NewFeatures(gridapp.FeaturesDeps{
					Devices: repo, Caps: gridsqlite.NewCapabilityRepository(a), Listen: listen,
				})

				bm, err := bookmarks.New(bookmarks.Deps{
					DB: a, Devices: bookmarkDevices{features: features},
					CanListen: listenAs(listen, caller.Principal), Region: func() string { return "r1" }, Now: time.Now, Logger: quiet,
				})
				if err != nil {
					t.Fatal(err)
				}

				visible, err := visibleDevices(decodesDeps{identity: caller, features: features})(ctx)
				if err != nil {
					t.Fatal(err)
				}

				decoded := map[string]bool{}
				for _, d := range visible {
					decoded[d.ID] = true
				}

				res, err := api.NewFeatureHandlers(caller, features).GetFeatures(ctx, api.GetFeaturesRequestObject{})
				if err != nil {
					t.Fatal(err)
				}

				listed := map[string]bool{}
				for _, d := range res.(api.GetFeatures200JSONResponse).Devices {
					listed[d.Id] = true
				}

				files := fileAccess{
					signedIn: func(ctx context.Context) bool { return caller.Authorize(ctx, identitydomain.RoleListener) == nil },
					policies: listen, logger: quiet,
				}.visibility(ctx)
				topics := topicAuthz{id: caller, policies: listen}

				// The open device is anonymous-listenable: nobody is sent to
				// sign in.
				if files.SignIn {
					t.Error("files send the caller to sign in")
				}

				for id, dv := range devices {
					policy := dv.override
					if policy == "" {
						policy = effectiveGlobal
					}

					want := dv.enabled && (!anonymous || policy == domain.ListenAnonymous)

					wantFiles := !anonymous || want // signed-in users see every file

					_, bmErr := bm.ForDevice(ctx, id, everything)
					if bmErr != nil && !errors.Is(bmErr, bookmarks.ErrDeviceNotFound) {
						t.Fatal(bmErr)
					}

					got := map[string]bool{
						"decodes":   decoded[id],
						"features":  listed[id],
						"events":    topics.AuthorizeTopic(ctx, events.MustTopic("decodes:device="+id)) == nil,
						"bookmarks": bmErr == nil,
					}

					for consumer, ok := range got {
						if ok != want {
							t.Errorf("%s: %s = %v, want %v", id, consumer, ok, want)
						}
					}

					if ok := files.Allows(id); ok != wantFiles {
						t.Errorf("%s: files = %v, want %v", id, ok, wantFiles)
					}
				}
			})
		}
	}
}

// TestFilesSignInOnlyUnderRegistered: the Files gate sends a visitor to sign
// in only under the registered global policy (also when it is missing,
// invalid or unreadable) and when no device is open to visitors; under
// the anonymous policy a visitor gets the gallery, possibly empty.
func TestFilesSignInOnlyUnderRegistered(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	now := time.Now()

	nodes, repo := gridsqlite.NewNodeRepository(a), gridsqlite.NewDeviceRepository(a)
	if err := nodes.Create(ctx, domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("attic"), domain.MustNodeURL("https://attic:8074"), now)); err != nil {
		t.Fatal(err)
	}

	d, err := domain.NewReportedDevice(domain.MustNodeID("attic"), domain.DeviceSpec{
		ID: shared.MustDeviceID("closed"), Name: "closed", Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2,
		SampleRates: []int64{1}, ListenPolicy: domain.ListenRegistered,
	}, 0, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.Save(ctx, d); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name   string
		global listenGlobal
		signIn bool
	}{
		{"anonymous", listenGlobal{v: domain.ListenAnonymous}, false},
		{"registered", listenGlobal{v: domain.ListenRegistered}, true},
		{"missing", listenGlobal{}, true},
		{"garbage", listenGlobal{v: "everyone"}, true},
		{"unreadable", listenGlobal{v: domain.ListenAnonymous, err: errors.New("settings down")}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			listen := gridapp.NewListenPolicies(repo, tt.global, quiet)

			for cname, signedIn := range map[string]bool{"anonymous": false, "listener": true} {
				vis := fileAccess{
					signedIn: func(context.Context) bool { return signedIn }, policies: listen, logger: quiet,
				}.visibility(ctx)

				if want := tt.signIn && !signedIn; vis.SignIn != want {
					t.Errorf("%s: sign in = %v, want %v", cname, vis.SignIn, want)
				}

				if vis.Allows("closed") != signedIn {
					t.Errorf("%s: files of closed = %v, want %v", cname, vis.Allows("closed"), signedIn)
				}
			}
		})
	}
}
