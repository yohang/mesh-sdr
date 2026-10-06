package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// LocalProvider is the form-login provider: username or e-mail plus a
// password verified against users.password_hash (TECHNICAL_SPEC §5.2).
//
// Every attempt runs exactly one password verification, against a dummy
// hash when the account is unknown or has no local password, so that the
// response time does not reveal whether an account exists (SR-06). After a
// successful verification, a hash with outdated parameters is replaced.
type LocalProvider struct {
	users  domain.UserRepository
	tx     Transactor
	hasher PasswordHasher
	now    Clock
	logger *slog.Logger
	dummy  domain.PasswordHash
}

// NewLocalProvider returns the provider. It computes the dummy hash with the
// current parameters.
func NewLocalProvider(ctx context.Context, users domain.UserRepository, tx Transactor, hasher PasswordHasher, now Clock, logger *slog.Logger) (*LocalProvider, error) {
	dummy, err := hasher.Hash(ctx, rand.Text())
	if err != nil {
		return nil, fmt.Errorf("compute the dummy password hash: %w", err)
	}

	return &LocalProvider{users: users, tx: tx, hasher: hasher, now: now, logger: logger, dummy: dummy}, nil
}

// ID implements Provider.
func (p *LocalProvider) ID() domain.ProviderID { return domain.ProviderLocal }

// Authenticate implements Provider.
func (p *LocalProvider) Authenticate(ctx context.Context, c Credentials) (domain.Identity, error) {
	u, err := p.users.ByLogin(ctx, c.Login)
	if err != nil && !errors.Is(err, domain.ErrUserNotFound) {
		return domain.Identity{}, fmt.Errorf("load user: %w", err)
	}

	hash := p.dummy
	if u != nil && u.CanPasswordLogin() {
		hash = u.PasswordHash()
	}

	ok, err := p.hasher.Verify(ctx, c.Password, hash)
	if err != nil {
		if ctx.Err() != nil {
			return domain.Identity{}, fmt.Errorf("verify password: %w", err)
		}

		// A malformed hash makes only this account unusable (AUTH-017).
		p.logger.WarnContext(ctx, "stored password hash is malformed", slog.String("user_id", u.ID().String()), slog.Any("error", err))

		return domain.Identity{}, domain.ErrInvalidCredentials
	}

	if !ok || u == nil || !u.CanPasswordLogin() {
		return domain.Identity{}, domain.ErrInvalidCredentials
	}

	if p.hasher.NeedsRehash(hash) {
		p.rehash(ctx, u.ID(), c.Password)
	}

	return domain.LocalIdentity(u.ID()), nil
}

// rehash replaces an outdated hash. A failure is logged and does not fail
// the login: the next login retries.
func (p *LocalProvider) rehash(ctx context.Context, id domain.UserID, password string) {
	h, err := p.hasher.Hash(ctx, password)
	if err == nil {
		err = p.tx.WithinTx(ctx, func(ctx context.Context) error {
			u, err := p.users.ByID(ctx, id)
			if err != nil {
				return err
			}

			if err := u.ReplacePasswordHash(h, p.now()); err != nil {
				return err
			}

			return p.users.Save(ctx, u)
		})
	}

	if err != nil {
		p.logger.WarnContext(ctx, "password re-hash failed", slog.String("user_id", id.String()), slog.Any("error", err))

		return
	}

	p.logger.DebugContext(ctx, "password re-hashed with the current parameters", slog.String("user_id", id.String()))
}
