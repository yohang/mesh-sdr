package app_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/identity/infra/keyring"
	"github.com/yohang/mesh-sdr/internal/identity/infra/memory"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

type devices map[string][]app.NodeDevice

func (d devices) NodeDevices(_ context.Context, node string) ([]app.NodeDevice, error) {
	return d[node], nil
}

type binder struct{ ok bool }

func (b binder) Bound(context.Context, string, string, app.Actor) (bool, error) { return b.ok, nil }

func (e *env) tokens(t *testing.T, b app.ConnectionBinder) (*app.Tokens, *keyring.Keyring) {
	t.Helper()

	k, err := keyring.Open(keyring.Options{Dir: filepath.Join(t.TempDir(), "keys"), TokenTTL: 5 * time.Minute}, t0)
	if err != nil {
		t.Fatal(err)
	}

	d := devices{"roof": {
		{ID: "hf", OperatorCanRetune: true},
		{ID: "vhf", ListenPolicy: domain.ListenRegistered},
		{ID: "ads-b"},
	}}

	return app.NewTokens(app.TokensDeps{
		Signer: k, Devices: d, Binder: b, Settings: app.DefaultSettings{}, Limiter: memory.NewKeyLimiter(time.Minute, 3, 10),
		Issuer: "https://hub.example", TTL: 5 * time.Minute, Now: e.clock.Now, Logger: slog.New(slog.DiscardHandler),
	}), k
}

func verify(t *testing.T, k *keyring.Keyring, raw string) token.Claims {
	t.Helper()

	jwks, revoked := k.Published(t0)

	set, err := token.NewKeySet(jwks, revoked)
	if err != nil {
		t.Fatal(err)
	}

	c, err := token.Verify(raw, set, token.Expect{Issuer: "https://hub.example", Audience: token.Audience("roof"), Now: t0})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	return c
}

func TestMintForAUser(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "op", "", domain.RoleListener)
	acc := e.accounts(nil)
	root := e.addUser(t, "root", "", domain.RoleAdmin)
	_ = root

	u, _ := e.users.ByUsername(ctx, mustName(t, "op"))
	if _, err := acc.SetRoles(ctx, e.actor(t, "root"), u.ID(), []domain.RoleGrant{grant(t, domain.RoleOperator, "hf")}); err != nil {
		t.Fatal(err)
	}

	tokens, k := e.tokens(t, nil)
	in, _ := e.login("op", password)

	m, err := tokens.Mint(ctx, app.MintInput{
		By: app.Actor{Principal: in.Principal}, SessionRef: in.Session.Ref(), NodeID: "roof", ConnectionID: "c-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	c := verify(t, k, m.Raw)
	if c.Subject != u.ID().String() || c.SessionID != in.Session.Ref() || c.ConnectionID != "c-1" ||
		!c.ExpiresAt.Equal(t0.Add(5*time.Minute)) || !slices.Contains(c.Roles, "operator") {
		t.Errorf("claims = %+v", c)
	}

	// hf: operator there and retune allowed; vhf: registered, listen only;
	// ads-b: listen only.
	if !c.Allows("hf", token.PermRetune) || !c.Allows("vhf", token.PermListen) || c.Allows("vhf", token.PermPreset) ||
		!c.Allows("ads-b", token.PermDemod) || c.Allows("ads-b", token.PermPreset) {
		t.Errorf("scopes = %+v", c.Scopes)
	}

	for range 2 {
		_, _ = tokens.Mint(ctx, app.MintInput{By: app.Actor{Principal: in.Principal}, SessionRef: in.Session.Ref(), NodeID: "roof", ConnectionID: "c-1"})
	}

	var rl *domain.RateLimitError
	if _, err := tokens.Mint(ctx, app.MintInput{By: app.Actor{Principal: in.Principal}, SessionRef: in.Session.Ref(), NodeID: "roof", ConnectionID: "c-1"}); !errors.As(err, &rl) {
		t.Errorf("4th mint in a minute: %v", err)
	}
}

func mustName(t *testing.T, s string) domain.Username {
	t.Helper()

	n, err := domain.NewUsername(s)
	if err != nil {
		t.Fatal(err)
	}

	return n
}

func TestMintRefusals(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)
	in, _ := e.login("alice", password)
	alice := app.Actor{Principal: in.Principal}

	tokens, _ := e.tokens(t, nil)

	if _, err := tokens.Mint(ctx, app.MintInput{By: app.Actor{}, NodeID: "roof", ConnectionID: "c"}); !errors.Is(err, domain.ErrAnonymousTokens) {
		t.Errorf("anonymous without the gateway: %v", err)
	}

	if _, err := tokens.Mint(ctx, app.MintInput{By: alice, NodeID: "elsewhere", ConnectionID: "c"}); !errors.Is(err, domain.ErrNoListenableDevice) {
		t.Errorf("node without devices: %v", err)
	}

	if _, err := tokens.Mint(ctx, app.MintInput{By: alice, NodeID: "roof", ConnectionID: "bad cid"}); !errors.Is(err, domain.ErrInvalidConnection) {
		t.Errorf("bad cid: %v", err)
	}

	bound, k := e.tokens(t, binder{ok: true})

	m, err := bound.Mint(ctx, app.MintInput{By: app.Actor{}, NodeID: "roof", ConnectionID: "anon-1"})
	if err != nil {
		t.Fatalf("anonymous with a bound connection: %v", err)
	}

	// Anonymous: only the devices open to anonymous listeners.
	if c := verify(t, k, m.Raw); c.Subject != token.AnonymousSubject || c.SessionID != "" || len(c.Scopes) != 2 || c.Allows("vhf", token.PermListen) {
		t.Errorf("anonymous claims = %+v", c)
	}

	notBound, _ := e.tokens(t, binder{ok: false})
	if _, err := notBound.Mint(ctx, app.MintInput{By: alice, NodeID: "roof", ConnectionID: "c"}); !errors.Is(err, domain.ErrInvalidConnection) {
		t.Errorf("connection of someone else: %v", err)
	}
}
