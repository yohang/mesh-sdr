// Package sqlite implements the schedule repository on the SQLite adapter
// (ADR 0006).
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
	"github.com/yohang/mesh-sdr/internal/schedules/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Schedules implements domain.Repository.
type Schedules struct{ db *db.DB }

// NewSchedules returns the repository.
func NewSchedules(a *db.DB) *Schedules { return &Schedules{db: a} }

var _ domain.Repository = (*Schedules)(nil)

// Get implements domain.Repository.
func (r *Schedules) Get(ctx context.Context, id shared.UUID) (*domain.Schedule, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetSchedule(ctx, id.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrScheduleNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get schedule %s: %w", id, err)
	}

	return fromRow(row)
}

// List implements domain.Repository.
func (r *Schedules) List(ctx context.Context) ([]*domain.Schedule, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListSchedules(ctx)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}

	return fromRows(rows)
}

// ListByDevice implements domain.Repository.
func (r *Schedules) ListByDevice(ctx context.Context, device domain.DeviceID) ([]*domain.Schedule, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListDeviceSchedules(ctx, device.String())
	if err != nil {
		return nil, fmt.Errorf("list schedules of %s: %w", device, err)
	}

	return fromRows(rows)
}

// ListByPreset implements domain.Repository.
func (r *Schedules) ListByPreset(ctx context.Context, preset shared.UUID) ([]*domain.Schedule, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListPresetSchedules(ctx, preset.Bytes())
	if err != nil {
		return nil, fmt.Errorf("list schedules of preset %s: %w", preset, err)
	}

	return fromRows(rows)
}

// Create implements domain.Repository.
func (r *Schedules) Create(ctx context.Context, s *domain.Schedule) error {
	v := s.Snapshot()

	err := sqlc.New(r.db.Writer(ctx)).InsertSchedule(ctx, sqlc.InsertScheduleParams{
		ID: v.ID.Bytes(), DeviceID: v.Device, PresetID: v.Preset.Bytes(), Kind: string(v.Kind), StartMinute: nullInt(v.Start),
		EndMinute: nullInt(v.End), DaysOfWeek: int64(v.Days), DaylightPhase: nullString(string(v.Phase)),
		Priority: int64(v.Priority), Enabled: boolInt(v.Enabled), DisabledReason: nullString(string(v.Reason)),
		DisabledAt: nullMS(v.DisabledAt), CreatedAt: v.CreatedAt.UnixMilli(), UpdatedAt: v.UpdatedAt.UnixMilli(),
		Version: int64(v.Version),
	})
	if err != nil {
		return writeError(err, "insert", v.ID)
	}

	return nil
}

// Update implements domain.Repository.
func (r *Schedules) Update(ctx context.Context, s *domain.Schedule, expectedVersion int) error {
	v := s.Snapshot()

	n, err := sqlc.New(r.db.Writer(ctx)).UpdateSchedule(ctx, sqlc.UpdateScheduleParams{
		DeviceID: v.Device, PresetID: v.Preset.Bytes(), Kind: string(v.Kind), StartMinute: nullInt(v.Start),
		EndMinute: nullInt(v.End), DaysOfWeek: int64(v.Days), DaylightPhase: nullString(string(v.Phase)),
		Priority: int64(v.Priority), Enabled: boolInt(v.Enabled), DisabledReason: nullString(string(v.Reason)),
		DisabledAt: nullMS(v.DisabledAt), UpdatedAt: v.UpdatedAt.UnixMilli(), Version: int64(v.Version),
		ID: v.ID.Bytes(), ExpectedVersion: int64(expectedVersion),
	})
	if err != nil {
		return writeError(err, "update", v.ID)
	}

	if n == 0 {
		if _, err := r.Get(ctx, v.ID); err != nil {
			return err
		}

		return domain.ErrVersionConflict
	}

	return nil
}

// Delete implements domain.Repository.
func (r *Schedules) Delete(ctx context.Context, id shared.UUID) error {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteSchedule(ctx, id.Bytes())
	if err != nil {
		return fmt.Errorf("delete schedule %s: %w", id, err)
	}

	if n == 0 {
		return domain.ErrScheduleNotFound
	}

	return nil
}

func writeError(err error, op string, id shared.UUID) error {
	if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		return domain.ErrUnknownPreset
	}

	return fmt.Errorf("%s schedule %s: %w", op, id, err)
}

func fromRows(rows []sqlc.Schedule) ([]*domain.Schedule, error) {
	out := make([]*domain.Schedule, 0, len(rows))

	for _, row := range rows {
		s, err := fromRow(row)
		if err != nil {
			return nil, err
		}

		out = append(out, s)
	}

	return out, nil
}

func fromRow(row sqlc.Schedule) (*domain.Schedule, error) {
	id, err := shared.UUIDFromBytes(row.ID)
	if err != nil {
		return nil, fmt.Errorf("schedule id: %w", err)
	}

	preset, err := shared.UUIDFromBytes(row.PresetID)
	if err != nil {
		return nil, fmt.Errorf("schedule %s preset: %w", id, err)
	}

	reason, err := domain.ParseDisabledReason(row.DisabledReason.String)
	if err != nil {
		return nil, fmt.Errorf("schedule %s: %w", id, err)
	}

	s, err := domain.Rehydrate(domain.Snapshot{
		ID: id, Device: row.DeviceID, Preset: preset, Kind: domain.Kind(row.Kind), Start: intOf(row.StartMinute),
		End: intOf(row.EndMinute), Phase: domain.Phase(row.DaylightPhase.String), Days: int(row.DaysOfWeek),
		Priority: int(row.Priority), Enabled: row.Enabled != 0, Reason: reason, DisabledAt: fromNullMS(row.DisabledAt),
		CreatedAt: time.UnixMilli(row.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(row.UpdatedAt).UTC(), Version: int(row.Version),
	})
	if err != nil {
		return nil, fmt.Errorf("stored schedule %s: %w", id, err)
	}

	return s, nil
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

func nullMS(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func fromNullMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}

	return time.UnixMilli(v.Int64).UTC()
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}

	return 0
}
