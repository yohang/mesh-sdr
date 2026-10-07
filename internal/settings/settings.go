package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Settings is the SQLite settings repository.
type Settings struct {
	db            *db.DB
	schemaVersion int64
}

var _ Repository = (*Settings)(nil)

// NewSettings returns the repository. schemaVersion is the version of the
// settings schema that validates the values it writes
// (`settings.schema_version`).
func NewSettings(a *db.DB, schemaVersion int) *Settings {
	return &Settings{db: a, schemaVersion: int64(schemaVersion)}
}

func rehydrate(key string, value sql.NullString, by []byte, at, version int64) (*Setting, error) {
	if !ValidKey(key) {
		return nil, fmt.Errorf("setting row %q: %w", key, ErrInvalidKey)
	}

	v, err := NewValue([]byte(value.String))
	if err != nil {
		return nil, fmt.Errorf("setting row %q: %w", key, err)
	}

	var author shared.UUID
	if by != nil {
		if author, err = shared.UUIDFromBytes(by); err != nil {
			return nil, fmt.Errorf("setting row %q: %w", key, err)
		}
	}

	s, err := NewSetting(key, v, version, author, time.UnixMilli(at).UTC())
	if err != nil {
		return nil, fmt.Errorf("setting row %q: %w", key, err)
	}

	return s, nil
}

// List implements Repository. Rows that break an invariant are
// skipped: the store ignores invalid values anyway.
func (r *Settings) List(ctx context.Context) ([]*Setting, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}

	out := make([]*Setting, 0, len(rows))

	for _, row := range rows {
		s, err := rehydrate(row.Key, row.Value, row.UpdatedBy, row.UpdatedAt, row.Version)
		if err != nil {
			continue
		}

		out = append(out, s)
	}

	return out, nil
}

// Get implements Repository.
func (r *Settings) Get(ctx context.Context, key string) (*Setting, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetSetting(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // no row is not an error
	}

	if err != nil {
		return nil, fmt.Errorf("get setting %s: %w", key, err)
	}

	return rehydrate(row.Key, row.Value, row.UpdatedBy, row.UpdatedAt, row.Version)
}

// Save implements Repository.
func (r *Settings) Save(ctx context.Context, s *Setting) error {
	var by []byte
	if !s.UpdatedBy().IsZero() {
		by = s.UpdatedBy().Bytes()
	}

	err := sqlc.New(r.db.Writer(ctx)).UpsertSetting(ctx, sqlc.UpsertSettingParams{
		Key:           s.Key(),
		Value:         sql.NullString{String: s.Value().String(), Valid: true},
		SchemaVersion: r.schemaVersion,
		UpdatedBy:     by,
		UpdatedAt:     s.UpdatedAt().UnixMilli(),
		Version:       s.Version(),
	})
	if err != nil {
		return fmt.Errorf("save setting %s: %w", s.Key(), err)
	}

	return nil
}

// Delete implements Repository.
func (r *Settings) Delete(ctx context.Context, key string) error {
	if err := sqlc.New(r.db.Writer(ctx)).DeleteSetting(ctx, key); err != nil {
		return fmt.Errorf("delete setting %s: %w", key, err)
	}

	return nil
}

// NextRevision implements Repository.
func (r *Settings) NextRevision(ctx context.Context) (int64, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).NextSettingsRevision(ctx)
	if err != nil {
		return 0, fmt.Errorf("next settings revision: %w", err)
	}

	return n, nil
}

// Revision implements Repository.
func (r *Settings) Revision(ctx context.Context) (int64, error) {
	n, err := sqlc.New(r.db.Reader(ctx)).SettingsRevision(ctx)
	if err != nil {
		return 0, fmt.Errorf("settings revision: %w", err)
	}

	return n, nil
}
