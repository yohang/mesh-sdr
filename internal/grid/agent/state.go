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

	s.revision, s.presets, s.devices, s.policy = st.Revision, presets, devices, st.Policy

	return out
}

// check validates the desired state of one device against its config.
func check(cfg ctl.Device, want ctl.DesiredDevice, presets map[string]ctl.Preset) (string, string) {
	for _, id := range want.Presets {
		p, ok := presets[id]
		if !ok {
			return CodeInvalidState, "preset " + id + " is not described"
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
	last := t.From

	for _, sl := range t.Slots {
		if sl.From < last || sl.Until <= sl.From || sl.Until > t.Until || !slices.Contains(want.Presets, sl.PresetID) {
			return CodeInvalidState, "invalid schedule timeline"
		}

		last = sl.Until
	}

	return "", ""
}
