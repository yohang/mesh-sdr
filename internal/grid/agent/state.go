package agent

import (
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

// Codes of the refusals of a device's desired state (ctl.state.applied).
const (
	CodeUnknownDevice      = "unknown_device"
	CodePresetIncompatible = "preset_incompatible"
	CodeInvalidState       = "invalid_state"
)

// DefaultWFMDeemphasis is the WFM de-emphasis (µs) when the hub sends none.
const DefaultWFMDeemphasis = 50

// DesiredState holds the desired state pushed by the hub (§8.1 rule 3,
// ADR 0020), in RAM only. It re-checks each device's presets against the
// node config before accepting it; a refused device keeps its previous
// state (§4.9). Executing the presets and the timeline is the device
// manager's job (SVC-001, SVC-010).
type DesiredState struct {
	config map[string]ctl.Device

	mu       sync.Mutex
	revision int64
	presets  map[string]ctl.Preset
	devices  map[string]ctl.DesiredDevice
	policy   ctl.StatePolicy
}

// NewDesiredState returns an empty desired state for the devices of the
// node config.
func NewDesiredState(config []ctl.Device) *DesiredState {
	m := make(map[string]ctl.Device, len(config))
	for _, d := range config {
		m[d.ID] = d
	}

	return &DesiredState{config: m, presets: map[string]ctl.Preset{}, devices: map[string]ctl.DesiredDevice{}}
}

// Revision returns the revision of the last applied state (0: none since
// boot), announced in ctl.welcome.
func (s *DesiredState) Revision() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.revision
}

// Device returns the desired state of a device.
func (s *DesiredState) Device(id string) (ctl.DesiredDevice, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]

	return d, ok
}

// Preset returns the data of a preset of the desired state.
func (s *DesiredState) Preset(id string) (ctl.Preset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.presets[id]

	return p, ok
}

// Policy returns the global policy of the desired state.
func (s *DesiredState) Policy() ctl.StatePolicy {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.policy
}

// ListenPolicy returns the effective listen policy of a device (SRC-023):
// its node config override, else the global policy of the last accepted
// desired state. Without either (no state received yet, unknown device) it
// fails closed to registered.
func (s *DesiredState) ListenPolicy(device string) string {
	if cfg, ok := s.config[device]; ok && (cfg.ListenPolicy == "anonymous" || cfg.ListenPolicy == "registered") {
		return cfg.ListenPolicy
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.config[device]; ok && s.policy.ListenPolicy == "anonymous" {
		return "anonymous"
	}

	return "registered"
}

// Apply checks and installs a desired state and returns the answer to the
// hub.
func (s *DesiredState) Apply(st ctl.StateApply) ctl.StateApplied {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := ctl.StateApplied{Revision: st.Revision, Errors: []ctl.StateError{}}
	devices := map[string]ctl.DesiredDevice{}

	for _, id := range slices.Sorted(maps.Keys(st.Devices)) {
		want := st.Devices[id]

		cfg, ok := s.config[id]
		if !ok {
			out.Errors = append(out.Errors, ctl.StateError{DeviceID: id, Code: CodeUnknownDevice, Reason: "not in the node config"})

			continue
		}

		if code, reason := check(cfg, want, st.Presets); code != "" {
			out.Errors = append(out.Errors, ctl.StateError{DeviceID: id, Code: code, Reason: reason})

			if prev, ok := s.devices[id]; ok {
				devices[id] = prev
			}

			continue
		}

		devices[id] = want
	}

	// Presets of the kept (refused) devices stay available with the new
	// ones.
	presets := maps.Clone(st.Presets)
	if presets == nil {
		presets = map[string]ctl.Preset{}
	}

	for _, d := range devices {
		for _, p := range d.Presets {
			if _, ok := presets[p]; !ok {
				presets[p] = s.presets[p]
			}
		}
	}

	// Each policy field is checked on its own; a refused one keeps its
	// previous value.
	switch st.Policy.ListenPolicy {
	case "anonymous", "registered":
		s.policy.ListenPolicy = st.Policy.ListenPolicy
	default:
		out.Errors = append(out.Errors, ctl.StateError{Code: CodeInvalidState, Reason: "invalid listen_policy"})
	}

	// The waterfall defaults: none from a hub that predates them (the node
	// defaults apply).
	if reason := checkWaterfall(st.Policy.Waterfall); reason != "" {
		out.Errors = append(out.Errors, ctl.StateError{Code: CodeInvalidState, Reason: reason})
	} else {
		s.policy.Waterfall = st.Policy.Waterfall
	}

	// A hub that predates wfm_deemphasis sends none: the default applies.
	switch st.Policy.WFMDeemphasis {
	case 0:
		s.policy.WFMDeemphasis = DefaultWFMDeemphasis
	case 50, 75:
		s.policy.WFMDeemphasis = st.Policy.WFMDeemphasis
	default:
		out.Errors = append(out.Errors, ctl.StateError{Code: CodeInvalidState, Reason: "invalid wfm_deemphasis"})
	}

	// The decoding settings: none from a hub that predates them.
	if reason := checkDecoders(st.Policy.Decoders); reason != "" {
		out.Errors = append(out.Errors, ctl.StateError{Code: CodeInvalidState, Reason: reason})
	} else {
		s.policy.Decoders = st.Policy.Decoders
	}

	s.presets, s.devices = presets, devices

	// The revision is recorded only when every part was accepted: after a
	// reconnect, ctl.welcome then asks for the state again and the hub
	// learns the refusals again (the node stays degraded).
	if len(out.Errors) == 0 {
		s.revision = st.Revision
	}

	return out
}

// Bounds of the values a node accepts from the hub (the hub validates them
// too, presets/domain): they keep every comparison below free of overflow.
const (
	maxFrequency = 300_000_000_000
	maxRate      = 2147483647
)

// Bounds of the waterfall levels (hub settings waterfall.*).
const (
	minWaterfallDB = -200
	maxWaterfallDB = 50
)

// checkWaterfall validates the waterfall defaults like the hub settings
// do; it returns the reason of a refusal.
func checkWaterfall(w *ctl.StateWaterfall) string {
	switch {
	case w == nil:
		return ""
	case w.MinDB < minWaterfallDB || w.MaxDB > maxWaterfallDB || w.MinDB >= w.MaxDB:
		return "invalid waterfall levels"
	case w.Palette != "default" && w.Palette != "turbo":
		return "invalid waterfall palette"
	}

	return ""
}

// checkDecoders range-checks the decoding settings like the hub does.
func checkDecoders(d *ctl.StateDecoders) string {
	switch {
	case d == nil:
		return ""
	case d.MaxRestarts < 1 || d.MaxRestarts > 100:
		return "invalid decoders.max_restarts"
	case !slices.Contains([]int{0, 512, 1024, 2048, 4096}, d.DigimodesFFTSize):
		return "invalid decoders.digimodes_fft_size"
	case d.FAX == nil:
		return ""
	case d.FAX.LPM < 30 || d.FAX.LPM > 480 || d.FAX.MinLength < 50 || d.FAX.MinLength > 450 || d.FAX.MaxLength < 500 || d.FAX.MaxLength > 8000:
		return "invalid fax settings"
	}

	return ""
}

// checkPreset validates the data of a preset like the hub does.
func checkPreset(p ctl.Preset) bool {
	switch {
	case p.CenterFreq <= 0 || p.CenterFreq > maxFrequency, p.SampRate <= 0 || p.SampRate > maxRate:
		return false
	case p.StartFreq <= 0 || p.StartFreq > maxFrequency, p.TuningStep <= 0 || p.TuningStep > maxRate:
		return false
	}

	d := p.StartFreq - p.CenterFreq
	if d < 0 {
		d = -d
	}

	return 2*d <= p.SampRate
}

// check validates the desired state of one device against its config.
func check(cfg ctl.Device, want ctl.DesiredDevice, presets map[string]ctl.Preset) (string, string) {
	for _, id := range want.Presets {
		p, ok := presets[id]
		if !ok {
			return CodeInvalidState, "preset " + id + " is not described"
		}

		if !checkPreset(p) {
			return CodeInvalidState, "preset " + id + " has invalid values"
		}

		half := p.SampRate / 2
		if p.CenterFreq-half < cfg.FreqMin || p.CenterFreq+half > cfg.FreqMax {
			return CodePresetIncompatible, "preset " + id + " is outside the device frequency range"
		}

		if !slices.Contains(cfg.SampleRates, p.SampRate) {
			return CodePresetIncompatible, "preset " + id + ": sample rate " + strconv.FormatInt(p.SampRate, 10) + " not supported"
		}
	}

	if want.ActivePresetID != "" && !slices.Contains(want.Presets, want.ActivePresetID) {
		return CodeInvalidState, "the active preset is not a preset of the device"
	}

	t := want.Schedule
	if len(t.Slots) > 0 && t.From >= t.Until {
		return CodeInvalidState, "invalid schedule horizon"
	}

	last := t.From

	for _, sl := range t.Slots {
		if sl.From < last || sl.Until <= sl.From || sl.Until > t.Until || !slices.Contains(want.Presets, sl.PresetID) {
			return CodeInvalidState, "invalid schedule timeline"
		}

		last = sl.Until
	}

	return "", ""
}
