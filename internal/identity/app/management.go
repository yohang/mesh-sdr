package app

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Search returns a page of users for Admin › Users (ACC-008).
func (s *Accounts) Search(ctx context.Context, q domain.UserQuery) ([]*domain.User, error) {
	users, err := s.users.Search(ctx, q)

	return users, wrap("search users", err)
}

// SetEnabled enables or disables a user. Disabling revokes every session
// of the user (AUTH-003) and refuses the last enabled admin; enabling
// clears the login lock-out. It returns whether the state changed.
func (s *Accounts) SetEnabled(ctx context.Context, by Actor, id domain.UserID, enabled bool) (bool, error) {
	changed := false

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		now := s.now()

		u, err := s.users.ByID(ctx, id)
		if err != nil {
			return err
		}

		if u.Enabled() == enabled {
			return nil
		}

		if !enabled && u.IsAdmin() {
			if err := s.keepAnAdmin(ctx); err != nil {
				return err
			}
		}

		action, n := domain.ActionUserEnable, 0

		if enabled {
			u.Enable(now)
		} else {
			action = domain.ActionUserDisable
			u.Disable(now)
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		if !enabled {
			if n, err = s.sessions.RevokeAllForUser(ctx, id, domain.RevokeUserDisabled, now); err != nil {
				return err
			}
		}

		changed = true

		return s.record(ctx, by, action, id, map[string]string{"enabled": strconv.FormatBool(!enabled)},
			map[string]string{"enabled": strconv.FormatBool(enabled), "revoked_sessions": strconv.Itoa(n)})
	})
	if err != nil {
		return false, wrap("set enabled", err)
	}

	if changed && !enabled {
		s.revocations.PublishRevocation(ctx, Revocation{Users: []domain.UserID{id}})
	}

	if changed {
		s.logger.InfoContext(ctx, "user enabled state changed", slog.String("user_id", id.String()), slog.Bool("enabled", enabled))
	}

	return changed, nil
}

// SetDisplayName changes a user's display name (admin); empty removes it.
func (s *Accounts) SetDisplayName(ctx context.Context, by Actor, id domain.UserID, name string) error {
	var display domain.DisplayName

	if name != "" {
		var err error
		if display, err = domain.NewDisplayName(name); err != nil {
			return err
		}
	}

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := s.users.ByID(ctx, id)
		if err != nil {
			return err
		}

		if !u.SetDisplayName(display, s.now()) {
			return nil
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		return s.record(ctx, by, domain.ActionUserUpdate, id, nil, map[string]string{"field": "display_name"})
	})

	return wrap("set display name", err)
}

// SetGeneratedPassword gives a user a random password that must be changed
// at the next sign-in (ACC-008), revokes every session of the user and
// returns the password, to show once to the admin.
func (s *Accounts) SetGeneratedPassword(ctx context.Context, by Actor, id domain.UserID) (string, error) {
	generated := GeneratePassword()

	pw, err := newPassword(generated, s.policies.Password(ctx))
	if err != nil {
		return "", err
	}

	hash, err := s.hasher.Hash(ctx, pw.Reveal())
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		now := s.now()

		u, err := s.users.ByID(ctx, id)
		if err != nil {
			return err
		}

		if err := u.ResetPassword(hash, true, now); err != nil {
			return err
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		n, err := s.sessions.RevokeAllForUser(ctx, id, domain.RevokePasswordReset, now)
		if err != nil {
			return err
		}

		return s.record(ctx, by, domain.ActionUserPasswordReset, id, nil,
			map[string]string{"must_change_password": "true", "revoked_sessions": strconv.Itoa(n)})
	})
	if err != nil {
		return "", wrap("set generated password", err)
	}

	s.revocations.PublishRevocation(ctx, Revocation{Users: []domain.UserID{id}})
	s.logger.InfoContext(ctx, "generated password set", slog.String("user_id", id.String()))

	return generated, nil
}
