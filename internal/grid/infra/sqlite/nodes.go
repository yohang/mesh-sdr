// Package sqlite holds the grid repositories of the SQLite dialect
// (ADR 0006), built on the sqlc queries of internal/db/sqlite.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// NodeRepository implements domain.NodeRepository.
type NodeRepository struct{ db *db.DB }

// NewNodeRepository returns the repository.
func NewNodeRepository(a *db.DB) *NodeRepository { return &NodeRepository{db: a} }

var _ domain.NodeRepository = (*NodeRepository)(nil)

func toMS(t time.Time) int64 { return t.UnixMilli() }

func nullMS(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func fromNullMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}

	return fromMS(v.Int64)
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullInt(v int64, valid bool) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: valid} }

func boolInt(b bool) int64 {
	if b {
		return 1
	}

	return 0
}

func uuidBytes(u shared.UUID) []byte {
	if u.IsZero() {
		return nil
	}

	return u.Bytes()
}

func uuidOf(b []byte) (shared.UUID, error) {
	if b == nil {
		return shared.UUID{}, nil
	}

	return shared.UUIDFromBytes(b)
}

func runtimeParams(n *domain.Node) sqlc.UpdateNodeRuntimeParams {
	r := n.Runtime()

	var off sql.NullInt64
	if r.ClockOffsetMS != nil {
		off = nullInt(*r.ClockOffsetMS, true)
	}

	return sqlc.UpdateNodeRuntimeParams{
		Status:          string(r.Status),
		StatusHint:      nullString(r.StatusHint),
		LastHeartbeatAt: nullMS(r.LastHeartbeatAt),
		BootID:          uuidBytes(r.BootID),
		SoftwareVersion: nullString(r.SoftwareVersion),
		ProtocolVersion: nullString(r.ProtocolVersion),
		Hostname:        nullString(r.Hostname),
		CpuCores:        nullInt(int64(r.CPUCores), r.CPUCores > 0),
		ClockOffsetMs:   off,
		ID:              n.ID().String(),
	}
}

// Create implements domain.NodeRepository.
func (r *NodeRepository) Create(ctx context.Context, n *domain.Node) error {
	s := n.Snapshot()

	locked, err := json.Marshal(s.LockedFields)
	if err != nil {
		return fmt.Errorf("encode locked fields: %w", err)
	}

	rt := runtimeParams(n)

	rows, err := sqlc.New(r.db.Writer(ctx)).CreateNode(ctx, sqlc.CreateNodeParams{
		ID: s.ID, Name: s.Name, Url: s.URL, EnrollmentState: string(s.Enrollment),
		EnrollmentTokenDigest: s.EnrollmentKey, EnrollmentTokenExpiresAt: nullMS(s.KeyExpiresAt),
		EnrolledAt: nullMS(s.EnrolledAt), CertFingerprint: s.CertFingerprint, CertSerial: nullString(s.CertSerial),
		CertNotAfter: nullMS(s.CertNotAfter), Status: rt.Status, StatusHint: rt.StatusHint,
		LastHeartbeatAt: rt.LastHeartbeatAt, BootID: rt.BootID, SoftwareVersion: rt.SoftwareVersion,
		ProtocolVersion: rt.ProtocolVersion, Hostname: rt.Hostname, CpuCores: rt.CpuCores, ClockOffsetMs: rt.ClockOffsetMs,
		Origin: string(s.Origin), LockedFields: string(locked), Disabled: boolInt(s.Disabled),
		CreatedAt: toMS(s.CreatedAt), UpdatedAt: toMS(s.UpdatedAt), Version: int64(s.Version),
		CertPendingFingerprint: s.PendingCert.Fingerprint, CertPendingSerial: nullString(s.PendingCert.Serial),
		CertPendingNotAfter: nullMS(s.PendingCert.NotAfter),
	})
	if err != nil {
		return fmt.Errorf("insert node %s: %w", s.ID, err)
	}

	if rows == 0 {
		return domain.ErrNodeExists
	}

	return nil
}

// Get implements domain.NodeRepository.
func (r *NodeRepository) Get(ctx context.Context, id domain.NodeID) (*domain.Node, error) {
	row, err := sqlc.New(r.db.Reader(ctx)).GetNode(ctx, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNodeNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", id, err)
	}

	return nodeFromRow(row)
}

// List implements domain.NodeRepository.
func (r *NodeRepository) List(ctx context.Context) ([]*domain.Node, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	out := make([]*domain.Node, 0, len(rows))

	for _, row := range rows {
		n, err := nodeFromRow(row)
		if err != nil {
			return nil, err
		}

		out = append(out, n)
	}

	return out, nil
}

// Save implements domain.NodeRepository. It writes the admin-managed
// columns only: the runtime state and the status belong to SaveRuntime and
// SaveStatus, so a copy read outside the transaction never overwrites them.
func (r *NodeRepository) Save(ctx context.Context, n *domain.Node, expectedVersion int) error {
	s := n.Snapshot()

	locked, err := json.Marshal(s.LockedFields)
	if err != nil {
		return fmt.Errorf("encode locked fields: %w", err)
	}

	rows, err := sqlc.New(r.db.Writer(ctx)).UpdateNode(ctx, sqlc.UpdateNodeParams{
		Name: s.Name, Url: s.URL, EnrollmentState: string(s.Enrollment),
		EnrollmentTokenDigest: s.EnrollmentKey, EnrollmentTokenExpiresAt: nullMS(s.KeyExpiresAt),
		EnrolledAt: nullMS(s.EnrolledAt), CertFingerprint: s.CertFingerprint, CertSerial: nullString(s.CertSerial),
		CertNotAfter:           nullMS(s.CertNotAfter),
		CertPendingFingerprint: s.PendingCert.Fingerprint, CertPendingSerial: nullString(s.PendingCert.Serial),
		CertPendingNotAfter: nullMS(s.PendingCert.NotAfter),
		Origin:              string(s.Origin), LockedFields: string(locked), Disabled: boolInt(s.Disabled),
		UpdatedAt: toMS(s.UpdatedAt), Version: int64(s.Version), ID: s.ID, ExpectedVersion: int64(expectedVersion),
	})
	if err != nil {
		return fmt.Errorf("update node %s: %w", s.ID, err)
	}

	if rows == 0 {
		if _, err := r.Get(ctx, n.ID()); err != nil {
			return err
		}

		return domain.ErrVersionConflict
	}

	return nil
}

// SaveRuntime implements domain.NodeRepository.
func (r *NodeRepository) SaveRuntime(ctx context.Context, n *domain.Node) error {
	rows, err := sqlc.New(r.db.Writer(ctx)).UpdateNodeRuntime(ctx, runtimeParams(n))
	if err != nil {
		return fmt.Errorf("update node %s runtime: %w", n.ID(), err)
	}

	if rows == 0 {
		return domain.ErrNodeNotFound
	}

	return nil
}

// SaveStatus implements domain.NodeRepository.
func (r *NodeRepository) SaveStatus(ctx context.Context, id domain.NodeID, status domain.Status, hint string) error {
	rows, err := sqlc.New(r.db.Writer(ctx)).UpdateNodeStatus(ctx, sqlc.UpdateNodeStatusParams{
		Status: string(status), StatusHint: nullString(hint), ID: id.String(),
	})
	if err != nil {
		return fmt.Errorf("update node %s status: %w", id, err)
	}

	if rows == 0 {
		return domain.ErrNodeNotFound
	}

	return nil
}

// Delete implements domain.NodeRepository.
func (r *NodeRepository) Delete(ctx context.Context, id domain.NodeID) error {
	rows, err := sqlc.New(r.db.Writer(ctx)).DeleteNode(ctx, id.String())
	if err != nil {
		return fmt.Errorf("delete node %s: %w", id, err)
	}

	if rows == 0 {
		return domain.ErrNodeNotFound
	}

	return nil
}

func nodeFromRow(row sqlc.Node) (*domain.Node, error) {
	var locked []string
	if err := json.Unmarshal([]byte(row.LockedFields), &locked); err != nil {
		return nil, fmt.Errorf("node %s: decode locked fields: %w", row.ID, err)
	}

	boot, err := uuidOf(row.BootID)
	if err != nil {
		return nil, fmt.Errorf("node %s: boot id: %w", row.ID, err)
	}

	rt := domain.Runtime{
		Status:          domain.Status(row.Status),
		StatusHint:      row.StatusHint.String,
		LastHeartbeatAt: fromNullMS(row.LastHeartbeatAt),
		BootID:          boot,
		SoftwareVersion: row.SoftwareVersion.String,
		ProtocolVersion: row.ProtocolVersion.String,
		Hostname:        row.Hostname.String,
		CPUCores:        int(row.CpuCores.Int64),
	}

	if row.ClockOffsetMs.Valid {
		v := row.ClockOffsetMs.Int64
		rt.ClockOffsetMS = &v
	}

	n, err := domain.RehydrateNode(domain.NodeSnapshot{
		ID: row.ID, Name: row.Name, URL: row.Url, Origin: domain.Origin(row.Origin), LockedFields: locked,
		Disabled: row.Disabled != 0, Enrollment: domain.EnrollmentState(row.EnrollmentState),
		EnrollmentKey: row.EnrollmentTokenDigest, KeyExpiresAt: fromNullMS(row.EnrollmentTokenExpiresAt),
		EnrolledAt: fromNullMS(row.EnrolledAt), CertFingerprint: row.CertFingerprint, CertSerial: row.CertSerial.String,
		CertNotAfter: fromNullMS(row.CertNotAfter), Runtime: rt,
		PendingCert: domain.CertSnapshot{
			Fingerprint: row.CertPendingFingerprint, Serial: row.CertPendingSerial.String, NotAfter: fromNullMS(row.CertPendingNotAfter),
		},
		CreatedAt: fromMS(row.CreatedAt), UpdatedAt: fromMS(row.UpdatedAt), Version: int(row.Version),
	})
	if err != nil {
		return nil, fmt.Errorf("node %s: rehydrate: %w", row.ID, err)
	}

	return n, nil
}

// RevocationRepository implements domain.RevocationRepository.
type RevocationRepository struct{ db *db.DB }

// NewRevocationRepository returns the repository.
func NewRevocationRepository(a *db.DB) *RevocationRepository { return &RevocationRepository{db: a} }

var _ domain.RevocationRepository = (*RevocationRepository)(nil)

// Add implements domain.RevocationRepository.
func (r *RevocationRepository) Add(ctx context.Context, c domain.RevokedCertificate) error {
	err := sqlc.New(r.db.Writer(ctx)).AddRevokedCertificate(ctx, sqlc.AddRevokedCertificateParams{
		Serial: c.Serial(), NodeID: c.NodeID().String(), NotAfter: toMS(c.NotAfter()),
		RevokedAt: toMS(c.RevokedAt()), Reason: c.Reason(),
	})
	if err != nil {
		return fmt.Errorf("revoke certificate %s: %w", c.Serial(), err)
	}

	return nil
}

// List implements domain.RevocationRepository.
func (r *RevocationRepository) List(ctx context.Context, now time.Time) ([]domain.RevokedCertificate, error) {
	rows, err := sqlc.New(r.db.Reader(ctx)).ListRevokedCertificates(ctx, toMS(now))
	if err != nil {
		return nil, fmt.Errorf("list revoked certificates: %w", err)
	}

	out := make([]domain.RevokedCertificate, 0, len(rows))

	for _, row := range rows {
		c, err := domain.RehydrateRevokedCertificate(row.Serial, row.NodeID, fromMS(row.NotAfter), fromMS(row.RevokedAt), row.Reason)
		if err != nil {
			return nil, fmt.Errorf("revoked certificate %s: %w", row.Serial, err)
		}

		out = append(out, c)
	}

	return out, nil
}
