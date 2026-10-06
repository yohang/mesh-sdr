package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Devices mirrors node device definitions and states into the read-only
// device registry (§7.1 devices, GRID-016).
type Devices struct {
	repo   domain.DeviceRepository
	audit  Auditor
	logger *slog.Logger
}

// NewDevices returns the service.
func NewDevices(repo domain.DeviceRepository, audit Auditor, logger *slog.Logger) *Devices {
	return &Devices{repo: repo, audit: audit, logger: logger}
}

// SpecOf converts a reported device.
func SpecOf(d ctl.Device) (domain.DeviceSpec, error) {
	id, err := domain.NewDeviceID(d.ID)
	if err != nil {
		return domain.DeviceSpec{}, err
	}

	return domain.DeviceSpec{
		ID: id, Name: d.Name, Type: d.Type, Enabled: d.Enabled, FreqMin: d.FreqMin, FreqMax: d.FreqMax,
		SampleRates: d.SampleRates, ListenPolicy: d.ListenPolicy, OperatorCanRetune: d.OperatorCanRetune,
		AlwaysOn: d.AlwaysOn, SchedulerEnabled: d.SchedulerEnabled,
	}, nil
}

// Sync applies the devices[] of a capability report (a ReportHandler):
// reported devices are upserted in report order, conflicting ids are
// refused (device_id_conflict), devices no longer reported become
// unavailable.
func (s *Devices) Sync(ctx context.Context, n *domain.Node, caps ctl.Capabilities, now time.Time) error {
	reported := map[domain.DeviceID]bool{}

	for i, rd := range caps.Devices {
		spec, err := SpecOf(rd)
		if err != nil {
			s.reject(ctx, n, rd.ID, err)

			continue
		}

		d, err := s.repo.Get(ctx, spec.ID)

		switch {
		case errors.Is(err, domain.ErrDeviceNotFound):
			d, err = domain.NewReportedDevice(n.ID(), spec, i, now)
		case err != nil:
			return err
		default:
			err = d.ApplySpec(n.ID(), spec, i, now)
		}

		if err != nil {
			s.reject(ctx, n, rd.ID, err)

			continue
		}

		reported[spec.ID] = true

		if err := s.repo.Save(ctx, d); err != nil {
			return err
		}
	}

	known, err := s.repo.ListByNode(ctx, n.ID())
	if err != nil {
		return err
	}

	for _, d := range known {
		if !reported[d.ID()] && d.MarkUnavailable(now) {
			if err := s.repo.Save(ctx, d); err != nil {
				return err
			}

			s.logger.InfoContext(ctx, "device no longer reported by its node", slog.String("device_id", d.ID().String()),
				slog.String("node_id", n.ID().String()))
		}
	}

	return nil
}

func (s *Devices) reject(ctx context.Context, n *domain.Node, id string, err error) {
	s.logger.WarnContext(ctx, "device report refused", slog.String("node_id", n.ID().String()),
		slog.String("device_id", id), slog.Any("error", err))

	if errors.Is(err, domain.ErrDeviceIDConflict) {
		s.audit.Record(ctx, AuditRecord{ActorKind: ActorNode, Action: "device.register", Target: id, Result: ResultDenied,
			Detail: map[string]string{"node_id": n.ID().String(), "reason": err.Error()}})
	}
}

// StateHandler applies device.state events (control ingestion).
func (s *Devices) StateHandler() EventHandler {
	return func(ctx context.Context, n *domain.Node, ev Event, now time.Time) error {
		st, err := decodeEvent[ctl.DeviceState](ev)
		if err != nil {
			return nil //nolint:nilerr // a malformed event is skipped
		}

		id, err := domain.NewDeviceID(st.DeviceID)
		if err != nil {
			return nil //nolint:nilerr // a malformed event is skipped
		}

		d, err := s.repo.Get(ctx, id)
		if errors.Is(err, domain.ErrDeviceNotFound) {
			return nil
		}

		if err != nil {
			return err
		}

		if d.Node() != n.ID() {
			s.logger.WarnContext(ctx, "device.state for another node's device ignored", slog.String("device_id", id.String()))

			return nil
		}

		state, err := domain.ParseRuntimeState(st.State)
		if err != nil {
			s.logger.WarnContext(ctx, "invalid device.state skipped", slog.Any("error", err))

			return nil
		}

		preset, _ := shared.ParseUUID(st.ActivePresetID)
		d.ApplyState(state, st.Reason, st.CenterFreq, preset, now)

		return s.repo.Save(ctx, d)
	}
}

// NodeStatusChanged marks the devices of a node offline when the node is
// not online (a StatusListener).
func (s *Devices) NodeStatusChanged(ctx context.Context, id domain.NodeID, status domain.Status, _ string) {
	if status == domain.StatusOnline || status == domain.StatusDegraded {
		return
	}

	if err := s.repo.SetNodeOffline(ctx, id); err != nil {
		s.logger.ErrorContext(ctx, "mark devices offline", slog.String("node_id", id.String()), slog.Any("error", err))
	}
}

// List returns the registry.
func (s *Devices) List(ctx context.Context) ([]*domain.Device, error) { return s.repo.List(ctx) }

// Get returns one device.
func (s *Devices) Get(ctx context.Context, id string) (*domain.Device, error) {
	did, err := domain.NewDeviceID(id)
	if err != nil {
		return nil, domain.ErrDeviceNotFound
	}

	return s.repo.Get(ctx, did)
}
