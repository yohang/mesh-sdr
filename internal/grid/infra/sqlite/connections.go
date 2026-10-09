package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ConnectionRepository implements domain.ConnectionRepository.
type ConnectionRepository struct{ db *db.DB }

// NewConnectionRepository returns the repository.
func NewConnectionRepository(a *db.DB) *ConnectionRepository { return &ConnectionRepository{db: a} }

var _ domain.ConnectionRepository = (*ConnectionRepository)(nil)

// Open implements domain.ConnectionRepository.
func (r *ConnectionRepository) Open(ctx context.Context, c *domain.Connection) (bool, error) {
	s := c.Snapshot()
	i := s.Info

	n, err := sqlc.New(r.db.Writer(ctx)).InsertConnection(ctx, sqlc.InsertConnectionParams{
		ID: i.ID.Bytes(), Kind: string(i.Kind), UserID: uuidBytes(i.UserID), SessionID: uuidBytes(i.SessionID),
		RoleID: int64(i.RoleID), Ip: i.IP, UserAgent: nullString(i.UserAgent), NodeID: nullString(i.NodeID),
		DeviceID: nullString(i.DeviceID), PresetID: uuidBytes(s.PresetID), TunedFreq: nullPtr(s.TunedFreq),
		Mode: nullString(i.Mode), SecondaryMode: nullString(s.SecondaryMode), OpenedAt: toMS(s.OpenedAt),
		LastHeartbeatAt: toMS(s.LastHeartbeat), ClosedAt: nullMS(s.ClosedAt), CloseReason: nullString(string(s.CloseReason)),
		BytesOut: s.BytesOut, BytesIn: s.BytesIn, HubIssued: boolInt(i.HubIssued),
	})
	if err != nil {
		return false, fmt.Errorf("open connection %s: %w", i.ID, err)
	}

	return n == 1, nil
}

func nullPtr(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: *v, Valid: true}
}

// Get implements domain.ConnectionRepository.
func (r *ConnectionRepository) Get(ctx context.Context, id shared.UUID) (*domain.Connection, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetConnection(ctx, id.Bytes())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrConnectionNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get connection %s: %w", id, err)
	}

	return connectionFromRow(row)
}

// Save implements domain.ConnectionRepository.
func (r *ConnectionRepository) Save(ctx context.Context, c *domain.Connection) error {
	s := c.Snapshot()

	err := sqlc.New(r.db.Writer(ctx)).UpdateConnection(ctx, sqlc.UpdateConnectionParams{
		DeviceID: nullString(s.Info.DeviceID), PresetID: uuidBytes(s.PresetID), TunedFreq: nullPtr(s.TunedFreq),
		Mode: nullString(s.Info.Mode), SecondaryMode: nullString(s.SecondaryMode), LastHeartbeatAt: toMS(s.LastHeartbeat),
		ClosedAt: nullMS(s.ClosedAt), CloseReason: nullString(string(s.CloseReason)), BytesOut: s.BytesOut, BytesIn: s.BytesIn,
		ID: s.Info.ID.Bytes(),
	})
	if err != nil {
		return fmt.Errorf("save connection %s: %w", s.Info.ID, err)
	}

	return nil
}

// Heartbeat implements domain.ConnectionRepository.
func (r *ConnectionRepository) Heartbeat(ctx context.Context, ids []shared.UUID, now time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	raw := make([][]byte, len(ids))
	for i, id := range ids {
		raw[i] = id.Bytes()
	}

	n, err := sqlc.New(r.db.Writer(ctx)).HeartbeatConnections(ctx, sqlc.HeartbeatConnectionsParams{Now: toMS(now), Ids: raw})
	if err != nil {
		return 0, fmt.Errorf("heartbeat connections: %w", err)
	}

	return n, nil
}

// CloseStale implements domain.ConnectionRepository.
func (r *ConnectionRepository) CloseStale(ctx context.Context, before time.Time) (int64, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).CloseStaleConnections(ctx, toMS(before))
	if err != nil {
		return 0, fmt.Errorf("close stale connections: %w", err)
	}

	return n, nil
}

// CloseAll implements domain.ConnectionRepository.
func (r *ConnectionRepository) CloseAll(ctx context.Context, reason domain.CloseReason, at time.Time) (int64, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).CloseAllConnections(ctx, sqlc.CloseAllConnectionsParams{Now: nullMS(at), Reason: nullString(string(reason))})
	if err != nil {
		return 0, fmt.Errorf("close connections: %w", err)
	}

	return n, nil
}

// CloseNode implements domain.ConnectionRepository.
func (r *ConnectionRepository) CloseNode(ctx context.Context, node domain.NodeID, reason domain.CloseReason, at time.Time) (int64, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).CloseNodeConnections(ctx, sqlc.CloseNodeConnectionsParams{
		Now: nullMS(at), Reason: nullString(string(reason)), NodeID: nullString(node.String()),
	})
	if err != nil {
		return 0, fmt.Errorf("close connections of %s: %w", node, err)
	}

	return n, nil
}

// ListOpen implements domain.ConnectionRepository.
func (r *ConnectionRepository) ListOpen(ctx context.Context) ([]*domain.Connection, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListOpenConnections(ctx)
	if err != nil {
		return nil, fmt.Errorf("list connections: %w", err)
	}

	out := make([]*domain.Connection, 0, len(rows))

	for _, row := range rows {
		c, err := connectionFromRow(row)
		if err != nil {
			return nil, err
		}

		out = append(out, c)
	}

	return out, nil
}

// CountOpenKind implements domain.ConnectionRepository.
func (r *ConnectionRepository) CountOpenKind(ctx context.Context, kind domain.ConnectionKind) (int, error) {
	n, err := sqlc.New(r.db.Reader(ctx)).CountOpenKindConnections(ctx, string(kind))
	if err != nil {
		return 0, fmt.Errorf("count %s connections: %w", kind, err)
	}

	return int(n), nil
}

// CountOpenNode implements domain.ConnectionRepository.
func (r *ConnectionRepository) CountOpenNode(ctx context.Context, node domain.NodeID) (int, error) {
	n, err := sqlc.New(r.db.Reader(ctx)).CountOpenNodeConnections(ctx, nullString(node.String()))
	if err != nil {
		return 0, fmt.Errorf("count connections of %s: %w", node, err)
	}

	return int(n), nil
}

// CountOpenMediaByDevice implements domain.ConnectionRepository.
func (r *ConnectionRepository) CountOpenMediaByDevice(ctx context.Context) (map[string]int, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).CountOpenMediaByDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("count listeners by device: %w", err)
	}

	out := make(map[string]int, len(rows))
	for _, row := range rows {
		if row.DeviceID.Valid {
			out[row.DeviceID.String] = int(row.Listeners)
		}
	}

	return out, nil
}

// OpenNodes implements domain.ConnectionRepository.
func (r *ConnectionRepository) OpenNodes(ctx context.Context) ([]domain.NodeID, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).OpenMediaNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("nodes with connections: %w", err)
	}

	out := make([]domain.NodeID, 0, len(rows))

	for _, r := range rows {
		if id, err := domain.NewNodeID(r.String); err == nil {
			out = append(out, id)
		}
	}

	return out, nil
}

// DeleteClosedBefore implements domain.ConnectionRepository.
func (r *ConnectionRepository) DeleteClosedBefore(ctx context.Context, before time.Time) (int64, error) {
	n, err := sqlc.New(r.db.Writer(ctx)).DeleteClosedConnections(ctx, nullMS(before))
	if err != nil {
		return 0, fmt.Errorf("delete closed connections: %w", err)
	}

	return n, nil
}

// EraseUser removes the personal data of a deleted user (SR-64): its
// closed rows are deleted, its open rows lose the user, session, address
// and user agent (presence keeps counting them until they close).
func (r *ConnectionRepository) EraseUser(ctx context.Context, user shared.UUID) error {
	q := sqlc.New(r.db.Writer(ctx))

	if _, err := q.DeleteClosedUserConnections(ctx, user.Bytes()); err != nil {
		return fmt.Errorf("delete connections of a user: %w", err)
	}

	if _, err := q.AnonymizeOpenUserConnections(ctx, user.Bytes()); err != nil {
		return fmt.Errorf("anonymise connections of a user: %w", err)
	}

	return nil
}

func connectionFromRow(row sqlc.Connection) (*domain.Connection, error) {
	id, err := shared.UUIDFromBytes(row.ID)
	if err != nil {
		return nil, fmt.Errorf("connection id: %w", err)
	}

	user, err := uuidOf(row.UserID)
	if err != nil {
		return nil, err
	}

	session, err := uuidOf(row.SessionID)
	if err != nil {
		return nil, err
	}

	preset, err := uuidOf(row.PresetID)
	if err != nil {
		return nil, err
	}

	var tuned *int64
	if row.TunedFreq.Valid {
		v := row.TunedFreq.Int64
		tuned = &v
	}

	return domain.RehydrateConnection(domain.ConnectionSnapshot{
		Info: domain.ConnectionInfo{
			ID: id, Kind: domain.ConnectionKind(row.Kind), UserID: user, SessionID: session, RoleID: int(row.RoleID),
			IP: row.Ip, UserAgent: row.UserAgent.String, NodeID: row.NodeID.String, DeviceID: row.DeviceID.String, Mode: row.Mode.String,
			HubIssued: row.HubIssued == 1,
		},
		PresetID: preset, TunedFreq: tuned, SecondaryMode: row.SecondaryMode.String, OpenedAt: fromMS(row.OpenedAt),
		LastHeartbeat: fromMS(row.LastHeartbeatAt), ClosedAt: fromNullMS(row.ClosedAt), CloseReason: domain.CloseReason(row.CloseReason.String),
		BytesOut: row.BytesOut, BytesIn: row.BytesIn,
	})
}
