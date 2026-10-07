package domain

import (
	"context"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Setting is one admin-set DB setting (a `settings` row): the "DB" layer of
// the precedence config > DB > default. Its version is the settings
// revision of its last write, so a version is never reused, even after the
// key was reset (the row deleted) and written again.
type Setting struct {
	key       string
	value     Value
	version   int64
	updatedBy shared.UUID
	updatedAt time.Time
}

// NewSetting returns a setting written at revision by a user (zero UUID when
// unknown).
func NewSetting(key string, value Value, revision int64, by shared.UUID, at time.Time) (*Setting, error) {
	if !ValidKey(key) || value.IsNull() || revision < 1 || at.IsZero() {
		return nil, ErrInvalidSettingRow
	}

	return &Setting{key: key, value: value, version: revision, updatedBy: by, updatedAt: at.UTC().Truncate(time.Millisecond)}, nil
}

// Replace sets a new value, written at revision.
func (s *Setting) Replace(value Value, revision int64, by shared.UUID, at time.Time) error {
	if value.IsNull() || revision <= s.version || at.IsZero() {
		return ErrInvalidSettingRow
	}

	s.value, s.version, s.updatedBy, s.updatedAt = value, revision, by, at.UTC().Truncate(time.Millisecond)

	return nil
}

// Key returns the key.
func (s *Setting) Key() string { return s.key }

// Value returns the stored value.
func (s *Setting) Value() Value { return s.value }

// Version returns the revision of the last write.
func (s *Setting) Version() int64 { return s.version }

// UpdatedBy returns the author of the last write (zero when unknown).
func (s *Setting) UpdatedBy() shared.UUID { return s.updatedBy }

// UpdatedAt returns the time of the last write.
func (s *Setting) UpdatedAt() time.Time { return s.updatedAt }

// Repository persists DB settings. Writes run in the caller's transaction.
type Repository interface {
	// List returns every row, keys the schema no longer defines included.
	List(ctx context.Context) ([]*Setting, error)
	// Get returns the row of key, or nil when there is none.
	Get(ctx context.Context, key string) (*Setting, error)
	// Save inserts or replaces the row of s.
	Save(ctx context.Context, s *Setting) error
	// Delete removes the row of key (no error when there is none).
	Delete(ctx context.Context, key string) error
	// NextRevision increments and returns the settings revision.
	NextRevision(ctx context.Context) (int64, error)
	// Revision returns the current settings revision.
	Revision(ctx context.Context) (int64, error)
}
