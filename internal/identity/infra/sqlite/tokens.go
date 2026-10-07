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

func optUserID(b []byte) (domain.UserID, error) {
	if len(b) == 0 {
		return domain.UserID{}, nil
	}

	return domain.UserIDFromBytes(b)
}

func idBytes(id domain.UserID) []byte {
	if id.IsZero() {
		return nil
	}

	return id.Bytes()
}

// Invitations is the SQLite InvitationRepository.
type Invitations struct{ db *db.DB }

var _ domain.InvitationRepository = (*Invitations)(nil)

// NewInvitations returns the repository.
func NewInvitations(a *db.DB) *Invitations { return &Invitations{db: a} }

// Add inserts an invitation.
func (r *Invitations) Add(ctx context.Context, i *domain.Invitation) error {
	err := sqlc.New(r.db.Writer(ctx)).InsertInvitation(ctx, sqlc.InsertInvitationParams{
		ID: i.ID().Bytes(), TokenHash: i.TokenHash().Bytes(), Delivery: string(i.Delivery()),
		Email: nullString(i.Email().String()), RoleID: int64(i.Role().ID()), DeviceID: nullString(i.Device().String()),
		CreatedBy: idBytes(i.CreatedBy()), CreatedAt: ms(i.CreatedAt()), ExpiresAt: ms(i.ExpiresAt()),
		RedeemedAt: nullMS(i.RedeemedAt()), RedeemedUserID: idBytes(i.RedeemedUserID()), RevokedAt: nullMS(i.RevokedAt()),
	})
	if err != nil {
		return fmt.Errorf("insert invitation %s: %w", i.ID(), err)
	}

	return nil
}

// Save stores the redemption or revocation of a pending invitation. It
// returns ErrInvitationNotPending when the stored row is no longer pending.
func (r *Invitations) Save(ctx context.Context, i *domain.Invitation) error {
	n, err := sqlc.New(r.db.Writer(ctx)).UpdateInvitation(ctx, sqlc.UpdateInvitationParams{
		RedeemedAt: nullMS(i.RedeemedAt()), RedeemedUserID: idBytes(i.RedeemedUserID()), RevokedAt: nullMS(i.RevokedAt()),
		ID: i.ID().Bytes(),
	})
	if err != nil {
		return fmt.Errorf("update invitation %s: %w", i.ID(), err)
	}

	if n == 0 {
		return domain.ErrInvitationNotPending
	}

	return nil
}

// ByID returns an invitation.
func (r *Invitations) ByID(ctx context.Context, id domain.InvitationID) (*domain.Invitation, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetInvitationByID(ctx, id.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrInvitationNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load invitation %s: %w", id, err)
	}

	return rehydrateInvitation(row)
}

// ByTokenHash returns the invitation of a token.
func (r *Invitations) ByTokenHash(ctx context.Context, h domain.TokenHash) (*domain.Invitation, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetInvitationByTokenHash(ctx, h.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrInvitationInvalid
	}

	if err != nil {
		return nil, fmt.Errorf("load invitation: %w", err)
	}

	return rehydrateInvitation(row)
}

// List returns the invitations, newest first.
func (r *Invitations) List(ctx context.Context, limit int) ([]*domain.Invitation, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListInvitations(ctx, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}

	out := make([]*domain.Invitation, 0, len(rows))

	for _, row := range rows {
		i, err := rehydrateInvitation(row)
		if err != nil {
			return nil, err
		}

		out = append(out, i)
	}

	return out, nil
}

// ClearEmailOfUser removes the address of the invitations a user redeemed.
func (r *Invitations) ClearEmailOfUser(ctx context.Context, id domain.UserID) error {
	if err := sqlc.New(r.db.Writer(ctx)).ClearInvitationEmailOfUser(ctx, id.Bytes()); err != nil {
		return fmt.Errorf("clear invitation e-mails of user %s: %w", id, err)
	}

	return nil
}

// DeleteEndedBefore deletes ended invitations.
func (r *Invitations) DeleteEndedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteEndedInvitations(ctx, sqlc.DeleteEndedInvitationsParams{Cutoff: ms(cutoff), Batch: int64(limit)})
	if err != nil {
		return 0, fmt.Errorf("delete ended invitations: %w", err)
	}

	return int(n), nil
}

func rehydrateInvitation(row sqlc.Invitation) (*domain.Invitation, error) {
	id, err := domain.InvitationIDFromBytes(row.ID)
	if err != nil {
		return nil, err
	}

	hash, err := domain.NewTokenHash(row.TokenHash)
	if err != nil {
		return nil, err
	}

	role, err := domain.RoleFromID(row.RoleID)
	if err != nil {
		return nil, err
	}

	s := domain.InvitationStateData{
		ID: id, TokenHash: hash, Delivery: domain.Delivery(row.Delivery), Role: role,
		CreatedAt: fromMS(row.CreatedAt), ExpiresAt: fromMS(row.ExpiresAt),
		RedeemedAt: fromNullMS(row.RedeemedAt), RevokedAt: fromNullMS(row.RevokedAt),
	}

	if row.Email.Valid {
		if s.Email, err = domain.NewEmail(row.Email.String); err != nil {
			return nil, err
		}
	}

	if row.DeviceID.Valid {
		if s.Device, err = domain.NewDeviceID(row.DeviceID.String); err != nil {
			return nil, err
		}
	}

	if s.CreatedBy, err = optUserID(row.CreatedBy); err != nil {
		return nil, err
	}

	if s.RedeemedUserID, err = optUserID(row.RedeemedUserID); err != nil {
		return nil, err
	}

	i, err := domain.RehydrateInvitation(s)
	if err != nil {
		return nil, fmt.Errorf("rehydrate invitation %s: %w", id, err)
	}

	return i, nil
}

// PasswordResets is the SQLite PasswordResetRepository.
type PasswordResets struct{ db *db.DB }

var _ domain.PasswordResetRepository = (*PasswordResets)(nil)

// NewPasswordResets returns the repository.
func NewPasswordResets(a *db.DB) *PasswordResets { return &PasswordResets{db: a} }

// Add invalidates the user's unused tokens and inserts t, in one
// transaction.
func (r *PasswordResets) Add(ctx context.Context, t *domain.PasswordResetToken) error {
	return r.db.WithinTx(ctx, func(ctx context.Context) error {
		q := sqlc.New(r.db.Writer(ctx))

		if err := q.InvalidatePasswordResetTokens(ctx, sqlc.InvalidatePasswordResetTokensParams{Now: ms(t.CreatedAt()), UserID: t.UserID().Bytes()}); err != nil {
			return fmt.Errorf("invalidate reset tokens: %w", err)
		}

		if err := q.InsertPasswordResetToken(ctx, sqlc.InsertPasswordResetTokenParams{
			ID: t.ID().Bytes(), UserID: t.UserID().Bytes(), TokenHash: t.TokenHash().Bytes(), CreatedAt: ms(t.CreatedAt()),
			ExpiresAt: ms(t.ExpiresAt()), UsedAt: nullMS(t.UsedAt()), RequestedIp: nullString(t.RequestedIP()),
			SentTo: nullString(t.SentTo().String()),
		}); err != nil {
			return fmt.Errorf("insert reset token: %w", err)
		}

		return nil
	})
}

// InvalidateForUser marks every unused reset token of a user used.
func (r *PasswordResets) InvalidateForUser(ctx context.Context, id domain.UserID, now time.Time) error {
	if err := sqlc.New(r.db.Writer(ctx)).InvalidatePasswordResetTokens(ctx, sqlc.InvalidatePasswordResetTokensParams{Now: ms(now), UserID: id.Bytes()}); err != nil {
		return fmt.Errorf("invalidate reset tokens: %w", err)
	}

	return nil
}

// Save stores the use of a token; it returns ErrInvalidToken when it was
// used meanwhile.
func (r *PasswordResets) Save(ctx context.Context, t *domain.PasswordResetToken) error {
	n, err := sqlc.New(r.db.Writer(ctx)).UsePasswordResetToken(ctx, sqlc.UsePasswordResetTokenParams{UsedAt: nullMS(t.UsedAt()), ID: t.ID().Bytes()})
	if err != nil {
		return fmt.Errorf("use reset token: %w", err)
	}

	if n == 0 {
		return domain.ErrInvalidToken
	}

	return nil
}

// ByTokenHash returns the token of a hash.
func (r *PasswordResets) ByTokenHash(ctx context.Context, h domain.TokenHash) (*domain.PasswordResetToken, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetPasswordResetToken(ctx, h.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrInvalidToken
	}

	if err != nil {
		return nil, fmt.Errorf("load reset token: %w", err)
	}

	s, err := oneTimeState(row.ID, row.UserID, row.TokenHash, row.CreatedAt, row.ExpiresAt, row.UsedAt)
	if err != nil {
		return nil, err
	}

	var sentTo domain.Email
	if row.SentTo.Valid {
		if sentTo, err = domain.NewEmail(row.SentTo.String); err != nil {
			return nil, err
		}
	}

	return domain.RehydratePasswordResetToken(s, row.RequestedIp.String, sentTo)
}

// DeleteEndedBefore deletes ended reset tokens.
func (r *PasswordResets) DeleteEndedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteEndedPasswordResetTokens(ctx, sqlc.DeleteEndedPasswordResetTokensParams{Cutoff: ms(cutoff), Batch: int64(limit)})
	if err != nil {
		return 0, fmt.Errorf("delete ended reset tokens: %w", err)
	}

	return int(n), nil
}

// EmailChanges is the SQLite EmailChangeRepository.
type EmailChanges struct{ db *db.DB }

var _ domain.EmailChangeRepository = (*EmailChanges)(nil)

// NewEmailChanges returns the repository.
func NewEmailChanges(a *db.DB) *EmailChanges { return &EmailChanges{db: a} }

// Add invalidates the user's unused tokens and inserts t.
func (r *EmailChanges) Add(ctx context.Context, t *domain.EmailChangeToken) error {
	return r.db.WithinTx(ctx, func(ctx context.Context) error {
		q := sqlc.New(r.db.Writer(ctx))

		if err := q.InvalidateEmailChangeTokens(ctx, sqlc.InvalidateEmailChangeTokensParams{Now: ms(t.CreatedAt()), UserID: t.UserID().Bytes()}); err != nil {
			return fmt.Errorf("invalidate e-mail tokens: %w", err)
		}

		if err := q.InsertEmailChangeToken(ctx, sqlc.InsertEmailChangeTokenParams{
			ID: t.ID().Bytes(), UserID: t.UserID().Bytes(), TokenHash: t.TokenHash().Bytes(), NewEmail: t.NewEmail().String(),
			CreatedAt: ms(t.CreatedAt()), ExpiresAt: ms(t.ExpiresAt()), UsedAt: nullMS(t.UsedAt()),
		}); err != nil {
			return fmt.Errorf("insert e-mail token: %w", err)
		}

		return nil
	})
}

// InvalidateForUser marks every unused e-mail token of a user used.
func (r *EmailChanges) InvalidateForUser(ctx context.Context, id domain.UserID, now time.Time) error {
	if err := sqlc.New(r.db.Writer(ctx)).InvalidateEmailChangeTokens(ctx, sqlc.InvalidateEmailChangeTokensParams{Now: ms(now), UserID: id.Bytes()}); err != nil {
		return fmt.Errorf("invalidate e-mail tokens: %w", err)
	}

	return nil
}

// Save stores the use of a token.
func (r *EmailChanges) Save(ctx context.Context, t *domain.EmailChangeToken) error {
	n, err := sqlc.New(r.db.Writer(ctx)).UseEmailChangeToken(ctx, sqlc.UseEmailChangeTokenParams{UsedAt: nullMS(t.UsedAt()), ID: t.ID().Bytes()})
	if err != nil {
		return fmt.Errorf("use e-mail token: %w", err)
	}

	if n == 0 {
		return domain.ErrInvalidToken
	}

	return nil
}

// ByTokenHash returns the token of a hash.
func (r *EmailChanges) ByTokenHash(ctx context.Context, h domain.TokenHash) (*domain.EmailChangeToken, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetEmailChangeToken(ctx, h.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrInvalidToken
	}

	if err != nil {
		return nil, fmt.Errorf("load e-mail token: %w", err)
	}

	s, err := oneTimeState(row.ID, row.UserID, row.TokenHash, row.CreatedAt, row.ExpiresAt, row.UsedAt)
	if err != nil {
		return nil, err
	}

	email, err := domain.NewEmail(row.NewEmail)
	if err != nil {
		return nil, err
	}

	return domain.RehydrateEmailChangeToken(s, email)
}

// DeleteEndedBefore deletes ended e-mail tokens.
func (r *EmailChanges) DeleteEndedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteEndedEmailChangeTokens(ctx, sqlc.DeleteEndedEmailChangeTokensParams{Cutoff: ms(cutoff), Batch: int64(limit)})
	if err != nil {
		return 0, fmt.Errorf("delete ended e-mail tokens: %w", err)
	}

	return int(n), nil
}

func oneTimeState(id, user, hash []byte, created, expires int64, used sql.NullInt64) (domain.OneTimeState, error) {
	tid, err := domain.TokenIDFromBytes(id)
	if err != nil {
		return domain.OneTimeState{}, err
	}

	uid, err := domain.UserIDFromBytes(user)
	if err != nil {
		return domain.OneTimeState{}, err
	}

	h, err := domain.NewTokenHash(hash)
	if err != nil {
		return domain.OneTimeState{}, err
	}

	return domain.OneTimeState{ID: tid, UserID: uid, TokenHash: h, CreatedAt: fromMS(created), ExpiresAt: fromMS(expires), UsedAt: fromNullMS(used)}, nil
}
