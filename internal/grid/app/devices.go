package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// DeviceListener is told about registry changes, inside their transaction
// (GRID-016: the schedules of stale, removed or changed devices).
type DeviceListener interface {
	// DeviceReported runs for every device of a capability report.
	DeviceReported(ctx context.Context, d *domain.Device) error
	// DevicesStale runs for the devices a report no longer lists.
	DevicesStale(ctx context.Context, ids []shared.DeviceID) error
	// DevicesRemoved runs for devices deleted from the registry.
	DevicesRemoved(ctx context.Context, ids []shared.DeviceID) error
}

// Devices mirrors node device definitions and states into the read-only
// device registry (§7.1 devices, GRID-016).
type Devices struct {
	repo      domain.DeviceRepository
	audit     auditor
	logger    *slog.Logger
	listener  DeviceListener
	tx        Transactor
	forgotten []func(ctx context.Context, d *domain.Device)
}

// OnForget registers a callback run after a device was forgotten
// (composition time only).
func (s *Devices) OnForget(f func(ctx context.Context, d *domain.Device)) {
	s.forgotten = append(s.forgotten, f)
}

// NewDevices returns the service.
func NewDevices(repo domain.DeviceRepository, auditLog audit.Appender, logger *slog.Logger) *Devices {
	return &Devices{repo: repo, audit: auditor{auditLog, logger}, logger: logger}
}

// SetListener registers the registry listener and the transactor that
// makes a forget and its consequences atomic (composition time only).
func (s *Devices) SetListener(l DeviceListener, tx Transactor) { s.listener, s.tx = l, tx }

// SpecOf converts a reported device.
func SpecOf(d ctl.Device) (domain.DeviceSpec, error) {
	id, err := shared.NewDeviceID(d.ID)
	if err != nil {
		return domain.DeviceSpec{}, err
	}

	spec := domain.DeviceSpec{
		ID: id, Name: d.Name, Type: d.Type, Enabled: d.Enabled, FreqMin: d.FreqMin, FreqMax: d.FreqMax,
		SampleRates: d.SampleRates, ListenPolicy: d.ListenPolicy, OperatorCanRetune: d.OperatorCanRetune,
		AlwaysOn: d.AlwaysOn, SchedulerEnabled: d.SchedulerEnabled,
	}

	if c := d.Config; c != nil {
		spec.Config = &domain.DeviceConfig{
			RFGain: c.RFGain, PPM: c.PPM, BiasTee: c.BiasTee, DirectSampling: c.DirectSampling, IQSwap: c.IQSwap, LFOOffset: c.LFOOffset,
		}
	}

	if g := d.GPS; g != nil {
		p, err := domain.NewPosition(g.Lat, g.Lon, g.Source == ctl.PositionDevice)
		if err != nil {
			return domain.DeviceSpec{}, err
		}

		spec.Position = &p
	}

	return spec, nil
}

// Sync applies the devices[] of a capability report (a ReportHandler):
// reported devices are upserted in report order, conflicting ids are
// refused (device_id_conflict), devices no longer reported become
// unavailable.
func (s *Devices) Sync(ctx context.Context, n *domain.Node, caps ctl.Capabilities, now time.Time) error {
	reported := map[shared.DeviceID]bool{}

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

		if s.listener != nil {
			if err := s.listener.DeviceReported(ctx, d); err != nil {
				return err
			}
		}
	}

	known, err := s.repo.ListByNode(ctx, n.ID())
	if err != nil {
		return err
	}

	var stale []shared.DeviceID

	for _, d := range known {
		if !reported[d.ID()] && d.MarkUnavailable(now) {
			if err := s.repo.Save(ctx, d); err != nil {
				return err
			}

			stale = append(stale, d.ID())
			s.logger.InfoContext(ctx, "device no longer reported by its node", slog.String("device_id", d.ID().String()),
				slog.String("node_id", n.ID().String()))
		}
	}

	if s.listener != nil && len(stale) > 0 {
		return s.listener.DevicesStale(ctx, stale)
	}

	return nil
}

func (s *Devices) reject(ctx context.Context, n *domain.Node, id string, err error) {
	s.logger.WarnContext(ctx, "device report refused", slog.String("node_id", n.ID().String()),
		slog.String("device_id", id), slog.Any("error", err))

	if errors.Is(err, domain.ErrDeviceIDConflict) {
		s.audit.Record(ctx, audit.Record{Actor: audit.System, Action: "device.register", TargetType: "device", TargetID: id, Result: audit.ResultDenied,
			After: map[string]string{"node_id": n.ID().String(), "reason": err.Error()}})
	}
}

// StateHandler applies device.state events (control ingestion).
func (s *Devices) StateHandler() EventHandler {
	return On(s.logger, func(ctx context.Context, n *domain.Node, st ctl.DeviceState, now time.Time) error {
		id, err := shared.NewDeviceID(st.DeviceID)
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
	})
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

// Forget deletes a device its node no longer reports (ADM-009) and audits
// it. A device still reported, or whose node is only offline, cannot be
// forgotten (domain.ErrDeviceStillReported): it is removed from the node
// config first. Presets are device-independent and not affected; the
// schedules of the device are disabled (listener).
func (s *Devices) Forget(ctx context.Context, actor audit.Actor, id string) error {
	d, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	since, missing := d.Missing()
	if !missing {
		return domain.ErrDeviceStillReported
	}

	forget := func(ctx context.Context) error {
		deleted, err := s.repo.DeleteMissing(ctx, d.ID())
		if err != nil {
			return err
		}

		if !deleted {
			return domain.ErrDeviceStillReported
		}

		if s.listener != nil {
			return s.listener.DevicesRemoved(ctx, []shared.DeviceID{d.ID()})
		}

		return nil
	}

	if s.tx != nil {
		err = s.tx.WithinTx(ctx, forget)
	} else {
		err = forget(ctx)
	}

	if err != nil {
		return err
	}

	s.audit.Record(ctx, audit.Record{Actor: actor, Action: "device.forget", TargetType: "device", TargetID: d.ID().String(), Result: audit.ResultOK,
		After: map[string]string{"node_id": d.Node().String(), "missing_since": since.UTC().Format(time.RFC3339)}})

	for _, f := range s.forgotten {
		f(ctx, d)
	}

	return nil
}

// ListByNode returns the devices of a node.
func (s *Devices) ListByNode(ctx context.Context, id domain.NodeID) ([]*domain.Device, error) {
	return s.repo.ListByNode(ctx, id)
}

// List returns the registry.
func (s *Devices) List(ctx context.Context) ([]*domain.Device, error) { return s.repo.List(ctx) }

// Get returns one device.
func (s *Devices) Get(ctx context.Context, id string) (*domain.Device, error) {
	did, err := shared.NewDeviceID(id)
	if err != nil {
		return nil, domain.ErrDeviceNotFound
	}

	return s.repo.Get(ctx, did)
}

// OwnedBy reports whether device is a device of node in the registry.
func (s *Devices) OwnedBy(ctx context.Context, node domain.NodeID, device shared.DeviceID) (bool, error) {
	d, err := s.repo.Get(ctx, device)

	switch {
	case errors.Is(err, domain.ErrDeviceNotFound):
		return false, nil
	case err != nil:
		return false, err
	}

	return d.Node() == node, nil
}
