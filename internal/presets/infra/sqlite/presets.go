// Package sqlite implements the preset repository on the SQLite adapter
// (ADR 0006).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/presets/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Presets implements domain.Repository.
type Presets struct{ db *db.DB }

// NewPresets returns the repository.
func NewPresets(a *db.DB) *Presets { return &Presets{db: a} }

var _ domain.Repository = (*Presets)(nil)

// Get implements domain.Repository.
func (r *Presets) Get(ctx context.Context, id shared.UUID) (*domain.Preset, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetPreset(ctx, id.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrPresetNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get preset %s: %w", id, err)
	}

	return fromRow(row)
}

// List implements domain.Repository.
func (r *Presets) List(ctx context.Context) ([]*domain.Preset, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListPresets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list presets: %w", err)
	}

	out := make([]*domain.Preset, 0, len(rows))

	for _, row := range rows {
		p, err := fromRow(row)
		if err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, nil
}

// SlugTaken implements domain.Repository.
func (r *Presets) SlugTaken(ctx context.Context, slug domain.Slug, except shared.UUID) (bool, error) {
	id := []byte{}
	if !except.IsZero() {
		id = except.Bytes()
	}

	n, err := sqlc.New(r.db.Reader(ctx)).PresetSlugTaken(ctx, sqlc.PresetSlugTakenParams{Slug: slug.String(), ID: id})
	if err != nil {
		return false, fmt.Errorf("check preset slug: %w", err)
	}

	return n, nil
}

// NextSortOrder implements domain.Repository.
func (r *Presets) NextSortOrder(ctx context.Context) (int, error) {
	n, err := sqlc.New(r.db.Reader(ctx)).NextPresetSortOrder(ctx)
	if err != nil {
		return 0, fmt.Errorf("next preset position: %w", err)
	}

	return int(n), nil
}

// Create implements domain.Repository.
func (r *Presets) Create(ctx context.Context, p *domain.Preset) error {
	s := p.Snapshot()

	tags, waterfall, err := encode(s)
	if err != nil {
		return err
	}

	err = sqlc.New(r.db.Writer(ctx)).InsertPreset(ctx, sqlc.InsertPresetParams{
		ID: s.ID.Bytes(), Slug: s.Slug, Name: s.Name, Description: nullString(s.Description), Tags: tags,
		CenterFreq: s.CenterFreq, SampRate: s.SampRate, StartFreq: s.StartFreq, StartMod: s.StartMod, TuningStep: s.TuningStep,
		InitialSquelchLevel: nullInt(s.Squelch), InitialNrLevel: nullInt(s.NR), WaterfallLevels: waterfall,
		SortOrder: int64(s.SortOrder), CreatedAt: s.CreatedAt.UnixMilli(), UpdatedAt: s.UpdatedAt.UnixMilli(), Version: int64(s.Version),
	})
	if err != nil {
		return writeError(err, "insert", s.ID)
	}

	return nil
}

// Update implements domain.Repository.
func (r *Presets) Update(ctx context.Context, p *domain.Preset, expectedVersion int) error {
	s := p.Snapshot()

	tags, waterfall, err := encode(s)
	if err != nil {
		return err
	}

	n, err := sqlc.New(r.db.Writer(ctx)).UpdatePreset(ctx, sqlc.UpdatePresetParams{
		Slug: s.Slug, Name: s.Name, Description: nullString(s.Description), Tags: tags, CenterFreq: s.CenterFreq,
		SampRate: s.SampRate, StartFreq: s.StartFreq, StartMod: s.StartMod, TuningStep: s.TuningStep,
		InitialSquelchLevel: nullInt(s.Squelch), InitialNrLevel: nullInt(s.NR), WaterfallLevels: waterfall,
		SortOrder: int64(s.SortOrder), UpdatedAt: s.UpdatedAt.UnixMilli(), Version: int64(s.Version),
		ID: s.ID.Bytes(), ExpectedVersion: int64(expectedVersion),
	})
	if err != nil {
		return writeError(err, "update", s.ID)
	}

	if n == 0 {
		if _, err := r.Get(ctx, s.ID); err != nil {
			return err
		}

		return domain.ErrVersionConflict
	}

	return nil
}

// Delete implements domain.Repository.
func (r *Presets) Delete(ctx context.Context, id shared.UUID) error {
	n, err := sqlc.New(r.db.Writer(ctx)).DeletePreset(ctx, id.Bytes())
	if err != nil {
		return writeError(err, "delete", id)
	}

	if n == 0 {
		return domain.ErrPresetNotFound
	}

	return nil
}

func writeError(err error, op string, id shared.UUID) error {
	msg := err.Error()

	switch {
	case strings.Contains(msg, "presets.slug"):
		return domain.ErrSlugTaken
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return domain.ErrPresetInUse
	default:
		return fmt.Errorf("%s preset %s: %w", op, id, err)
	}
}

func encode(s domain.Snapshot) (string, sql.NullString, error) {
	tags, err := json.Marshal(s.Tags)
	if err != nil {
		return "", sql.NullString{}, fmt.Errorf("encode preset tags: %w", err)
	}

	var waterfall sql.NullString

	if s.Waterfall != nil {
		b, err := json.Marshal(levels{Min: s.Waterfall[0], Max: s.Waterfall[1]})
		if err != nil {
			return "", sql.NullString{}, fmt.Errorf("encode waterfall levels: %w", err)
		}

		waterfall = sql.NullString{String: string(b), Valid: true}
	}

	return string(tags), waterfall, nil
}

// levels is the stored form of the waterfall levels (§7.1 `{min, max}`).
type levels struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

func fromRow(row sqlc.Preset) (*domain.Preset, error) {
	id, err := shared.UUIDFromBytes(row.ID)
	if err != nil {
		return nil, fmt.Errorf("preset id: %w", err)
	}

	var tags []string
	if err := json.Unmarshal([]byte(row.Tags), &tags); err != nil {
		return nil, fmt.Errorf("preset %s tags: %w", id, err)
	}

	s := domain.Snapshot{
		ID: id, Slug: row.Slug, Name: row.Name, Description: row.Description.String, Tags: tags, CenterFreq: row.CenterFreq,
		SampRate: row.SampRate, StartFreq: row.StartFreq, StartMod: row.StartMod, TuningStep: row.TuningStep,
		Squelch: intOf(row.InitialSquelchLevel), NR: intOf(row.InitialNrLevel), SortOrder: int(row.SortOrder),
		CreatedAt: time.UnixMilli(row.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(row.UpdatedAt).UTC(), Version: int(row.Version),
	}

	if row.WaterfallLevels.Valid {
		var l levels
		if err := json.Unmarshal([]byte(row.WaterfallLevels.String), &l); err != nil {
			return nil, fmt.Errorf("preset %s waterfall levels: %w", id, err)
		}

		s.Waterfall = &[2]int{l.Min, l.Max}
	}

	p, err := domain.Rehydrate(s)
	if err != nil {
		return nil, fmt.Errorf("stored preset %s: %w", id, err)
	}

	return p, nil
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullInt(v *int) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

func intOf(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}

	return new(int(v.Int64))
}
