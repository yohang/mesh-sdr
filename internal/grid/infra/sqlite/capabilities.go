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
)

// CapabilityRepository implements domain.CapabilityRepository.
type CapabilityRepository struct{ db db.Adapter }

// NewCapabilityRepository returns the repository.
func NewCapabilityRepository(a db.Adapter) *CapabilityRepository { return &CapabilityRepository{db: a} }

var _ domain.CapabilityRepository = (*CapabilityRepository)(nil)

// Replace implements domain.CapabilityRepository. It joins the caller's
// transaction or runs its own.
func (r *CapabilityRepository) Replace(ctx context.Context, rep domain.CapabilityReport) error {
	protocols, err := json.Marshal(rep.Protocols())
	if err != nil {
		return fmt.Errorf("encode protocols: %w", err)
	}

	id := rep.Node().String()

	err = r.db.WithinTx(ctx, func(ctx context.Context) error {
		q := sqlc.New(r.db.Writer(ctx))

		if err := q.DeleteNodeCapabilities(ctx, id); err != nil {
			return err
		}

		for _, c := range rep.Capabilities() {
			err := q.InsertNodeCapability(ctx, sqlc.InsertNodeCapabilityParams{
				NodeID: id, Capability: c.Key(), Available: boolInt(c.Available()), Status: string(c.Status()),
				Version: nullString(c.Version()), Detail: string(c.Detail()), ProbedAt: toMS(rep.ReportedAt()),
				ProbeMs: 0, Error: nullString(c.Error()),
			})
			if err != nil {
				return err
			}
		}

		return q.UpsertCapabilityReport(ctx, sqlc.UpsertCapabilityReportParams{
			NodeID: id, Document: string(rep.Document()), CapabilitiesHash: rep.Hash(), ProductVersion: rep.ProductVersion(),
			Protocols: string(protocols), Platform: string(rep.Platform()), ReportedAt: toMS(rep.ReportedAt()),
		})
	})
	if err != nil {
		return fmt.Errorf("replace capabilities of %s: %w", id, err)
	}

	return nil
}

// Get implements domain.CapabilityRepository.
func (r *CapabilityRepository) Get(ctx context.Context, id domain.NodeID) (domain.CapabilityReport, error) {
	q := sqlc.New(r.db.Reader(ctx))

	row, err := q.GetCapabilityReport(ctx, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CapabilityReport{}, domain.ErrCapabilitiesNotReported
	}

	if err != nil {
		return domain.CapabilityReport{}, fmt.Errorf("capabilities of %s: %w", id, err)
	}

	rows, err := q.ListNodeCapabilities(ctx, id.String())
	if err != nil {
		return domain.CapabilityReport{}, fmt.Errorf("capabilities of %s: %w", id, err)
	}

	caps := make([]domain.Capability, 0, len(rows))

	for _, c := range rows {
		cap, err := domain.NewCapability(c.Capability, c.Available != 0, domain.CapabilityStatus(c.Status), c.Version.String,
			json.RawMessage(c.Detail), c.Error.String)
		if err != nil {
			return domain.CapabilityReport{}, fmt.Errorf("capability %s of %s: %w", c.Capability, id, err)
		}

		caps = append(caps, cap)
	}

	var protocols []string
	if err := json.Unmarshal([]byte(row.Protocols), &protocols); err != nil {
		return domain.CapabilityReport{}, fmt.Errorf("protocols of %s: %w", id, err)
	}

	return domain.NewCapabilityReport(id, row.CapabilitiesHash, row.ProductVersion, protocols,
		json.RawMessage(row.Platform), json.RawMessage(row.Document), fromMS(row.ReportedAt), caps)
}
