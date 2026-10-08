package wire

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/db"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/schedules"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// scheduling is the presets and schedules modules of the hub
// (ADR 0020), wired to the grid: the schedule guard listens to the device
// registry, the planner feeds the desired state pushed to the nodes.
type scheduling struct {
	presets   *presets.Service
	schedules *schedules.Service
	guard     *schedules.Guard
	planner   *schedules.Planner
	publish   *schedulesPublishJob
}

// settingsReader reads the effective settings (settings/app.Store).
type settingsReader interface {
	String(key string) string
	Int(key string) int
	Bool(key string) bool
	Duration(key string) time.Duration
	Strings(key string) []string
}

// wsjtModes are the modes of decoders.wsjt_decoding_depths.
var wsjtModes = []string{"ft8", "ft4", "jt65", "jt9", "wspr", "fst4", "fst4w", "q65"}

// stateDecoders are the decoding settings of the desired state (Admin ›
// Decoding).
func stateDecoders(s settingsReader) *ctl.StateDecoders {
	d := &ctl.StateDecoders{
		MaxRestarts: s.Int("decoders.max_restarts"), DigimodesFFTSize: s.Int("decoders.digimodes_fft_size"), ShowCW: s.Bool("decoders.cw_showcw"),
		WSJTDepth: s.Int("decoders.wsjt_decoding_depth"), WSJTDepths: map[string]int{},
		Q65Combinations: s.Strings("decoders.q65_enabled_combinations"), JS8Profiles: s.Strings("decoders.js8_enabled_profiles"),
		JS8Depth:     s.Int("decoders.js8_decoding_depth"),
		PagingFilter: s.Bool("decoders.paging_filter"), PagingCharset: s.String("decoders.paging_charset"),
		ISMReportLevels: s.Bool("decoders.ism_report_levels"),
		FAX: &ctl.StateFAX{
			LPM: s.Int("fax_lpm"), MinLength: s.Int("fax_min_length"), MaxLength: s.Int("fax_max_length"),
			PostProcess: s.Bool("fax_postprocess"), Color: s.Bool("fax_color"), AM: s.Bool("fax_am"),
		},
	}

	// Every mode gets its effective depth: its own, else the global one.
	for _, m := range wsjtModes {
		d.WSJTDepths[m] = d.WSJTDepth
		if v := s.Int("decoders.wsjt_decoding_depths." + m); v > 0 {
			d.WSJTDepths[m] = v
		}
	}

	seconds := func(list []string) []int {
		var out []int

		for _, v := range list {
			if n, err := strconv.Atoi(v); err == nil {
				out = append(out, n)
			}
		}

		return out
	}

	d.FST4Intervals = seconds(s.Strings("decoders.fst4_enabled_intervals"))
	d.FST4WIntervals = seconds(s.Strings("decoders.fst4w_enabled_intervals"))

	return d
}

// newScheduling builds the modules. g.states may be nil (grid disabled):
// nothing is pushed then.
func newScheduling(adapter *db.DB, g *hubGrid, values settingsReader, audit audit.Appender, now func() time.Time,
	logger *slog.Logger,
) *scheduling {
	ids := shared.NewUUIDv7Generator()
	devices := scheduleDevices{repo: g.deviceRepo}
	schedRepo := schedules.NewSchedules(adapter)
	presetRepo := presets.NewPresets(adapter)

	changed := func(ctx context.Context) {
		if g.states != nil {
			g.states.PublishAll(ctx)
		}
	}

	s := &scheduling{}

	sdeps := schedules.Deps{
		Repo: schedRepo, Tx: adapter, Devices: devices, Audit: audit, IDs: ids, Now: now,
		Changed: changed, Logger: component(logger, "schedules.app"),
	}

	// Presets and schedules know each other: the catalogue is filled once
	// the preset service exists.
	catalog := &presetCatalog{}
	sdeps.Presets = catalog
	s.schedules = schedules.NewService(sdeps)
	s.guard = schedules.NewGuard(sdeps)
	s.planner = schedules.NewPlanner(sdeps)
	s.presets = presets.NewService(presets.Deps{
		Repo: presetRepo, Tx: adapter, Audit: audit, IDs: ids, Now: now,
		Usage: s.schedules, Listener: s.guard, Changed: changed, Logger: component(logger, "presets.app"),
	})
	catalog.presets = s.presets

	s.publish = &schedulesPublishJob{guard: s.guard, grid: g}

	// Grid hooks (GRID-016, ADM-009): the guard joins the registry's
	// transactions; the desired state reads the planner.
	listener := deviceListener{s.guard}
	g.devices.SetListener(listener, adapter)
	g.nodes.OnDelete(func(ctx context.Context, id griddomain.NodeID) error {
		list, err := g.deviceRepo.ListByNode(ctx, id)
		if err != nil {
			return err
		}

		ids := make([]shared.DeviceID, len(list))
		for i, d := range list {
			ids[i] = d.ID()
		}

		return listener.DevicesRemoved(ctx, ids)
	})

	if g.desired != nil {
		g.desired.source = desiredStates{planner: s.planner, presets: s.presets, settings: values, now: now}
	}

	return s
}

// schedulesPublishJob is the schedules.publish job (ADR 0020 Q7): the
// guard's safety net, then the desired state of every connected node
// (the timeline slides hourly).
type schedulesPublishJob struct {
	guard *schedules.Guard
	grid  *hubGrid
}

// JobSchedulesPublish is the name of the job; it runs hourly.
const (
	JobSchedulesPublish   = "schedules.publish"
	SchedulesPublishEvery = time.Hour
)

func (j *schedulesPublishJob) Name() string { return JobSchedulesPublish }

func (j *schedulesPublishJob) Run(ctx context.Context) (int64, error) {
	n, err := j.guard.Reconcile(ctx)
	if err != nil {
		return n, err
	}

	if j.grid.states != nil {
		n += int64(j.grid.states.PublishAll(ctx))
	}

	return n, nil
}

// scheduleDevices adapts the grid device registry to schedules/app.Devices.
type scheduleDevices struct{ repo griddomain.DeviceRepository }

func scheduleDevice(d *griddomain.Device) schedules.Device {
	lo, hi := d.FreqRange()
	_, stale := d.Missing()

	return schedules.Device{
		ID: d.ID().String(), Node: d.Node().String(), Name: d.Name(), FreqMin: lo, FreqMax: hi, SampleRates: d.SampleRates(),
		SchedulerEnabled: d.Flags().SchedulerEnabled, Stale: stale, ActivePreset: d.ActivePreset(),
	}
}

func (a scheduleDevices) Device(ctx context.Context, id string) (schedules.Device, bool, error) {
	did, err := shared.NewDeviceID(id)
	if err != nil {
		return schedules.Device{}, false, nil //nolint:nilerr // an invalid id is not in the registry
	}

	d, err := a.repo.Get(ctx, did)

	switch {
	case errors.Is(err, griddomain.ErrDeviceNotFound):
		return schedules.Device{}, false, nil
	case err != nil:
		return schedules.Device{}, false, err
	}

	return scheduleDevice(d), true, nil
}

func (a scheduleDevices) NodeDevices(ctx context.Context, node string) ([]schedules.Device, error) {
	id, err := griddomain.NewNodeID(node)
	if err != nil {
		return nil, err
	}

	list, err := a.repo.ListByNode(ctx, id)
	if err != nil {
		return nil, err
	}

	out := make([]schedules.Device, len(list))
	for i, d := range list {
		out[i] = scheduleDevice(d)
	}

	return out, nil
}

func (a scheduleDevices) All(ctx context.Context) ([]schedules.Device, error) {
	list, err := a.repo.List(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]schedules.Device, len(list))
	for i, d := range list {
		out[i] = scheduleDevice(d)
	}

	return out, nil
}

func limitsOf(d schedules.Device) presets.DeviceLimits {
	return presets.DeviceLimits{FreqMin: d.FreqMin, FreqMax: d.FreqMax, SampleRates: d.SampleRates}
}

// presetCatalog adapts the preset service to schedules/app.Presets.
type presetCatalog struct{ presets *presets.Service }

func (c *presetCatalog) Fit(ctx context.Context, id shared.UUID, d schedules.Device) (schedules.Fit, error) {
	err := c.presets.Check(ctx, id, limitsOf(d))

	var de *shared.Error

	switch {
	case err == nil:
		return schedules.Fit{Exists: true}, nil
	case errors.Is(err, presets.ErrPresetNotFound):
		return schedules.Fit{}, nil
	case errors.Is(err, presets.ErrPresetIncompatible) && errors.As(err, &de):
		return schedules.Fit{Exists: true, Reason: de.Message()}, nil
	}

	return schedules.Fit{}, err
}

func (c *presetCatalog) Compatible(ctx context.Context, d schedules.Device) ([]shared.UUID, error) {
	list, err := c.presets.Compatible(ctx, limitsOf(d))
	if err != nil {
		return nil, err
	}

	out := make([]shared.UUID, len(list))
	for i, p := range list {
		out[i] = p.ID()
	}

	return out, nil
}

// deviceListener adapts the schedule guard to grid/app.DeviceListener
// (DevicesStale and DevicesRemoved are the guard's).
type deviceListener struct{ *schedules.Guard }

func (l deviceListener) DeviceReported(ctx context.Context, d *griddomain.Device) error {
	return l.Guard.DeviceReported(ctx, scheduleDevice(d))
}

// lazyDesired is the grid's desired-state source, filled in once the
// scheduling modules exist (they are wired after the grid).
type lazyDesired struct{ source gridapp.DesiredStates }

func (l *lazyDesired) Desired(ctx context.Context, node griddomain.NodeID) (ctl.StateApply, error) {
	if l.source == nil {
		return ctl.StateApply{Presets: map[string]ctl.Preset{}, Devices: map[string]ctl.DesiredDevice{}}, nil
	}

	return l.source.Desired(ctx, node)
}

// desiredStates builds ctl.state.apply from the planner (§4.4).
type desiredStates struct {
	planner  *schedules.Planner
	presets  *presets.Service
	settings settingsReader
	now      func() time.Time
}

func (s desiredStates) Desired(ctx context.Context, node griddomain.NodeID) (ctl.StateApply, error) {
	plans, err := s.planner.Plan(ctx, node.String(), s.now())
	if err != nil {
		return ctl.StateApply{}, err
	}

	all, err := s.presets.List(ctx)
	if err != nil {
		return ctl.StateApply{}, err
	}

	byID := make(map[shared.UUID]*presets.Preset, len(all))
	for _, p := range all {
		byID[p.ID()] = p
	}

	policy := s.settings.String("listen_policy")
	if policy == "" {
		policy = string(identitydomain.ListenRegistered)
	}

	st := ctl.StateApply{
		Presets: map[string]ctl.Preset{}, Devices: map[string]ctl.DesiredDevice{},
		Policy: ctl.StatePolicy{
			ListenPolicy: policy, WFMDeemphasis: s.settings.Int("wfm_deemphasis"),
			Waterfall: &ctl.StateWaterfall{
				MinDB: s.settings.Int("waterfall.min_db"), MaxDB: s.settings.Int("waterfall.max_db"), Palette: s.settings.String("waterfall.palette"),
			},
			Decoders: stateDecoders(s.settings),
		},
	}

	for _, plan := range plans {
		dev := ctl.DesiredDevice{
			Presets: make([]string, 0, len(plan.Presets)),
			Schedule: ctl.Timeline{
				From: plan.Timeline.From().UnixMilli(), Until: plan.Timeline.Until().UnixMilli(), Slots: []ctl.TimelineSlot{},
			},
		}

		for _, id := range plan.Presets {
			p, ok := byID[id]
			if !ok {
				continue
			}

			dev.Presets = append(dev.Presets, id.String())
			st.Presets[id.String()] = ctlPreset(p)
		}

		if !plan.Start.IsZero() {
			dev.ActivePresetID = plan.Start.String()
		}

		for _, sl := range plan.Timeline.Slots() {
			if !slices.Contains(dev.Presets, sl.Preset.String()) {
				continue
			}

			dev.Schedule.Slots = append(dev.Schedule.Slots, ctl.TimelineSlot{
				From: sl.From.UnixMilli(), Until: sl.Until.UnixMilli(), PresetID: sl.Preset.String(),
			})
		}

		st.Devices[plan.Device.ID] = dev
	}

	return st, nil
}

func ctlPreset(p *presets.Preset) ctl.Preset {
	out := ctl.Preset{
		Name: p.Name(), CenterFreq: p.CenterFreq(), SampRate: p.SampRate(), StartFreq: p.StartFreq(),
		StartMod: p.StartMod(), TuningStep: p.TuningStep(),
	}

	if v, ok := p.InitialSquelchLevel(); ok {
		out.InitialSquelchLevel = &v
	}

	if v, ok := p.InitialNRLevel(); ok {
		out.InitialNRLevel = &v
	}

	if w, ok := p.WaterfallLevels(); ok {
		lo, hi := w.Min(), w.Max()
		out.WaterfallMin, out.WaterfallMax = &lo, &hi
	}

	return out
}

// deviceSchedules adapts the schedules to the device page (grid/http).
type deviceSchedules struct {
	schedules *schedules.Service
	presets   *presets.Service
}

func (a deviceSchedules) ForDevice(ctx context.Context, device string) ([]gridhttp.ScheduleRow, error) {
	list, err := a.schedules.ForDevice(ctx, device)
	if err != nil {
		return nil, err
	}

	out := make([]gridhttp.ScheduleRow, 0, len(list))

	for _, s := range list {
		row := gridhttp.ScheduleRow{
			ID: s.ID().String(), Preset: s.Preset().String(), Window: s.Window().String(), Days: s.Days().String(),
			Priority: s.Priority().Int(), Enabled: s.Enabled(),
		}

		if p, err := a.presets.Get(ctx, s.Preset().String()); err == nil {
			row.Preset = p.Name()
		}

		if reason, at := s.DisabledReason(); reason != schedules.ReasonNone {
			row.DisabledReason, row.DisabledAt = string(reason), at
		}

		out = append(out, row)
	}

	return out, nil
}

// presetName names a preset for the schedules page ("" when unknown).
func (s *scheduling) presetName(ctx context.Context, id shared.UUID) string {
	p, err := s.presets.Get(ctx, id.String())
	if err != nil {
		return ""
	}

	return p.Name()
}

// presetBand describes a preset for Admin › Connections.
func (s *scheduling) presetBand(ctx context.Context, id shared.UUID) (gridhttp.PresetBand, bool) {
	p, err := s.presets.Get(ctx, id.String())
	if err != nil {
		return gridhttp.PresetBand{}, false
	}

	return gridhttp.PresetBand{Name: p.Name(), CenterFreq: p.CenterFreq(), SampRate: p.SampRate()}, true
}

// NeedingAttention counts the schedules the hub disabled (Admin › Overview).
func (a deviceSchedules) NeedingAttention(ctx context.Context) (int, error) {
	list, err := a.schedules.List(ctx)
	if err != nil {
		return 0, err
	}

	n := 0

	for _, s := range list {
		if reason, _ := s.DisabledReason(); reason != schedules.ReasonNone {
			n++
		}
	}

	return n, nil
}
