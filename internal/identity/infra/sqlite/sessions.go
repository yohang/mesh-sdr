package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Sessions is the SQLite SessionRepository.
type Sessions struct{ db db.Adapter }

var _ domain.SessionRepository = (*Sessions)(nil)

// NewSessions returns the repository.
func NewSessions(a db.Adapter) *Sessions { return &Sessions{db: a} }

// Add inserts a session.
func (r *Sessions) Add(ctx context.Context, s *domain.Session) error {
	err := sqlc.New(r.db.Writer(ctx)).InsertSession(ctx, sqlc.InsertSessionParams{
		ID:                s.ID().Bytes(),
		TokenHash:         s.TokenHash().Bytes(),
		CsrfSecret:        s.CSRFSecret().Bytes(),
		UserID:            s.UserID().Bytes(),
		AuthProvider:      s.Provider().String(),
		CreatedAt:         ms(s.CreatedAt()),
		LastSeenAt:        ms(s.LastSeenAt()),
		IdleExpiresAt:     ms(s.IdleExpiresAt()),
		AbsoluteExpiresAt: ms(s.AbsoluteExpiresAt()),
		Ip:                nullIP(s.IP()),
		UserAgent:         nullString(s.UserAgent()),
		RevokedAt:         nullMS(s.RevokedAt()),
		RevokeReason:      nullString(string(s.RevokeReason())),
	})
	if err != nil {
		return fmt.Errorf("insert session %s: %w", s.ID(), err)
	}

	return nil
}

// Save stores the activity and revocation of a session.
func (r *Sessions) Save(ctx context.Context, s *domain.Session) error {
	err := sqlc.New(r.db.Writer(ctx)).UpdateSession(ctx, sqlc.UpdateSessionParams{
		LastSeenAt:    ms(s.LastSeenAt()),
		IdleExpiresAt: ms(s.IdleExpiresAt()),
		RevokedAt:     nullMS(s.RevokedAt()),
		RevokeReason:  nullString(string(s.RevokeReason())),
		ID:            s.ID().Bytes(),
	})
	if err != nil {
		return fmt.Errorf("update session %s: %w", s.ID(), err)
	}

	return nil
}

// ByTokenHash returns a session by the hash of its cookie value.
func (r *Sessions) ByTokenHash(ctx context.Context, h domain.TokenHash) (*domain.Session, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetSessionByTokenHash(ctx, h.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrSessionNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}

	s, err := rehydrateSession(row)
	if err != nil {
		return nil, fmt.Errorf("rehydrate session %x: %w", row.ID, err)
	}

	return s, nil
}

func rehydrateSession(row sqlc.Session) (*domain.Session, error) {
	id, err := domain.SessionIDFromBytes(row.ID)
	if err != nil {
		return nil, err
	}

	uid, err := domain.UserIDFromBytes(row.UserID)
	if err != nil {
		return nil, err
	}

	hash, err := domain.NewTokenHash(row.TokenHash)
	if err != nil {
		return nil, err
	}

	secret, err := domain.RehydrateCSRFSecret(row.CsrfSecret)
	if err != nil {
		return nil, err
	}

	provider, err := domain.NewProviderID(row.AuthProvider)
	if err != nil {
		return nil, err
	}

	var reason domain.RevokeReason
	if row.RevokeReason.Valid {
		if reason, err = domain.ParseRevokeReason(row.RevokeReason.String); err != nil {
			return nil, err
		}
	}

	return domain.RehydrateSession(domain.SessionState{
		ID: id, TokenHash: hash, CSRFSecret: secret, UserID: uid, Provider: provider,
		CreatedAt: fromMS(row.CreatedAt), LastSeenAt: fromMS(row.LastSeenAt),
		IdleExpiresAt: fromMS(row.IdleExpiresAt), AbsoluteExpiresAt: fromMS(row.AbsoluteExpiresAt),
		IP: parseIP(row.Ip), UserAgent: row.UserAgent.String,
		RevokedAt: fromNullMS(row.RevokedAt), RevokeReason: reason,
	})
}

// RevokeAllForUser revokes every unrevoked session of a user.
func (r *Sessions) RevokeAllForUser(ctx context.Context, id domain.UserID, reason domain.RevokeReason, now time.Time) (int, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).RevokeUserSessions(ctx, sqlc.RevokeUserSessionsParams{
		RevokedAt: nullMS(now), RevokeReason: nullString(string(reason)), UserID: id.Bytes(),
	})
	if err != nil {
		return 0, fmt.Errorf("revoke sessions of user %s: %w", id, err)
	}

	return int(n), nil
}

// DeleteEndedBefore deletes up to limit sessions that expired or were
// revoked before cutoff.
func (r *Sessions) DeleteEndedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteEndedSessions(ctx, sqlc.DeleteEndedSessionsParams{
		Cutoff: nullMS(cutoff), Batch: int64(limit),
	})
	if err != nil {
		return 0, fmt.Errorf("delete ended sessions: %w", err)
	}

	return int(n), nil
}
