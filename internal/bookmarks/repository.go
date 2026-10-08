package bookmarks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Repository stores the bookmarks in the `bookmarks` table.
type Repository struct{ db *db.DB }

// NewRepository returns the repository.
func NewRepository(d *db.DB) *Repository { return &Repository{db: d} }

// Get returns one bookmark.
func (r *Repository) Get(ctx context.Context, id shared.UUID) (*Bookmark, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetBookmark(ctx, id.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBookmarkNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get bookmark %s: %w", id, err)
	}

	return fromRow(row)
}

// InRange returns the bookmarks of [from, to] that apply in region: every
// hub row, the general pack rows and the region's pack rows, by frequency
// (hub rows before pack rows on the same frequency).
func (r *Repository) InRange(ctx context.Context, region string, from, to int64) ([]*Bookmark, error) {
	tag, err := json.Marshal(regionTagPrefix + region)
	if err != nil {
		return nil, fmt.Errorf("encode region tag: %w", err)
	}

	rows, err := sqlc.New(r.db.Reader(ctx)).ListBookmarksInRange(ctx, sqlc.ListBookmarksInRangeParams{
		FromHz: from, ToHz: to, RegionTag: "%" + string(tag) + "%",
	})
	if err != nil {
		return nil, fmt.Errorf("list bookmarks: %w", err)
	}

	return fromRows(rows)
}

// ByOrigin returns every bookmark of an origin, by frequency.
func (r *Repository) ByOrigin(ctx context.Context, o Origin) ([]*Bookmark, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListBookmarksByOrigin(ctx, string(o))
	if err != nil {
		return nil, fmt.Errorf("list %s bookmarks: %w", o, err)
	}

	return fromRows(rows)
}

// KeyTaken reports whether another bookmark than except has this name,
// frequency and modulation.
func (r *Repository) KeyTaken(ctx context.Context, name string, frequency int64, modulation string, except shared.UUID) (bool, error) {
	id := []byte{}
	if !except.IsZero() {
		id = except.Bytes()
	}

	taken, err := sqlc.New(r.db.Reader(ctx)).BookmarkKeyTaken(ctx, sqlc.BookmarkKeyTakenParams{
		Name: name, Frequency: frequency, Modulation: modulation, ExceptID: id,
	})
	if err != nil {
		return false, fmt.Errorf("check bookmark key: %w", err)
	}

	return taken, nil
}

// Create stores a new bookmark.
func (r *Repository) Create(ctx context.Context, b *Bookmark) error {
	s := b.Snapshot()

	tags, locked, err := encodeLists(s)
	if err != nil {
		return err
	}

	err = sqlc.New(r.db.Writer(ctx)).InsertBookmark(ctx, sqlc.InsertBookmarkParams{
		ID: s.ID.Bytes(), Name: s.Name, Frequency: s.Frequency, Modulation: s.Modulation,
		Underlying: nullString(s.Underlying), Description: nullString(s.Description), Scannable: boolInt(s.Scannable),
		Tags: tags, Origin: string(s.Origin), LockedFields: locked, Scope: string(s.Scope.Kind()),
		DeviceID: nullString(s.Scope.Device().String()), PresetID: nullUUID(s.Scope.Preset()), CreatedBy: nullUUID(s.CreatedBy),
		CreatedAt: s.CreatedAt.UnixMilli(), UpdatedAt: s.UpdatedAt.UnixMilli(), Version: int64(s.Version),
	})
	if err != nil {
		return writeError(err, "insert", s.ID)
	}

	return nil
}

// Update stores a changed bookmark of the same origin when its stored
// version is expectedVersion.
func (r *Repository) Update(ctx context.Context, b *Bookmark, expectedVersion int) error {
	s := b.Snapshot()

	tags, _, err := encodeLists(s)
	if err != nil {
		return err
	}

	n, err := sqlc.New(r.db.Writer(ctx)).UpdateBookmark(ctx, sqlc.UpdateBookmarkParams{
		Name: s.Name, Frequency: s.Frequency, Modulation: s.Modulation, Underlying: nullString(s.Underlying),
		Description: nullString(s.Description), Scannable: boolInt(s.Scannable), Tags: tags, Scope: string(s.Scope.Kind()),
		DeviceID: nullString(s.Scope.Device().String()), PresetID: nullUUID(s.Scope.Preset()),
		UpdatedAt: s.UpdatedAt.UnixMilli(), Version: int64(s.Version), ID: s.ID.Bytes(), Origin: string(s.Origin),
		ExpectedVersion: int64(expectedVersion),
	})
	if err != nil {
		return writeError(err, "update", s.ID)
	}

	if n == 0 {
		if _, err := r.Get(ctx, s.ID); err != nil {
			return err
		}

		return ErrVersionConflict
	}

	return nil
}

// Delete deletes a bookmark of an origin.
func (r *Repository) Delete(ctx context.Context, id shared.UUID, o Origin) error {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteBookmark(ctx, sqlc.DeleteBookmarkParams{ID: id.Bytes(), Origin: string(o)})
	if err != nil {
		return writeError(err, "delete", id)
	}

	if n == 0 {
		return ErrBookmarkNotFound
	}

	return nil
}

func writeError(err error, op string, id shared.UUID) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed: bookmarks.name") {
		return ErrDuplicate
	}

	return fmt.Errorf("%s bookmark %s: %w", op, id, err)
}

func encodeLists(s Snapshot) (tags, locked string, err error) {
	t, err := json.Marshal(nonNil(s.Tags))
	if err != nil {
		return "", "", fmt.Errorf("encode bookmark tags: %w", err)
	}

	l, err := json.Marshal(nonNil(s.Locked))
	if err != nil {
		return "", "", fmt.Errorf("encode bookmark locked fields: %w", err)
	}

	return string(t), string(l), nil
}

func fromRows(rows []sqlc.Bookmark) ([]*Bookmark, error) {
	out := make([]*Bookmark, 0, len(rows))

	for _, row := range rows {
		b, err := fromRow(row)
		if err != nil {
			return nil, err
		}

		out = append(out, b)
	}

	return out, nil
}

func fromRow(row sqlc.Bookmark) (*Bookmark, error) {
	id, err := shared.UUIDFromBytes(row.ID)
	if err != nil {
		return nil, fmt.Errorf("bookmark id: %w", err)
	}

	var tags, locked []string
	if err := json.Unmarshal([]byte(row.Tags), &tags); err != nil {
		return nil, fmt.Errorf("bookmark %s tags: %w", id, err)
	}

	if err := json.Unmarshal([]byte(row.LockedFields), &locked); err != nil {
		return nil, fmt.Errorf("bookmark %s locked fields: %w", id, err)
	}

	scope, err := scopeOf(row)
	if err != nil {
		return nil, fmt.Errorf("bookmark %s scope: %w", id, err)
	}

	var by shared.UUID
	if len(row.CreatedBy) > 0 {
		if by, err = shared.UUIDFromBytes(row.CreatedBy); err != nil {
			return nil, fmt.Errorf("bookmark %s creator: %w", id, err)
		}
	}

	if row.Version < 1 || row.Version > math.MaxInt32 {
		return nil, fmt.Errorf("bookmark %s: invalid version %d", id, row.Version)
	}

	b, err := Rehydrate(Snapshot{
		ID: id, Name: row.Name, Frequency: row.Frequency, Modulation: row.Modulation, Underlying: row.Underlying.String,
		Description: row.Description.String, Scannable: row.Scannable == 1, Tags: tags, Origin: Origin(row.Origin),
		Locked: locked, Scope: scope, CreatedBy: by, CreatedAt: time.UnixMilli(row.CreatedAt), UpdatedAt: time.UnixMilli(row.UpdatedAt),
		Version: int(row.Version),
	})
	if err != nil {
		return nil, fmt.Errorf("stored bookmark %s: %w", id, err)
	}

	return b, nil
}

func scopeOf(row sqlc.Bookmark) (Scope, error) {
	switch ScopeKind(row.Scope) {
	case ScopeDevice:
		d, err := shared.NewDeviceID(row.DeviceID.String)
		if err != nil {
			return Scope{}, err
		}

		return OnDevice(d)
	case ScopePreset:
		p, err := shared.UUIDFromBytes(row.PresetID)
		if err != nil {
			return Scope{}, err
		}

		return OnPreset(p)
	case ScopeAll:
		return AllDevices(), nil
	default:
		return Scope{}, ErrInvalidBookmark.WithDetail("unknown scope " + row.Scope)
	}
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullUUID(u shared.UUID) []byte {
	if u.IsZero() {
		return nil
	}

	return u.Bytes()
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}

	return 0
}
