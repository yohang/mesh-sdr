package app

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// SessionView is an active session as its owner or an admin sees it
// (ACC-005).
type SessionView struct {
	// Ref is the public handle of the session (never its id).
	Ref        string
	Current    bool
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	IP         string
	UserAgent  string
	Browser    string
	System     string
}

func sessionViews(sessions []*domain.Session, current domain.SessionID) []SessionView {
	out := make([]SessionView, 0, len(sessions))

	for _, s := range sessions {
		browser, system := DescribeUserAgent(s.UserAgent())

		v := SessionView{
			Ref: s.Ref(), Current: s.ID() == current, CreatedAt: s.CreatedAt(), LastSeenAt: s.LastSeenAt(),
			ExpiresAt: s.AbsoluteExpiresAt(), UserAgent: s.UserAgent(), Browser: browser, System: system,
		}

		if s.IP().IsValid() {
			v.IP = s.IP().String()
		}

		out = append(out, v)
	}

	return out
}

// DescribeUserAgent names the browser and the operating system of a
// User-Agent header with a few well-known markers; unknown values give
// "Unknown". The raw value is kept for display (as text, SR-21).
func DescribeUserAgent(ua string) (browser, system string) {
	browser, system = "Unknown browser", "Unknown system"

	for _, b := range []struct{ marker, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"},
		{"curl/", "curl"},
	} {
		if strings.Contains(ua, b.marker) {
			browser = b.name

			break
		}
	}

	for _, o := range []struct{ marker, name string }{
		{"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iPadOS"}, {"Windows", "Windows"}, {"Mac OS X", "macOS"},
		{"CrOS", "ChromeOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, o.marker) {
			system = o.name

			break
		}
	}

	return browser, system
}

// OwnSessions returns the active sessions of the actor.
func (s *Accounts) OwnSessions(ctx context.Context, by Actor) ([]SessionView, error) {
	if by.Principal.IsAnonymous() {
		return nil, domain.ErrUnauthenticated
	}

	return s.sessionsOf(ctx, by.Principal.UserID(), by.Principal.SessionID())
}

// UserSessions returns the active sessions of a user (admin).
func (s *Accounts) UserSessions(ctx context.Context, id domain.UserID) ([]SessionView, error) {
	if _, err := s.users.ByID(ctx, id); err != nil {
		return nil, wrap("load user", err)
	}

	return s.sessionsOf(ctx, id, domain.SessionID{})
}

func (s *Accounts) sessionsOf(ctx context.Context, id domain.UserID, current domain.SessionID) ([]SessionView, error) {
	list, err := s.sessions.ActiveForUser(ctx, id, s.now())
	if err != nil {
		return nil, wrap("list sessions", err)
	}

	return sessionViews(list, current), nil
}

// RevokeOwnSession signs out one of the actor's sessions, by handle. A
// handle of another user's session is not found (SR-18).
func (s *Accounts) RevokeOwnSession(ctx context.Context, by Actor, ref string) error {
	if by.Principal.IsAnonymous() {
		return domain.ErrUnauthenticated
	}

	n, err := s.revoke(ctx, by, by.Principal.UserID(), domain.RevokeLogout, false, func(x *domain.Session) bool { return x.Ref() == ref })

	return oneRevoked(n, err)
}

func oneRevoked(n int, err error) error {
	if err == nil && n == 0 {
		return domain.ErrSessionNotFound
	}

	return err
}

// RevokeOtherSessions signs out every session of the actor but the
// request's one, and returns how many.
func (s *Accounts) RevokeOtherSessions(ctx context.Context, by Actor) (int, error) {
	if by.Principal.IsAnonymous() {
		return 0, domain.ErrUnauthenticated
	}

	current := by.Principal.SessionID()

	return s.revoke(ctx, by, by.Principal.UserID(), domain.RevokeLogout, true, func(x *domain.Session) bool { return x.ID() != current })
}

// RevokeAllOwnSessions signs out every session of the actor, the request's
// included (`/auth/logout-all`).
func (s *Accounts) RevokeAllOwnSessions(ctx context.Context, by Actor) (int, error) {
	if by.Principal.IsAnonymous() {
		return 0, domain.ErrUnauthenticated
	}

	return s.revoke(ctx, by, by.Principal.UserID(), domain.RevokeLogout, true, func(*domain.Session) bool { return true })
}

// RevokeUserSession signs out one session of a user (admin).
func (s *Accounts) RevokeUserSession(ctx context.Context, by Actor, id domain.UserID, ref string) error {
	return oneRevoked(s.revoke(ctx, by, id, domain.RevokeAdmin, false, func(x *domain.Session) bool { return x.Ref() == ref }))
}

// RevokeUserSessions signs out every session of a user (admin) and returns
// how many.
func (s *Accounts) RevokeUserSessions(ctx context.Context, by Actor, id domain.UserID) (int, error) {
	if _, err := s.users.ByID(ctx, id); err != nil {
		return 0, wrap("load user", err)
	}

	return s.revoke(ctx, by, id, domain.RevokeAdmin, true, func(*domain.Session) bool { return true })
}

// revoke revokes the active sessions of a user that match, audits each and
// publishes them. everywhere (sign out of other or all sessions) also
// invalidates the user's pending reset and e-mail links.
func (s *Accounts) revoke(ctx context.Context, by Actor, id domain.UserID, reason domain.RevokeReason, everywhere bool,
	match func(*domain.Session) bool,
) (int, error) {
	var refs []string

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		now := s.now()

		list, err := s.sessions.ActiveForUser(ctx, id, now)
		if err != nil {
			return err
		}

		if everywhere {
			if err := s.pending.invalidate(ctx, id, now); err != nil {
				return err
			}
		}

		for _, x := range list {
			if !match(x) || !x.Revoke(reason, now) {
				continue
			}

			if err := s.sessions.Revoke(ctx, x); err != nil {
				return err
			}

			refs = append(refs, x.Ref())
		}

		if len(refs) == 0 {
			return nil
		}

		e, err := domain.NewAuditEntry(now, by.audit(), domain.ActionSessionsRevoke, domain.ResultOK)
		if err != nil {
			return err
		}

		return s.audit.Append(ctx, e.WithTarget("user", id.String()).WithRequestID(by.Meta.RequestID).
			WithAfter(map[string]string{"sessions": strconv.Itoa(len(refs)), "reason": string(reason)}))
	})
	if err != nil {
		return 0, wrap("revoke sessions", err)
	}

	if len(refs) > 0 {
		s.revocations.PublishRevocation(ctx, Revocation{Sessions: refs})
		s.logger.InfoContext(ctx, "sessions revoked", slog.String("user_id", id.String()), slog.Int("sessions", len(refs)),
			slog.String("reason", string(reason)))
	}

	return len(refs), nil
}
