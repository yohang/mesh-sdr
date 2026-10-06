package app_test

import (
	"context"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// sent is one notification recorded by recNotifier.
type sent struct {
	kind, to, link string
}

// recNotifier records notifications instead of sending them.
type recNotifier struct {
	mu       sync.Mutex
	disabled bool
	sent     []sent
}

func (n *recNotifier) add(kind string, to domain.Email, link string) error {
	if n.disabled {
		return app.ErrMailDisabled
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	n.sent = append(n.sent, sent{kind, to.String(), link})

	return nil
}

func (n *recNotifier) all() []sent {
	n.mu.Lock()
	defer n.mu.Unlock()

	return append([]sent(nil), n.sent...)
}

func (n *recNotifier) Enabled() bool { return !n.disabled }

func (n *recNotifier) Invitation(_ context.Context, to domain.Email, link string, _ domain.Role, _ time.Time) error {
	return n.add("invitation", to, link)
}

func (n *recNotifier) PasswordReset(_ context.Context, to domain.Email, link string, _ time.Time) error {
	return n.add("password_reset", to, link)
}

func (n *recNotifier) EmailConfirmation(_ context.Context, to domain.Email, link string, _ time.Time) error {
	return n.add("email_confirmation", to, link)
}

func (n *recNotifier) PasswordChanged(_ context.Context, to domain.Email, _ time.Time) error {
	return n.add("password_changed", to, "")
}

func (n *recNotifier) EmailChanged(_ context.Context, to, next domain.Email, _ time.Time) error {
	return n.add("email_changed", to, next.String())
}

func (n *recNotifier) Test(_ context.Context, to domain.Email) error { return n.add("test", to, "") }
