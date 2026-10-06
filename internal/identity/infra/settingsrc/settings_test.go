package settingsrc_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/identity/infra/settingsrc"
)

type values struct {
	strs  map[string]string
	ints  map[string]int
	durs  map[string]time.Duration
	count int
	win   time.Duration
}

func newPolicies(v values) settingsrc.Policies {
	return settingsrc.New(v, slog.New(slog.DiscardHandler))
}

func (v values) String(k string) string           { return v.strs[k] }
func (v values) Int(k string) int                 { return v.ints[k] }
func (v values) Duration(k string) time.Duration  { return v.durs[k] }
func (v values) Rate(string) (int, time.Duration) { return v.count, v.win }

func TestPolicies(t *testing.T) {
	p := newPolicies(values{
		ints: map[string]int{settingsrc.KeyLockoutDelay: 3, settingsrc.KeyLockoutLockAfter: 6},
		durs: map[string]time.Duration{
			settingsrc.KeyLockoutLockFor: time.Hour, settingsrc.KeyLockoutMaxLock: 2 * time.Hour,
			settingsrc.KeyRetentionAudit: 24 * time.Hour,
		},
		count: 10, win: time.Minute,
	})

	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	if got := p.ThrottlePolicy().BlockedUntil(3, now); !got.Equal(now.Add(time.Second)) {
		t.Errorf("delay after 3 failures = %s", got.Sub(now))
	}

	if !p.ThrottlePolicy().Locks(6) || p.ThrottlePolicy().Locks(5) {
		t.Error("lock threshold")
	}

	if every, burst := p.LoginRate(); every != 6*time.Second || burst != 10 {
		t.Errorf("login rate = %s, %d", every, burst)
	}

	if p.AuditRetention() != 30*24*time.Hour {
		t.Errorf("audit retention below the floor: %s", p.AuditRetention())
	}

	if p.SessionRetention() != 30*24*time.Hour {
		t.Errorf("session retention default: %s", p.SessionRetention())
	}

	if n := newPolicies(values{ints: map[string]int{settingsrc.KeyPasswordMin: 14}}).PasswordMinLength(context.Background()); n != 14 {
		t.Errorf("password min length = %d", n)
	}

	if n := newPolicies(values{}).PasswordMinLength(context.Background()); n != 10 {
		t.Errorf("default password min length = %d", n)
	}

	links := newPolicies(values{ints: map[string]int{settingsrc.KeyInvitationTTL: 48, settingsrc.KeyPasswordResetTTL: 15}})
	if d := links.InvitationTTL(context.Background()); d != 48*time.Hour {
		t.Errorf("invitation ttl = %s", d)
	}

	if d := links.PasswordResetTTL(context.Background()); d != 15*time.Minute {
		t.Errorf("reset ttl = %s", d)
	}

	if d := newPolicies(values{}).InvitationTTL(context.Background()); d != 7*24*time.Hour {
		t.Errorf("default invitation ttl = %s", d)
	}

	if d := newPolicies(values{}).PasswordResetTTL(context.Background()); d != 30*time.Minute {
		t.Errorf("default reset ttl = %s", d)
	}

	if lp := newPolicies(values{strs: map[string]string{settingsrc.KeyListenPolicy: "registered"}}).ListenPolicy(context.Background()); lp != domain.ListenRegistered {
		t.Errorf("listen policy = %s", lp)
	}

	if lp := newPolicies(values{strs: map[string]string{settingsrc.KeyListenPolicy: "anonymous"}}).ListenPolicy(context.Background()); lp != domain.ListenAnonymous {
		t.Errorf("anonymous listen policy = %s", lp)
	}

	if every, burst := newPolicies(values{}).LoginRate(); every != 12*time.Second || burst != 5 {
		t.Errorf("default login rate = %s, %d", every, burst)
	}
}

// An unavailable or invalid listen policy fails closed (registered) and is
// logged.
func TestListenPolicyFailsClosed(t *testing.T) {
	for name, v := range map[string]values{
		"unavailable": {},
		"invalid":     {strs: map[string]string{settingsrc.KeyListenPolicy: "everyone"}},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer

			p := settingsrc.New(v, slog.New(slog.NewTextHandler(&buf, nil)))
			if lp := p.ListenPolicy(context.Background()); lp != domain.ListenRegistered {
				t.Errorf("listen policy = %s, want registered", lp)
			}

			if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), settingsrc.KeyListenPolicy) {
				t.Errorf("no warning logged: %q", buf.String())
			}
		})
	}
}
