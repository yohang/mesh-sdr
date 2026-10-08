package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// deviceCapabilities is the devices.capabilities column: the policy flags
// and the driver values of the node config (SRC-022), absent from the rows
// of nodes that do not report them.
type deviceCapabilities struct {
	domain.DeviceFlags

	Config *domain.DeviceConfig `json:"config,omitempty"`
}

// DeviceRepository implements domain.DeviceRepository.
type DeviceRepository struct{ db *db.DB }

// NewDeviceRepository returns the repository.
func NewDeviceRepository(a *db.DB) *DeviceRepository { return &DeviceRepository{db: a} }

var _ domain.DeviceRepository = (*DeviceRepository)(nil)

// Get implements domain.DeviceRepository.
func (r *DeviceRepository) Get(ctx context.Context, id shared.DeviceID) (*domain.Device, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetDevice(ctx, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrDeviceNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get device %s: %w", id, err)
	}

	return deviceFromRow(row)
}

// List implements domain.DeviceRepository.
func (r *DeviceRepository) List(ctx context.Context) ([]*domain.Device, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}

	return devicesFromRows(rows)
}

// ListByNode implements domain.DeviceRepository.
func (r *DeviceRepository) ListByNode(ctx context.Context, node domain.NodeID) ([]*domain.Device, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListNodeDevices(ctx, node.String())
	if err != nil {
		return nil, fmt.Errorf("list devices of %s: %w", node, err)
	}

	return devicesFromRows(rows)
}

// Save implements domain.DeviceRepository. A row owned by another node is
// never overwritten.
func (r *DeviceRepository) Save(ctx context.Context, d *domain.Device) error {
	s := d.Snapshot()

	rates, err := json.Marshal(s.SampleRates)
	if err != nil {
		return fmt.Errorf("encode sample rates: %w", err)
	}

	flags, err := json.Marshal(deviceCapabilities{DeviceFlags: s.Flags, Config: s.Config})
	if err != nil {
		return fmt.Errorf("encode capabilities: %w", err)
	}

	var center sql.NullInt64
	if s.CenterFreq != nil {
		center = nullInt(*s.CenterFreq, true)
	}

	err = sqlc.New(r.db.Writer(ctx)).UpsertDevice(ctx, sqlc.UpsertDeviceParams{
		ID: s.ID, NodeID: s.Node, Name: s.Name, Type: s.Type, FreqMin: s.FreqMin, FreqMax: s.FreqMax,
		SampleRates: string(rates), Capabilities: string(flags), Online: boolInt(s.Online), RuntimeState: string(s.State),
		RuntimeStateAt: toMS(s.StateAt), RuntimeReason: nullString(s.Reason), ActivePresetID: uuidBytes(s.ActivePreset),
		CenterFreq: center, SortOrder: int64(s.SortOrder), ReportedAt: toMS(s.ReportedAt),
	})
	if err != nil {
		return fmt.Errorf("save device %s: %w", s.ID, err)
	}

	return nil
}

// SetNodeOffline implements domain.DeviceRepository.
func (r *DeviceRepository) SetNodeOffline(ctx context.Context, node domain.NodeID) error {
	if err := sqlc.New(r.db.Writer(ctx)).SetNodeDevicesOffline(ctx, node.String()); err != nil {
		return fmt.Errorf("devices of %s offline: %w", node, err)
	}

	return nil
}

func devicesFromRows(rows []sqlc.Device) ([]*domain.Device, error) {
	out := make([]*domain.Device, 0, len(rows))

	for _, row := range rows {
		d, err := deviceFromRow(row)
		if err != nil {
			return nil, err
		}

		out = append(out, d)
	}

	return out, nil
}

func deviceFromRow(row sqlc.Device) (*domain.Device, error) {
	var rates []int64
	if err := json.Unmarshal([]byte(row.SampleRates), &rates); err != nil {
		return nil, fmt.Errorf("device %s sample rates: %w", row.ID, err)
	}

	var caps deviceCapabilities
	if err := json.Unmarshal([]byte(row.Capabilities), &caps); err != nil {
		return nil, fmt.Errorf("device %s capabilities: %w", row.ID, err)
	}

	preset, err := uuidOf(row.ActivePresetID)
	if err != nil {
		return nil, fmt.Errorf("device %s preset: %w", row.ID, err)
	}

	var center *int64
	if row.CenterFreq.Valid {
		v := row.CenterFreq.Int64
		center = &v
	}

	return domain.RehydrateDevice(domain.DeviceSnapshot{
		ID: row.ID, Node: row.NodeID, Name: row.Name, Type: row.Type, FreqMin: row.FreqMin, FreqMax: row.FreqMax,
		SampleRates: rates, Flags: caps.DeviceFlags, Config: caps.Config, Online: row.Online != 0, State: domain.RuntimeState(row.RuntimeState),
		StateAt: fromMS(row.RuntimeStateAt), Reason: row.RuntimeReason.String, ActivePreset: preset, CenterFreq: center,
		SortOrder: int(row.SortOrder), ReportedAt: fromMS(row.ReportedAt),
	})
}

// DeleteMissing implements domain.DeviceRepository: the condition is in the
// statement, so a device reported again meanwhile is kept.
func (r *DeviceRepository) DeleteMissing(ctx context.Context, id shared.DeviceID) (bool, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteMissingDevice(ctx, id.String())
	if err != nil {
		return false, fmt.Errorf("delete device %s: %w", id, err)
	}

	return n > 0, nil
}
