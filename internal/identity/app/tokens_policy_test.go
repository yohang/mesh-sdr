package app_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// TestScopesRightsMatrix checks the scopes of a refreshed token for
// anonymous visitors, listeners and admins for every global listen policy
// and device override (ACC-012, ACC-013): anonymous visitors only listen,
// and only where the effective policy is anonymous.
func TestScopesRightsMatrix(t *testing.T) {
	e := newEnv(t, nil)
	e.addUser(t, "alice", "", domain.RoleListener)
	e.addUser(t, "root", "", domain.RoleAdmin)
	alice, _ := e.login("alice", password)
	root, _ := e.login("root", password)

	subjects := map[string]domain.Principal{"anonymous": domain.Anonymous(), "listener": alice.Principal, "admin": root.Principal}

	devices := []app.NodeDevice{
		{ID: "inherit", OperatorCanRetune: true},
		{ID: "open", ListenPolicy: domain.ListenAnonymous, OperatorCanRetune: true},
		{ID: "closed", ListenPolicy: domain.ListenRegistered, OperatorCanRetune: true},
	}

	listenOnly := []string{"listen", "demod"}
	full := []string{"listen", "demod", "preset", "retune"}

	cases := []struct {
		global  domain.ListenPolicy
		subject string
		want    map[string][]string
	}{
		{domain.ListenAnonymous, "anonymous", map[string][]string{"inherit": listenOnly, "open": listenOnly}},
		{domain.ListenAnonymous, "listener", map[string][]string{"inherit": listenOnly, "open": listenOnly, "closed": listenOnly}},
		{domain.ListenAnonymous, "admin", map[string][]string{"inherit": full, "open": full, "closed": full}},
		{domain.ListenRegistered, "anonymous", map[string][]string{"open": listenOnly}},
		{domain.ListenRegistered, "listener", map[string][]string{"inherit": listenOnly, "open": listenOnly, "closed": listenOnly}},
		{domain.ListenRegistered, "admin", map[string][]string{"inherit": full, "open": full, "closed": full}},
	}

	for _, tc := range cases {
		t.Run(string(tc.global)+"/"+tc.subject, func(t *testing.T) {
			got := map[string][]string{}
			for _, s := range app.Scopes(subjects[tc.subject], devices, tc.global) {
				got[s.Device] = s.Perms
			}

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

// TestMintAnonymousRateLimited: anonymous refreshes are limited per
// connection; an exhausted bucket refuses with a retry delay.
func TestMintAnonymousRateLimited(t *testing.T) {
	e := newEnv(t, nil)
	tokens, _ := e.tokens(t, binder{ok: true})
	in := app.MintInput{By: app.Actor{}, NodeID: "roof", ConnectionID: "anon-1"}

	// The test limiter allows a burst of 3.
	for i := range 3 {
		if _, err := tokens.Mint(context.Background(), in); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}

	_, err := tokens.Mint(context.Background(), in)

	var rl *domain.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter() <= 0 || !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("fourth mint: %v", err)
	}

	// Another connection has its own bucket.
	in.ConnectionID = "anon-2"
	if _, err := tokens.Mint(context.Background(), in); err != nil {
		t.Fatalf("other connection: %v", err)
	}
}
