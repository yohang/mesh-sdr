package api

import (
	"context"

	reportingapp "github.com/yohang/mesh-sdr/internal/reporting/app"
	reportingdomain "github.com/yohang/mesh-sdr/internal/reporting/domain"
	schedulesdomain "github.com/yohang/mesh-sdr/internal/schedules/domain"
)

// ScheduleService is the schedule use cases (ADR 0020).
type ScheduleService interface {
	List(ctx context.Context) ([]*schedulesdomain.Schedule, error)
	Get(ctx context.Context, id string) (*schedulesdomain.Schedule, error)
	Create(ctx context.Context, d schedulesdomain.Draft) (*schedulesdomain.Schedule, error)
	Replace(ctx context.Context, id string, expectedVersion int, d schedulesdomain.Draft) (*schedulesdomain.Schedule, error)
	Delete(ctx context.Context, id string) error
}

// DeviceScope tells whether the caller operates a device (operator role
// globally or on that device, or admin): the read scope of schedules.
type DeviceScope interface {
	CanOperate(ctx context.Context, device string) bool
}

// ScheduleHandlers serve /schedules.
type ScheduleHandlers struct {
	schedules ScheduleService
	scope     DeviceScope
}

// NewScheduleHandlers returns the handlers.
func NewScheduleHandlers(schedules ScheduleService, scope DeviceScope) ScheduleHandlers {
	return ScheduleHandlers{schedules: schedules, scope: scope}
}

func scheduleDTO(s *schedulesdomain.Schedule) Schedule {
	out := Schedule{
		Id: apiUUID(s.ID()), DeviceId: s.Device().String(), PresetId: apiUUID(s.Preset()), Kind: ScheduleKind(s.Window().Kind()),
		DaysOfWeek: s.Days().Mask(), Priority: s.Priority().Int(), Enabled: s.Enabled(), CreatedAt: s.CreatedAt(),
		UpdatedAt: s.UpdatedAt(), Version: s.Version(),
	}

	switch w := s.Window(); w.Kind() {
	case schedulesdomain.KindStatic:
		start, end := w.Minutes()
		out.StartMinute, out.EndMinute = &start, &end
	case schedulesdomain.KindDaylight:
		p := ScheduleDaylightPhase(w.Phase())
		out.DaylightPhase = &p
	}

	if reason, at := s.DisabledReason(); reason != schedulesdomain.ReasonNone {
		r := ScheduleDisabledReason(reason)
		out.DisabledReason, out.DisabledAt = &r, &at
	}

	return out
}

func scheduleDraft(device, preset string, kind *string, start, end, days *int, phase *string, priority *int, enabled *bool) schedulesdomain.Draft {
	d := schedulesdomain.Draft{
		DeviceID: device, PresetID: preset, StartMinute: start, EndMinute: end, DaysOfWeek: days, Priority: priority, Enabled: enabled,
	}

	if kind != nil {
		d.Kind = *kind
	}

	if phase != nil {
		d.DaylightPhase = *phase
	}

	return d
}

func strPtr[T ~string](v *T) *string {
	if v == nil {
		return nil
	}

	s := string(*v)

	return &s
}

// ListSchedules implements StrictServerInterface: the schedules of the
// devices the caller operates.
func (h ScheduleHandlers) ListSchedules(ctx context.Context, req ListSchedulesRequestObject) (ListSchedulesResponseObject, error) {
	list, err := h.schedules.List(ctx)
	if err != nil {
		return nil, err
	}

	out := ListSchedules200JSONResponse{Items: []Schedule{}}

	for _, s := range list {
		device := s.Device().String()

		if (req.Params.DeviceId != nil && *req.Params.DeviceId != device) || !h.scope.CanOperate(ctx, device) {
			continue
		}

		out.Items = append(out.Items, scheduleDTO(s))
	}

	return out, nil
}

// CreateSchedule implements StrictServerInterface.
func (h ScheduleHandlers) CreateSchedule(ctx context.Context, req CreateScheduleRequestObject) (CreateScheduleResponseObject, error) {
	b := req.Body

	s, err := h.schedules.Create(ctx, scheduleDraft(b.DeviceId, b.PresetId, strPtr(b.Kind), b.StartMinute, b.EndMinute, b.DaysOfWeek,
		strPtr(b.DaylightPhase), b.Priority, b.Enabled))
	if err != nil {
		return nil, err
	}

	return CreateSchedule201JSONResponse(scheduleDTO(s)), nil
}

// GetSchedule implements StrictServerInterface: 404 for a schedule of a
// device the caller does not operate.
func (h ScheduleHandlers) GetSchedule(ctx context.Context, req GetScheduleRequestObject) (GetScheduleResponseObject, error) {
	s, err := h.schedules.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}

	if !h.scope.CanOperate(ctx, s.Device().String()) {
		return nil, schedulesdomain.ErrScheduleNotFound
	}

	return GetSchedule200JSONResponse(scheduleDTO(s)), nil
}

// ReplaceSchedule implements StrictServerInterface.
func (h ScheduleHandlers) ReplaceSchedule(ctx context.Context, req ReplaceScheduleRequestObject) (ReplaceScheduleResponseObject, error) {
	b := req.Body

	s, err := h.schedules.Replace(ctx, req.Id, b.Version, scheduleDraft(b.DeviceId, b.PresetId, strPtr(b.Kind), b.StartMinute,
		b.EndMinute, b.DaysOfWeek, strPtr(b.DaylightPhase), b.Priority, b.Enabled))
	if err != nil {
		return nil, err
	}

	return ReplaceSchedule200JSONResponse(scheduleDTO(s)), nil
}

// DeleteSchedule implements StrictServerInterface.
func (h ScheduleHandlers) DeleteSchedule(ctx context.Context, req DeleteScheduleRequestObject) (DeleteScheduleResponseObject, error) {
	if err := h.schedules.Delete(ctx, req.Id); err != nil {
		return nil, err
	}

	return DeleteSchedule204Response{}, nil
}

// ReportingStatusReader reads the outbox status (reporting/app.Engine).
type ReportingStatusReader interface {
	Status(ctx context.Context) ([]reportingapp.NetworkStatus, error)
}

// ReportingHandlers serve /reporting.
type ReportingHandlers struct{ status ReportingStatusReader }

// NewReportingHandlers returns the handlers.
func NewReportingHandlers(status ReportingStatusReader) ReportingHandlers {
	return ReportingHandlers{status: status}
}

// GetReportingStatus implements StrictServerInterface.
func (h ReportingHandlers) GetReportingStatus(ctx context.Context, _ GetReportingStatusRequestObject) (GetReportingStatusResponseObject, error) {
	list, err := h.status.Status(ctx)
	if err != nil {
		return nil, err
	}

	out := GetReportingStatus200JSONResponse{Networks: make([]ReportingNetwork, 0, len(list))}

	for _, n := range list {
		out.Networks = append(out.Networks, ReportingNetwork{
			Network: ReportingNetworkNetwork(n.Network), Enabled: n.Enabled,
			Pending: n.Counts[reportingdomain.StatusPending], InFlight: n.Counts[reportingdomain.StatusInFlight],
			Failed: n.Counts[reportingdomain.StatusFailed], Sent: n.Counts[reportingdomain.StatusSent],
			Dead: n.Counts[reportingdomain.StatusDead], LastSentAt: optTime(n.LastSentAt), OldestDueAt: optTime(n.OldestDueAt),
		})
	}

	return out, nil
}
