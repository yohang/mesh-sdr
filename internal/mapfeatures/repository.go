package mapfeatures

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
)

// Repository stores the features (map_features).
type Repository struct{ db *db.DB }

// NewRepository returns the repository.
func NewRepository(d *db.DB) *Repository { return &Repository{db: d} }

// Get returns the feature of key (ok false: none).
func (r *Repository) Get(ctx context.Context, key string) (Feature, bool, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetMapFeature(ctx, key)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Feature{}, false, nil
	case err != nil:
		return Feature{}, false, fmt.Errorf("get map feature %s: %w", key, err)
	}

	f, err := featureOf(row)

	return f, err == nil, err
}

// Upsert stores a feature, replacing the one of the same key.
func (r *Repository) Upsert(ctx context.Context, f Feature) error {
	geometry, err := encodeJSON(f.Geometry)
	if err != nil {
		return fmt.Errorf("encode geometry of %s: %w", f.Key, err)
	}

	details, err := encodeJSON(f.Details)
	if err != nil {
		return fmt.Errorf("encode details of %s: %w", f.Key, err)
	}

	p := sqlc.UpsertMapFeatureParams{
		FeatureKey: f.Key, Kind: string(f.Kind), Subject: f.Subject, Source: f.Source, DeviceID: nullString(f.DeviceID),
		Lat: nullFloat(f.Lat), Lon: nullFloat(f.Lon), Geometry: sql.NullString{String: geometry, Valid: true}, Details: details,
		UpdatedAt: f.UpdatedAt.UnixMilli(),
	}

	if !f.ExpiresAt.IsZero() {
		p.ExpiresAt = sql.NullInt64{Int64: f.ExpiresAt.UnixMilli(), Valid: true}
	}

	if err := sqlc.New(r.db.Writer(ctx)).UpsertMapFeature(ctx, p); err != nil {
		return fmt.Errorf("upsert map feature %s: %w", f.Key, err)
	}

	return nil
}

// Delete deletes the feature of key and reports whether it existed.
func (r *Repository) Delete(ctx context.Context, key string) (bool, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteMapFeature(ctx, key)
	if err != nil {
		return false, fmt.Errorf("delete map feature %s: %w", key, err)
	}

	return n > 0, nil
}

// List returns the newest limit features of devices not expired at now,
// newest first.
func (r *Repository) List(ctx context.Context, devices []string, now time.Time, limit int) ([]Feature, error) {
	if len(devices) == 0 || limit <= 0 {
		return []Feature{}, nil
	}

	list, err := json.Marshal(devices)
	if err != nil {
		return nil, err
	}

	rows, err := sqlc.New(r.db.Reader(ctx)).ListMapFeatures(ctx, sqlc.ListMapFeaturesParams{
		DevicesJson: string(list), NowMs: sql.NullInt64{Int64: now.UnixMilli(), Valid: true}, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list map features: %w", err)
	}

	return featuresOf(rows)
}

// OfKind returns the features of a kind of a device, newest first.
func (r *Repository) OfKind(ctx context.Context, device string, kind Kind) ([]Feature, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListDeviceMapFeaturesOfKind(ctx, sqlc.ListDeviceMapFeaturesOfKindParams{
		DeviceID: nullString(device), Kind: string(kind),
	})
	if err != nil {
		return nil, fmt.Errorf("list map features of kind %s of %s: %w", kind, device, err)
	}

	return featuresOf(rows)
}

// DeleteExpired deletes up to limit features expired at now and returns
// them as removals.
func (r *Repository) DeleteExpired(ctx context.Context, now time.Time, limit int) ([]Removal, error) {
	rows, err := sqlc.New(r.db.Writer(ctx)).DeleteExpiredMapFeatures(ctx, sqlc.DeleteExpiredMapFeaturesParams{
		NowMs: sql.NullInt64{Int64: now.UnixMilli(), Valid: true}, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("delete expired map features: %w", err)
	}

	out := make([]Removal, 0, len(rows))
	for _, row := range rows {
		out = append(out, Removal{Key: row.FeatureKey, Reason: ReasonExpired, DeviceID: row.DeviceID.String})
	}

	return out, nil
}

func featuresOf(rows []sqlc.MapFeature) ([]Feature, error) {
	out := make([]Feature, 0, len(rows))

	for _, row := range rows {
		f, err := featureOf(row)
		if err != nil {
			return nil, err
		}

		out = append(out, f)
	}

	return out, nil
}

func featureOf(row sqlc.MapFeature) (Feature, error) {
	f := Feature{
		Key: row.FeatureKey, Kind: Kind(row.Kind), Subject: row.Subject, Source: row.Source, DeviceID: row.DeviceID.String,
		UpdatedAt: time.UnixMilli(row.UpdatedAt).UTC(),
	}

	if row.Lat.Valid && row.Lon.Valid {
		lat, lon := row.Lat.Float64, row.Lon.Float64
		f.Lat, f.Lon = &lat, &lon
	}

	if row.ExpiresAt.Valid {
		f.ExpiresAt = time.UnixMilli(row.ExpiresAt.Int64).UTC()
	}

	if row.Geometry.Valid {
		if err := json.Unmarshal([]byte(row.Geometry.String), &f.Geometry); err != nil {
			return Feature{}, fmt.Errorf("geometry of map feature %s: %w", row.FeatureKey, err)
		}
	}

	if err := json.Unmarshal([]byte(row.Details), &f.Details); err != nil {
		return Feature{}, fmt.Errorf("details of map feature %s: %w", row.FeatureKey, err)
	}

	return f, nil
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullFloat(f *float64) sql.NullFloat64 {
	if f == nil {
		return sql.NullFloat64{}
	}

	return sql.NullFloat64{Float64: *f, Valid: true}
}
