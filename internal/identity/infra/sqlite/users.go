package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// IDGenerator returns new UUIDv7 surrogate keys (for user_roles rows).
type IDGenerator interface {
	New(now time.Time) (shared.UUID, error)
}

// Users is the SQLite UserRepository.
type Users struct {
	db  *db.DB
	ids IDGenerator
}

var _ domain.UserRepository = (*Users)(nil)

// NewUsers returns the repository.
func NewUsers(a *db.DB, ids IDGenerator) *Users { return &Users{db: a, ids: ids} }

// Add inserts the user, its identities and its role grants in one
// transaction.
func (r *Users) Add(ctx context.Context, u *domain.User) error {
	return r.db.WithinTx(ctx, func(ctx context.Context) error {
		q := sqlc.New(r.db.Writer(ctx))

		err := q.InsertUser(ctx, sqlc.InsertUserParams{
			ID:                 u.ID().Bytes(),
			Username:           u.Username().String(),
			Email:              nullString(u.Email().String()),
			EmailVerifiedAt:    nullMS(u.EmailVerifiedAt()),
			DisplayName:        nullString(u.DisplayName().String()),
			PasswordHash:       nullString(u.PasswordHash().String()),
			MustChangePassword: boolInt(u.MustChangePassword()),
			Enabled:            boolInt(u.Enabled()),
			FailedLoginCount:   int64(u.FailedLogins()),
			LockedUntil:        nullMS(u.LockedUntil()),
			LastLoginAt:        nullMS(u.LastLoginAt()),
			Origin:             string(u.Origin()),
			CreatedAt:          ms(u.CreatedAt()),
			UpdatedAt:          ms(u.UpdatedAt()),
			Version:            int64(u.Version()),
		})
		if err != nil {
			return uniqueViolation(err, u)
		}

		for _, i := range u.Identities() {
			if err := q.InsertUserIdentity(ctx, sqlc.InsertUserIdentityParams{
				UserID: u.ID().Bytes(), Provider: i.Provider().String(), Subject: i.Subject(), CreatedAt: ms(u.CreatedAt()),
			}); err != nil {
				return fmt.Errorf("insert identity of user %s: %w", u.ID(), err)
			}
		}

		for _, g := range u.Grants() {
			id, err := r.ids.New(u.CreatedAt())
			if err != nil {
				return fmt.Errorf("role grant id: %w", err)
			}

			if err := q.InsertUserRole(ctx, sqlc.InsertUserRoleParams{
				ID: id.Bytes(), UserID: u.ID().Bytes(), RoleID: int64(g.Role().ID()),
				DeviceID: nullString(g.Device().String()), GrantedAt: ms(u.CreatedAt()),
			}); err != nil {
				return fmt.Errorf("insert role grant of user %s: %w", u.ID(), err)
			}
		}

		return nil
	})
}

func uniqueViolation(err error, u *domain.User) error {
	msg := err.Error()

	switch {
	case strings.Contains(msg, "ux_users_username_lower"):
		return domain.ErrUsernameTaken
	case strings.Contains(msg, "ux_users_email_lower"):
		return domain.ErrEmailTaken
	default:
		return fmt.Errorf("insert user %s: %w", u.ID(), err)
	}
}

// Save updates the user's mutable fields when its version matches, and its
// role grants when they changed, in one transaction.
func (r *Users) Save(ctx context.Context, u *domain.User) error {
	return r.db.WithinTx(ctx, func(ctx context.Context) error {
		q := sqlc.New(r.db.Writer(ctx))

		n, err := q.UpdateUser(ctx, sqlc.UpdateUserParams{
			Email:              nullString(u.Email().String()),
			EmailVerifiedAt:    nullMS(u.EmailVerifiedAt()),
			DisplayName:        nullString(u.DisplayName().String()),
			PasswordHash:       nullString(u.PasswordHash().String()),
			MustChangePassword: boolInt(u.MustChangePassword()),
			Enabled:            boolInt(u.Enabled()),
			FailedLoginCount:   int64(u.FailedLogins()),
			LockedUntil:        nullMS(u.LockedUntil()),
			LastLoginAt:        nullMS(u.LastLoginAt()),
			UpdatedAt:          ms(u.UpdatedAt()),
			ID:                 u.ID().Bytes(),
			Version:            int64(u.Version()),
		})
		if err != nil {
			if strings.Contains(err.Error(), "ux_users_email_lower") {
				return domain.ErrEmailTaken
			}

			return fmt.Errorf("update user %s: %w", u.ID(), err)
		}

		if n == 0 {
			return domain.ErrVersionConflict
		}

		if by, at := u.GrantedBy(); !at.IsZero() {
			if err := r.syncGrants(ctx, q, u, by, at); err != nil {
				return err
			}
		}

		u.Saved()

		return nil
	})
}

// syncGrants deletes the stored grants the user no longer has and inserts
// the new ones; unchanged rows keep their granted_by and granted_at.
func (r *Users) syncGrants(ctx context.Context, q *sqlc.Queries, u *domain.User, by domain.UserID, at time.Time) error {
	rows, err := q.ListUserRoles(ctx, u.ID().Bytes())
	if err != nil {
		return fmt.Errorf("load role grants: %w", err)
	}

	stored := map[string]bool{}

	for _, row := range rows {
		key := fmt.Sprint(row.RoleID, "/", row.DeviceID.String)
		stored[key] = true
		keep := false

		for _, g := range u.Grants() {
			if int64(g.Role().ID()) == row.RoleID && g.Device().String() == row.DeviceID.String {
				keep = true
			}
		}

		if !keep {
			if err := q.DeleteUserRole(ctx, sqlc.DeleteUserRoleParams{UserID: u.ID().Bytes(), RoleID: row.RoleID, DeviceID: row.DeviceID.String}); err != nil {
				return fmt.Errorf("delete role grant: %w", err)
			}
		}
	}

	var grantedBy []byte
	if !by.IsZero() {
		grantedBy = by.Bytes()
	}

	for _, g := range u.Grants() {
		if stored[fmt.Sprint(int64(g.Role().ID()), "/", g.Device().String())] {
			continue
		}

		id, err := r.ids.New(at)
		if err != nil {
			return fmt.Errorf("role grant id: %w", err)
		}

		if err := q.InsertUserRole(ctx, sqlc.InsertUserRoleParams{
			ID: id.Bytes(), UserID: u.ID().Bytes(), RoleID: int64(g.Role().ID()),
			DeviceID: nullString(g.Device().String()), GrantedBy: grantedBy, GrantedAt: ms(at),
		}); err != nil {
			return fmt.Errorf("insert role grant: %w", err)
		}
	}

	return nil
}

// Search returns a page of users matching q.
func (r *Users) Search(ctx context.Context, uq domain.UserQuery) ([]*domain.User, error) {
	q := sqlc.New(r.db.Reader(ctx))

	enabled := int64(-1)
	if uq.Enabled != nil {
		enabled = boolInt(*uq.Enabled)
	}

	role := int64(0)
	if uq.Role != domain.RoleAnonymous {
		role = int64(uq.Role.ID())
	}

	limit := uq.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	rows, err := q.SearchUsers(ctx, sqlc.SearchUsersParams{
		Text: uq.Text, Enabled: enabled, NeverLoggedIn: boolInt(uq.NeverLoggedIn), RoleID: role,
		MaxRows: int64(limit), SkipRows: int64(max(uq.Offset, 0)),
	})
	if err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}

	out := make([]*domain.User, 0, len(rows))

	for _, row := range rows {
		u, err := r.load(ctx, q, func() (sqlc.User, error) { return row, nil })
		if err != nil {
			return nil, err
		}

		out = append(out, u)
	}

	return out, nil
}

// Delete deletes a user; its identities, grants, sessions and tokens go
// with it (ON DELETE CASCADE).
func (r *Users) Delete(ctx context.Context, id domain.UserID) error {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteUser(ctx, id.Bytes())
	if err != nil {
		return fmt.Errorf("delete user %s: %w", id, err)
	}

	if n == 0 {
		return domain.ErrUserNotFound
	}

	return nil
}

// Usernames returns the usernames of the given users that exist.
func (r *Users) Usernames(ctx context.Context, ids []domain.UserID) (map[domain.UserID]domain.Username, error) {
	out := map[domain.UserID]domain.Username{}
	if len(ids) == 0 {
		return out, nil
	}

	raw := make([][]byte, len(ids))
	for i, id := range ids {
		raw[i] = id.Bytes()
	}

	rows, err := sqlc.New(r.db.Reader(ctx)).ListUsernames(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("list usernames: %w", err)
	}

	for _, row := range rows {
		id, err := domain.UserIDFromBytes(row.ID)
		if err != nil {
			continue
		}

		if name, err := domain.NewUsername(row.Username); err == nil {
			out[id] = name
		}
	}

	return out, nil
}

// ByID returns a user.
func (r *Users) ByID(ctx context.Context, id domain.UserID) (*domain.User, error) {
	q := sqlc.New(r.db.Reader(ctx))

	return r.load(ctx, q, func() (sqlc.User, error) { return q.GetUserByID(ctx, id.Bytes()) })
}

// ByUsername returns a user by case-insensitive username.
func (r *Users) ByUsername(ctx context.Context, name domain.Username) (*domain.User, error) {
	q := sqlc.New(r.db.Reader(ctx))

	return r.load(ctx, q, func() (sqlc.User, error) { return q.GetUserByUsername(ctx, name.String()) })
}

// ByLogin returns a user by username or, for a login containing '@', by
// e-mail.
func (r *Users) ByLogin(ctx context.Context, l domain.Login) (*domain.User, error) {
	q := sqlc.New(r.db.Reader(ctx))

	if l.IsEmail() {
		return r.load(ctx, q, func() (sqlc.User, error) { return q.GetUserByEmail(ctx, l.String()) })
	}

	return r.load(ctx, q, func() (sqlc.User, error) { return q.GetUserByUsername(ctx, l.String()) })
}

// ByIdentity returns the user owning a login identity.
func (r *Users) ByIdentity(ctx context.Context, i domain.Identity) (*domain.User, error) {
	q := sqlc.New(r.db.Reader(ctx))

	return r.load(ctx, q, func() (sqlc.User, error) {
		return q.GetUserByIdentity(ctx, sqlc.GetUserByIdentityParams{Provider: i.Provider().String(), Subject: i.Subject()})
	})
}

// List returns the enabled users (every user with includeDisabled), by
// case-insensitive username.
func (r *Users) List(ctx context.Context, includeDisabled bool) ([]*domain.User, error) {
	q := sqlc.New(r.db.Reader(ctx))

	minEnabled := int64(1)
	if includeDisabled {
		minEnabled = 0
	}

	rows, err := q.ListUsers(ctx, minEnabled)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	out := make([]*domain.User, 0, len(rows))

	for _, row := range rows {
		u, err := r.load(ctx, q, func() (sqlc.User, error) { return row, nil })
		if err != nil {
			return nil, err
		}

		out = append(out, u)
	}

	return out, nil
}

// CountEnabledAdmins returns the number of enabled global admins. Inside a
// write transaction it reads the transaction's view.
func (r *Users) CountEnabledAdmins(ctx context.Context) (int, error) {
	n, err := sqlc.New(r.db.Reader(ctx)).CountEnabledAdmins(ctx)
	if err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}

	return int(n), nil
}

func (r *Users) load(ctx context.Context, q *sqlc.Queries, get func() (sqlc.User, error)) (*domain.User, error) {
	row, err := get()
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrUserNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}

	identities, err := q.ListUserIdentities(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("load identities: %w", err)
	}

	roles, err := q.ListUserRoles(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("load role grants: %w", err)
	}

	u, err := rehydrateUser(row, identities, roles)
	if err != nil {
		return nil, fmt.Errorf("rehydrate user %x: %w", row.ID, err)
	}

	return u, nil
}

func rehydrateUser(row sqlc.User, identities []sqlc.ListUserIdentitiesRow, roles []sqlc.ListUserRolesRow) (*domain.User, error) {
	id, err := domain.UserIDFromBytes(row.ID)
	if err != nil {
		return nil, err
	}

	name, err := domain.NewUsername(row.Username)
	if err != nil {
		return nil, err
	}

	s := domain.UserState{
		ID: id, Username: name, EmailVerifiedAt: fromNullMS(row.EmailVerifiedAt),
		MustChangePassword: row.MustChangePassword == 1, Enabled: row.Enabled == 1,
		FailedLogins: int(row.FailedLoginCount), LockedUntil: fromNullMS(row.LockedUntil),
		LastLoginAt: fromNullMS(row.LastLoginAt), Origin: domain.Origin(row.Origin),
		CreatedAt: fromMS(row.CreatedAt), UpdatedAt: fromMS(row.UpdatedAt), Version: int(row.Version),
	}

	if row.Email.Valid {
		if s.Email, err = domain.NewEmail(row.Email.String); err != nil {
			return nil, err
		}
	}

	if row.DisplayName.Valid {
		if s.DisplayName, err = domain.NewDisplayName(row.DisplayName.String); err != nil {
			return nil, err
		}
	}

	// A malformed stored hash keeps the user loadable: it is left out, so
	// password login fails for this account only (AUTH-017).
	if row.PasswordHash.Valid {
		s.PasswordHash, _ = domain.NewPasswordHash(row.PasswordHash.String)
	}

	for _, i := range identities {
		p, err := domain.NewProviderID(i.Provider)
		if err != nil {
			return nil, err
		}

		ident, err := domain.NewIdentity(p, i.Subject)
		if err != nil {
			return nil, err
		}

		s.Identities = append(s.Identities, ident)
	}

	for _, g := range roles {
		role, err := domain.RoleFromID(g.RoleID)
		if err != nil {
			return nil, err
		}

		var dev shared.DeviceID
		if g.DeviceID.Valid {
			if dev, err = shared.NewDeviceID(g.DeviceID.String); err != nil {
				return nil, err
			}
		}

		grant, err := domain.NewRoleGrant(role, dev)
		if err != nil {
			return nil, err
		}

		s.Grants = append(s.Grants, grant)
	}

	return domain.RehydrateUser(s)
}
