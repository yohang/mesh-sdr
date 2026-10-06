package notify_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/identity/infra/notify"
	"github.com/yohang/mesh-sdr/internal/mail"
)

type outbox struct{ got []mail.Message }

func (o *outbox) Enqueue(m mail.Message) error {
	o.got = append(o.got, m)

	return nil
}

func TestMessages(t *testing.T) {
	ctx := context.Background()
	o := &outbox{}
	n := notify.New(o, "https://sdr.example.org/")
	to, _ := domain.NewEmail("alice@example.org")
	next, _ := domain.NewEmail("new@example.org")
	exp := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	for _, err := range []error{
		n.Invitation(ctx, to, "https://sdr.example.org/invite/T1", domain.RoleOperator, exp),
		n.PasswordReset(ctx, to, "https://sdr.example.org/password/reset/T2", exp),
		n.EmailConfirmation(ctx, next, "https://sdr.example.org/account/email/verify/T3", exp),
		n.PasswordChanged(ctx, to, exp),
		n.EmailChanged(ctx, to, next, exp),
		n.Test(ctx, to),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}

	if !n.Enabled() || len(o.got) != 6 {
		t.Fatalf("queued %d", len(o.got))
	}

	inv := o.got[0]
	if inv.To != "alice@example.org" || !strings.Contains(inv.Subject, "sdr.example.org") ||
		!strings.Contains(inv.Body, "/invite/T1") || !strings.Contains(inv.Body, "operator") || !strings.Contains(inv.Body, "2026-10-07 12:00 UTC") {
		t.Errorf("invitation = %+v", inv)
	}

	if o.got[2].To != "new@example.org" || !strings.Contains(o.got[4].Body, "new@example.org") {
		t.Error("e-mail change messages wrong")
	}
}

func TestDisabled(t *testing.T) {
	n := notify.New(nil, "https://sdr.example.org")
	to, _ := domain.NewEmail("alice@example.org")

	if n.Enabled() || !errors.Is(n.Test(context.Background(), to), app.ErrMailDisabled) {
		t.Error("disabled notifier sends")
	}
}
