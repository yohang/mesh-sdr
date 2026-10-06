package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// SetupTTL is the validity of the first-admin setup link (AUTH-018).
const SetupTTL = time.Hour

// SetupPath is the path of the setup link; the token follows it.
const SetupPath = "/setup"

// Setup is the first-admin bootstrap (AUTH-018): when no enabled admin
// exists at hub start, Begin creates a one-time setup link with a random
// token. The token lives in memory only (its SHA-256), is valid for
// SetupTTL and is consumed by the creation of the first admin; a restart
// issues a new one.
type Setup struct {
	users    domain.UserRepository
	audit    domain.AuditLog
	tx       Transactor
	hasher   PasswordHasher
	ids      IDGenerator
	now      Clock
	policies Policies
	auth     *Auth
	limiter  IPLimiter
	hubURL   string
	logger   *slog.Logger

	mu      sync.Mutex
	hash    [sha256.Size]byte
	expires time.Time
	active  bool
}

// SetupDeps are the dependencies of Setup.
type SetupDeps struct {
	Users    domain.UserRepository
	Audit    domain.AuditLog
	Tx       Transactor
	Hasher   PasswordHasher
	IDs      IDGenerator
	Now      Clock
	Policies Policies
	// Auth opens the session of the new admin.
	Auth *Auth
	// Limiter rate-limits setup requests per client address.
	Limiter IPLimiter
	// HubURL is hub.url: setup links are built from it, never from a
	// request.
	HubURL string
	Logger *slog.Logger
}

// NewSetup returns the service, without a setup link.
func NewSetup(d SetupDeps) *Setup {
	return &Setup{
		users: d.Users, audit: d.Audit, tx: d.Tx, hasher: d.Hasher, ids: d.IDs, now: d.Now, policies: d.Policies,
		auth: d.Auth, limiter: d.Limiter, hubURL: strings.TrimRight(d.HubURL, "/"), logger: d.Logger,
	}
}

// Begin creates the setup link when no enabled admin exists, and returns
// its URL (empty when an admin exists). The caller shows it once.
func (s *Setup) Begin(ctx context.Context) (string, error) {
	n, err := s.users.CountEnabledAdmins(ctx)
	if err != nil {
		return "", fmt.Errorf("check for an admin: %w", err)
	}

	if n > 0 {
		return "", nil
	}

	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)

	s.mu.Lock()
	s.hash, s.expires, s.active = sha256.Sum256([]byte(token)), s.now().Add(SetupTTL), true
	expires := s.expires
	s.mu.Unlock()

	s.logger.WarnContext(ctx, "no admin account: a one-time setup URL to create the first admin was printed on stderr",
		slog.Time("expires_at", expires))

	return s.hubURL + SetupPath + "/" + token, nil
}

// Check reports whether token is the pending, unexpired setup token: it
// returns nil, ErrSetupTokenInvalid, or a *domain.RateLimitError when the
// client address made too many setup requests.
func (s *Setup) Check(token string, meta RequestMeta) error {
	if err := s.allow(meta); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.validLocked(token) {
		return domain.ErrSetupTokenInvalid
	}

	return nil
}

func (s *Setup) allow(meta RequestMeta) error {
	if s.limiter == nil {
		return nil
	}

	if ok, wait := s.limiter.Allow(meta.IP, s.now()); !ok {
		return domain.NewRateLimitError(wait)
	}

	return nil
}

// MinLength returns the minimum password length in force.
func (s *Setup) MinLength(ctx context.Context) int { return s.policies.Password(ctx).MinLength() }

func (s *Setup) validLocked(token string) bool {
	if !s.active || token == "" || !s.now().Before(s.expires) {
		return false
	}

	h := sha256.Sum256([]byte(token))

	return subtle.ConstantTimeCompare(h[:], s.hash[:]) == 1
}

// SetupInput is the first admin account.
type SetupInput struct {
	Token       string
	Username    string
	Email       string // optional
	DisplayName string // optional
	Password    string
	// Previous is the session cookie the browser presented, if any.
	Previous string
	Meta     RequestMeta
}

// Complete creates the first admin with a local password that follows the
// policy, consumes the token and signs the admin in. It returns
// ErrSetupTokenInvalid when the token is not the pending one or an enabled
// admin exists meanwhile, and a *domain.RateLimitError like Check.
func (s *Setup) Complete(ctx context.Context, in SetupInput) (LoginResult, error) {
	if err := s.allow(in.Meta); err != nil {
		return LoginResult{}, err
	}

	// The lock serialises completions: the token is used at most once.
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.validLocked(in.Token) {
		return LoginResult{}, domain.ErrSetupTokenInvalid
	}

	u, err := s.newAdmin(ctx, in)
	if err != nil {
		return LoginResult{}, err
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		n, err := s.users.CountEnabledAdmins(ctx)
		if err != nil {
			return err
		}

		if n > 0 {
			return domain.ErrSetupTokenInvalid
		}

		if err := s.users.Add(ctx, u); err != nil {
			return err
		}

		e, err := domain.NewAuditEntry(s.now(), domain.SystemActor(), domain.ActionUserCreate, domain.ResultOK)
		if err != nil {
			return err
		}

		return s.audit.Append(ctx, e.WithTarget("user", u.ID().String()).WithRequestID(in.Meta.RequestID).
			WithAfter(map[string]string{"role": domain.RoleAdmin.String(), "via": "setup"}))
	})

	if errors.Is(err, domain.ErrSetupTokenInvalid) {
		s.active = false // an admin exists: the link is useless

		return LoginResult{}, err
	}

	if err != nil {
		return LoginResult{}, fmt.Errorf("create the first admin: %w", err)
	}

	s.active = false
	s.logger.InfoContext(ctx, "first admin created through the setup link", slog.String("user_id", u.ID().String()))

	return s.auth.OpenSession(ctx, u.ID(), LoginInput{Previous: in.Previous, Meta: in.Meta})
}

func (s *Setup) newAdmin(ctx context.Context, in SetupInput) (*domain.User, error) {
	name, err := domain.NewUsername(in.Username)
	if err != nil {
		return nil, err
	}

	var email domain.Email
	if in.Email != "" {
		if email, err = domain.NewEmail(in.Email); err != nil {
			return nil, err
		}
	}

	var display domain.DisplayName
	if in.DisplayName != "" {
		if display, err = domain.NewDisplayName(in.DisplayName); err != nil {
			return nil, err
		}
	}

	pw, err := domain.NewPassword(in.Password, s.policies.Password(ctx))
	if err != nil {
		return nil, err
	}

	hash, err := s.hasher.Hash(ctx, pw.Reveal())
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	now := s.now()

	uid, err := s.ids.New(now)
	if err != nil {
		return nil, fmt.Errorf("user id: %w", err)
	}

	id, err := domain.NewUserID(uid)
	if err != nil {
		return nil, err
	}

	admin, err := domain.NewRoleGrant(domain.RoleAdmin, domain.DeviceID{})
	if err != nil {
		return nil, err
	}

	return domain.NewLocalUser(domain.NewLocalUserParams{
		ID: id, Username: name, Email: email, DisplayName: display, PasswordHash: hash,
		Grants: []domain.RoleGrant{admin}, Origin: domain.OriginDB, Now: now,
	})
}
