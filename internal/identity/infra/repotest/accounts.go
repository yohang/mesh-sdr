package repotest

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func newID(t *testing.T) []byte {
	t.Helper()

	u, err := ids.New(t0)
	if err != nil {
		t.Fatal(err)
	}

	return u.Bytes()
}

func tokenID(t *testing.T) domain.TokenID {
	t.Helper()

	id, err := domain.TokenIDFromBytes(newID(t))
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func invitationID(t *testing.T) domain.InvitationID {
	t.Helper()

	id, err := domain.InvitationIDFromBytes(newID(t))
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func addUser(t *testing.T, r Repos, name, email string, grants ...domain.RoleGrant) *domain.User {
	t.Helper()

	u := NewUser(t, name, email, grants...)
	if err := r.Users.Add(context.Background(), u); err != nil {
		t.Fatal(err)
	}

	return u
}

// RunAccounts checks the contracts added by the accounts epic: user search,
// grant changes and deletion, session lists, invitations, one-time tokens
// and audit search.
func RunAccounts(t *testing.T, open Factory) {
	ctx := context.Background()
	admin, _ := domain.NewRoleGrant(domain.RoleAdmin, shared.DeviceID{})
	op, _ := domain.NewRoleGrant(domain.RoleOperator, shared.DeviceID{})
	dev, _ := shared.NewDeviceID("rtl-1")
	opDev, _ := domain.NewRoleGrant(domain.RoleOperator, dev)

	t.Run("grants change", func(t *testing.T) {
		r := open(t)
		boss := addUser(t, r, "boss", "", admin)
		u := addUser(t, r, "alice", "", opDev)

		if !u.ReplaceGrants([]domain.RoleGrant{op, opDev}, boss.ID(), t0.Add(time.Hour)) {
			t.Fatal("no change")
		}

		if err := r.Users.Save(ctx, u); err != nil {
			t.Fatal(err)
		}

		got, _ := r.Users.ByID(ctx, u.ID())
		if g := got.Grants(); len(g) != 2 || got.Role() != domain.RoleOperator {
			t.Errorf("grants = %v", g)
		}

		got.ReplaceGrants(nil, boss.ID(), t0.Add(2*time.Hour))

		if err := r.Users.Save(ctx, got); err != nil {
			t.Fatal(err)
		}

		if again, _ := r.Users.ByID(ctx, u.ID()); len(again.Grants()) != 0 || again.Role() != domain.RoleListener {
			t.Errorf("grants after removal = %v", again.Grants())
		}
	})

	t.Run("search, usernames and delete", func(t *testing.T) {
		r := open(t)
		a := addUser(t, r, "anna", "anna@example.org", admin)
		b := addUser(t, r, "bob", "", op)
		c := addUser(t, r, "carl", "carl@corp.example")

		c.Disable(t0)

		if err := r.Users.Save(ctx, c); err != nil {
			t.Fatal(err)
		}

		names := func(q domain.UserQuery) []string {
			us, err := r.Users.Search(ctx, q)
			if err != nil {
				t.Fatal(err)
			}

			out := []string{}
			for _, u := range us {
				out = append(out, u.Username().String())
			}

			return out
		}

		off, on := false, true

		for _, tc := range []struct {
			q    domain.UserQuery
			want string
		}{
			{domain.UserQuery{}, "anna bob carl"},
			{domain.UserQuery{Text: "CORP"}, "carl"},
			{domain.UserQuery{Text: "display b"}, "bob"},
			{domain.UserQuery{Role: domain.RoleAdmin}, "anna"},
			{domain.UserQuery{Role: domain.RoleListener}, "carl"},
			{domain.UserQuery{Enabled: &off}, "carl"},
			{domain.UserQuery{Enabled: &on, NeverLoggedIn: true}, "anna bob"},
			{domain.UserQuery{After: "anna", Limit: 1}, "bob"},
		} {
			if got := names(tc.q); joinSpace(got) != tc.want {
				t.Errorf("search %+v = %v, want %s", tc.q, got, tc.want)
			}
		}

		m, err := r.Users.Usernames(ctx, []domain.UserID{a.ID(), b.ID(), userID(t)})
		if err != nil || len(m) != 2 || m[a.ID()].String() != "anna" {
			t.Errorf("usernames = %v, %v", m, err)
		}

		if err := r.Users.Delete(ctx, b.ID()); err != nil {
			t.Fatal(err)
		}

		if _, err := r.Users.ByID(ctx, b.ID()); !errors.Is(err, domain.ErrUserNotFound) {
			t.Errorf("deleted user: %v", err)
		}

		if err := r.Users.Delete(ctx, b.ID()); !errors.Is(err, domain.ErrUserNotFound) {
			t.Errorf("delete twice: %v", err)
		}
	})

	t.Run("active sessions of a user", func(t *testing.T) {
		r := open(t)
		u := addUser(t, r, "alice", "")
		p := domain.DefaultSessionPolicy()

		var sessions []*domain.Session

		for i := range 3 {
			s, _, err := domain.StartSession(domain.StartSessionParams{
				ID: sessionID(t), UserID: u.ID(), Provider: domain.ProviderLocal, Policy: p, Now: t0.Add(time.Duration(i) * time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}

			if err := r.Sessions.Add(ctx, s); err != nil {
				t.Fatal(err)
			}

			sessions = append(sessions, s)
		}

		sessions[1].Revoke(domain.RevokeLogout, t0.Add(time.Hour))

		if err := r.Sessions.Revoke(ctx, sessions[1]); err != nil {
			t.Fatal(err)
		}

		got, err := r.Sessions.ActiveForUser(ctx, u.ID(), t0.Add(2*time.Hour))
		if err != nil || len(got) != 2 || got[0].ID() != sessions[2].ID() {
			t.Fatalf("active = %d, %v", len(got), err)
		}

		if none, _ := r.Sessions.ActiveForUser(ctx, u.ID(), t0.Add(48*time.Hour)); len(none) != 0 {
			t.Errorf("expired sessions listed: %d", len(none))
		}
	})

	t.Run("invitations", func(t *testing.T) {
		r := open(t)
		boss := addUser(t, r, "boss", "", admin)
		mail, _ := domain.NewEmail("new@example.org")

		inv, tok, err := domain.NewInvitation(domain.NewInvitationParams{
			ID: invitationID(t), Role: domain.RoleOperator, Device: dev, Email: mail, Delivery: domain.DeliveryEmail,
			CreatedBy: boss.ID(), Now: t0, TTL: 7 * 24 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}

		if err := r.Invitations.Add(ctx, inv); err != nil {
			t.Fatal(err)
		}

		got, err := r.Invitations.ByTokenHash(ctx, tok.Hash())
		if err != nil || got.ID() != inv.ID() || got.Email() != mail || got.Device() != dev || got.CreatedBy() != boss.ID() ||
			got.StateAt(t0) != domain.InvitationPending {
			t.Fatalf("by token = %+v, %v", got, err)
		}

		if _, err := r.Invitations.ByTokenHash(ctx, domain.NewLinkToken().Hash()); !errors.Is(err, domain.ErrInvitationInvalid) {
			t.Errorf("unknown token: %v", err)
		}

		invitee := addUser(t, r, "invitee", "")
		if err := got.Redeem(invitee.ID(), t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}

		if err := r.Invitations.Save(ctx, got); err != nil {
			t.Fatal(err)
		}

		// A stale copy cannot revoke a redeemed invitation.
		stale, _ := r.Invitations.ByID(ctx, inv.ID())
		if stale.StateAt(t0.Add(2*time.Hour)) != domain.InvitationRedeemed || stale.RedeemedUserID() != invitee.ID() {
			t.Errorf("stored state = %v", stale.StateAt(t0))
		}

		if err := r.Invitations.Save(ctx, inv); !errors.Is(err, domain.ErrInvitationNotPending) {
			t.Errorf("save of a stale pending copy: %v", err)
		}

		if err := r.Invitations.ClearEmailOfUser(ctx, invitee.ID()); err != nil {
			t.Fatal(err)
		}

		if cleared, _ := r.Invitations.ByID(ctx, inv.ID()); !cleared.Email().IsZero() {
			t.Error("e-mail kept")
		}

		list, err := r.Invitations.List(ctx, 10)
		if err != nil || len(list) != 1 {
			t.Errorf("list = %d, %v", len(list), err)
		}

		if _, err := r.Invitations.ByID(ctx, invitationID(t)); !errors.Is(err, domain.ErrInvitationNotFound) {
			t.Errorf("unknown id: %v", err)
		}

		if n, err := r.Invitations.DeleteEndedBefore(ctx, t0.Add(31*24*time.Hour), 10); err != nil || n != 1 {
			t.Errorf("deleted %d, %v", n, err)
		}
	})

	t.Run("password reset tokens", func(t *testing.T) {
		r := open(t)
		u := addUser(t, r, "alice", "")

		first, firstTok, _ := domain.NewPasswordResetToken(tokenID(t), u.ID(), "192.0.2.1", t0, 30*time.Minute)
		sent, _ := domain.NewEmail("alice@example.org")
		first.MailTo(sent)
		second, secondTok, _ := domain.NewPasswordResetToken(tokenID(t), u.ID(), "", t0.Add(time.Minute), 30*time.Minute)

		for _, tok := range []*domain.PasswordResetToken{first, second} {
			if err := r.Resets.Add(ctx, tok); err != nil {
				t.Fatal(err)
			}
		}

		// Issuing the second token invalidated the first.
		if got, err := r.Resets.ByTokenHash(ctx, firstTok.Hash()); err != nil || got.ValidAt(t0.Add(2*time.Minute)) || got.RequestedIP() != "192.0.2.1" ||
			got.SentTo() != sent {
			t.Errorf("first token = %v", err)
		}

		got, err := r.Resets.ByTokenHash(ctx, secondTok.Hash())
		if err != nil || !got.ValidAt(t0.Add(2*time.Minute)) {
			t.Fatalf("second token: %v", err)
		}

		if err := got.Use(t0.Add(2 * time.Minute)); err != nil {
			t.Fatal(err)
		}

		if err := r.Resets.Save(ctx, got); err != nil {
			t.Fatal(err)
		}

		if err := r.Resets.Save(ctx, got); !errors.Is(err, domain.ErrInvalidToken) {
			t.Errorf("second use: %v", err)
		}

		if _, err := r.Resets.ByTokenHash(ctx, domain.NewLinkToken().Hash()); !errors.Is(err, domain.ErrInvalidToken) {
			t.Errorf("unknown token: %v", err)
		}

		third, thirdTok, _ := domain.NewPasswordResetToken(tokenID(t), u.ID(), "", t0, 30*time.Minute)
		if err := r.Resets.Add(ctx, third); err != nil {
			t.Fatal(err)
		}

		if err := r.Resets.InvalidateForUser(ctx, u.ID(), t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}

		if got, _ := r.Resets.ByTokenHash(ctx, thirdTok.Hash()); got.ValidAt(t0.Add(2 * time.Minute)) {
			t.Error("token valid after InvalidateForUser")
		}

		if n, err := r.Resets.DeleteEndedBefore(ctx, t0.Add(48*time.Hour), 10); err != nil || n != 3 {
			t.Errorf("deleted %d, %v", n, err)
		}
	})

	t.Run("e-mail change tokens", func(t *testing.T) {
		r := open(t)
		u := addUser(t, r, "alice", "")
		mail, _ := domain.NewEmail("next@example.org")

		tok, link, _ := domain.NewEmailChangeToken(tokenID(t), u.ID(), mail, t0)
		if err := r.EmailChanges.Add(ctx, tok); err != nil {
			t.Fatal(err)
		}

		got, err := r.EmailChanges.ByTokenHash(ctx, link.Hash())
		if err != nil || got.NewEmail() != mail || got.UserID() != u.ID() {
			t.Fatalf("token = %v", err)
		}

		_ = got.Use(t0.Add(time.Minute))
		if err := r.EmailChanges.Save(ctx, got); err != nil {
			t.Fatal(err)
		}

		if err := r.EmailChanges.Save(ctx, got); !errors.Is(err, domain.ErrInvalidToken) {
			t.Errorf("second use: %v", err)
		}

		other, otherLink, _ := domain.NewEmailChangeToken(tokenID(t), u.ID(), mail, t0)
		if err := r.EmailChanges.Add(ctx, other); err != nil {
			t.Fatal(err)
		}

		if err := r.EmailChanges.InvalidateForUser(ctx, u.ID(), t0); err != nil {
			t.Fatal(err)
		}

		if again, _ := r.EmailChanges.ByTokenHash(ctx, otherLink.Hash()); again.ValidAt(t0.Add(time.Minute)) {
			t.Error("e-mail token valid after InvalidateForUser")
		}

		if n, err := r.EmailChanges.DeleteEndedBefore(ctx, t0.Add(48*time.Hour), 10); err != nil || n != 2 {
			t.Errorf("deleted %d, %v", n, err)
		}
	})

	t.Run("audit search", func(t *testing.T) {
		r := open(t)
		u := addUser(t, r, "alice", "")
		ip := netip.MustParseAddr("192.0.2.1")

		for i, action := range []string{domain.ActionLoginSuccess, domain.ActionLoginFailure, domain.ActionUserDisable, "user.role.update"} {
			actor := domain.UserActor(u.ID(), ip)
			if i == 2 {
				actor = domain.CLIActor()
			}

			e, _ := domain.NewAuditEntry(t0.Add(time.Duration(i)*time.Hour), actor, action, domain.ResultOK)
			if err := r.Audit.Append(ctx, e.WithTarget("user", u.ID().String())); err != nil {
				t.Fatal(err)
			}
		}

		search := func(q domain.AuditQuery) []string {
			recs, err := r.Audit.Search(ctx, q)
			if err != nil {
				t.Fatal(err)
			}

			out := []string{}
			for _, rec := range recs {
				out = append(out, rec.Entry.Action())
			}

			return out
		}

		for _, tc := range []struct {
			q    domain.AuditQuery
			want string
		}{
			{domain.AuditQuery{}, "user.role.update user.disable auth.login.failure auth.login.success"},
			{domain.AuditQuery{ActionPrefix: "auth.login."}, "auth.login.failure auth.login.success"},
			{domain.AuditQuery{ActorUserID: u.ID()}, "user.role.update auth.login.failure auth.login.success"},
			{domain.AuditQuery{ActorKind: domain.ActorCLI}, "user.disable"},
			{domain.AuditQuery{TargetType: "user", TargetID: u.ID().String(), From: t0.Add(time.Hour), To: t0.Add(3 * time.Hour)}, "user.disable auth.login.failure"},
			{domain.AuditQuery{Limit: 2}, "user.role.update user.disable"},
		} {
			if got := search(tc.q); joinSpace(got) != tc.want {
				t.Errorf("search %+v = %v, want %s", tc.q, got, tc.want)
			}
		}

		recs, _ := r.Audit.Search(ctx, domain.AuditQuery{Limit: 2})
		if got := search(domain.AuditQuery{BeforeID: recs[1].ID}); joinSpace(got) != "auth.login.failure auth.login.success" {
			t.Errorf("next page = %v", got)
		}
	})
}

func joinSpace(s []string) string {
	out := ""

	for i, v := range s {
		if i > 0 {
			out += " "
		}

		out += v
	}

	return out
}
