package settingsrc_test

import (
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/infra/settingsrc"
)

type values struct {
	ints  map[string]int
	durs  map[string]time.Duration
	count int
	win   time.Duration
}

func (v values) Int(k string) int                 { return v.ints[k] }
func (v values) Duration(k string) time.Duration  { return v.durs[k] }
func (v values) Rate(string) (int, time.Duration) { return v.count, v.win }

func TestPolicies(t *testing.T) {
	p := settingsrc.New(values{
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

	if n := settingsrc.New(values{ints: map[string]int{settingsrc.KeyPasswordMin: 14}}).PasswordMinLength(context.Background()); n != 14 {
		t.Errorf("password min length = %d", n)
	}

	if n := settingsrc.New(values{}).PasswordMinLength(context.Background()); n != 10 {
		t.Errorf("default password min length = %d", n)
	}

	links := settingsrc.New(values{ints: map[string]int{settingsrc.KeyInvitationTTL: 48, settingsrc.KeyPasswordResetTTL: 15}})
	if d := links.InvitationTTL(context.Background()); d != 48*time.Hour {
		t.Errorf("invitation ttl = %s", d)
	}

	if d := links.PasswordResetTTL(context.Background()); d != 15*time.Minute {
		t.Errorf("reset ttl = %s", d)
	}

	if d := settingsrc.New(values{}).InvitationTTL(context.Background()); d != 7*24*time.Hour {
		t.Errorf("default invitation ttl = %s", d)
	}

	if d := settingsrc.New(values{}).PasswordResetTTL(context.Background()); d != 30*time.Minute {
		t.Errorf("default reset ttl = %s", d)
	}

	if every, burst := settingsrc.New(values{}).LoginRate(); every != 12*time.Second || burst != 5 {
		t.Errorf("default login rate = %s, %d", every, burst)
	}
}
