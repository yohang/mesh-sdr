package ctl

// StateApply is ctl.state.apply (hub → node, §4.4, ADR 0020): the complete
// desired state of the node's devices, level-triggered (§8.1 rule 3). It
// carries no device setting.
//
// To stay within the 64 KiB control message limit, the data of the presets
// is sent once in Presets and each device lists the ids of those that fit
// it (ADR 0020). Revision identifies the content: the hub derives it from
// the rest of the message, so equal states have equal revisions.
type StateApply struct {
	Revision int64                    `json:"revision"`
	Presets  map[string]Preset        `json:"presets"`
	Devices  map[string]DesiredDevice `json:"devices"`
	Policy   StatePolicy              `json:"policy"`
}

// Preset is the data of a preset a node needs to apply it (§7.1 presets,
// without the display-only fields).
type Preset struct {
	Name                string `json:"name"`
	CenterFreq          int64  `json:"center_freq"`
	SampRate            int64  `json:"samp_rate"`
	StartFreq           int64  `json:"start_freq"`
	StartMod            string `json:"start_mod"`
	TuningStep          int64  `json:"tuning_step"`
	InitialSquelchLevel *int   `json:"initial_squelch_level,omitempty"`
	InitialNRLevel      *int   `json:"initial_nr_level,omitempty"`
	WaterfallMin        *int   `json:"waterfall_min,omitempty"`
	WaterfallMax        *int   `json:"waterfall_max,omitempty"`
}

// DesiredDevice is the desired state of one device.
type DesiredDevice struct {
	// Presets are the ids of the presets that fit the device, by sort
	// order (§4.4: only compatible presets).
	Presets []string `json:"presets"`
	// ActivePresetID is the preset to start the device with (empty: none
	// fits).
	ActivePresetID string   `json:"active_preset_id,omitempty"`
	Schedule       Timeline `json:"schedule"`
}

// Timeline is the schedule timeline of a device (§8.5): the scheduled
// preset per interval [from, until) in Unix milliseconds, over the horizon
// [From, Until). No scheduled preset applies outside the slots.
type Timeline struct {
	From  int64          `json:"from"`
	Until int64          `json:"until"`
	Slots []TimelineSlot `json:"slots"`
}

// TimelineSlot is one interval of a timeline.
type TimelineSlot struct {
	From     int64  `json:"from"`
	Until    int64  `json:"until"`
	PresetID string `json:"preset_id"`
}

// StatePolicy holds the global settings a node enforces.
type StatePolicy struct {
	ListenPolicy string `json:"listen_policy"`
	// WFMDeemphasis is the broadcast FM de-emphasis in µs (50 or 75).
	WFMDeemphasis int `json:"wfm_deemphasis"`
}

// StateApplied answers ctl.state.apply (node → hub, §4.4). It carries no
// seq: it is not an event. Errors name the devices that kept their
// previous desired state (§4.9).
type StateApplied struct {
	Revision int64        `json:"revision"`
	Errors   []StateError `json:"errors"`
}

// StateError is the refusal of the desired state of one device.
type StateError struct {
	DeviceID string `json:"device_id"`
	Code     string `json:"code"`
	Reason   string `json:"reason,omitempty"`
}
