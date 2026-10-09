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
	// Waterfall are the waterfall defaults of device.config (hub settings
	// waterfall.*, ADR 0026); nil: the node defaults.
	Waterfall *StateWaterfall `json:"waterfall,omitempty"`
	// Decoders are the decoding settings (hub settings decoders.*, Admin ›
	// Decoding); nil: the node defaults.
	Decoders *StateDecoders `json:"decoders,omitempty"`
}

// PagingCharsets are the POCSAG charsets of the paging decoder
// (multimon-ng -C).
var PagingCharsets = []string{"US", "FR", "DE", "DK", "SE", "SI"}

// StateDecoders are the decoding settings a node applies.
type StateDecoders struct {
	// MaxRestarts is the crash-loop threshold of decoder processes: that
	// many unexpected exits within 5 minutes (1 to 100).
	MaxRestarts int `json:"max_restarts"`
	// DigimodesFFTSize is the secondary FFT size of the text decoders
	// (512, 1024, 2048 or 4096; 0 from an older hub: the default).
	DigimodesFFTSize int `json:"digimodes_fft_size,omitempty"`
	// ShowCW: the CW decoder also prints dots and dashes.
	ShowCW bool `json:"cw_showcw,omitempty"`
	// DSCShowErrors: the DSC decoder also reports what it could not decode
	// as a call.
	DSCShowErrors bool `json:"dsc_show_errors,omitempty"`
	// The settings of the slot decoders (additive fields, DEC-024,
	// DEC-021…023, DEC-029); zero values take the node defaults.
	//
	// WSJTDepth is the WSJT decoding depth (1 to 3), WSJTDepths the
	// per-mode depths (ft8, ft4, jt65, jt9, wspr, fst4, fst4w, q65; 0 or
	// missing: WSJTDepth).
	WSJTDepth  int            `json:"wsjt_decoding_depth,omitempty"`
	WSJTDepths map[string]int `json:"wsjt_decoding_depths,omitempty"`
	// FST4Intervals and FST4WIntervals are the enabled T/R periods in
	// seconds.
	FST4Intervals  []int `json:"fst4_enabled_intervals,omitempty"`
	FST4WIntervals []int `json:"fst4w_enabled_intervals,omitempty"`
	// Q65Combinations are the enabled Q65 submode and period combinations
	// ("A30").
	Q65Combinations []string `json:"q65_enabled_combinations,omitempty"`
	// JS8Profiles are the enabled JS8 speeds (normal, slow, fast, turbo),
	// JS8Depth the JS8 decoding depth (1 to 3).
	JS8Profiles []string `json:"js8_enabled_profiles,omitempty"`
	JS8Depth    int      `json:"js8_decoding_depth,omitempty"`
	// FAX are the HF FAX decoder settings (fax_*, DEC-038); nil: the node
	// defaults.
	FAX *StateFAX `json:"fax,omitempty"`
	// PagingFilter keeps only the readable pages (DEC-033).
	PagingFilter bool `json:"paging_filter,omitempty"`
	// PagingCharset is the POCSAG charset: US, FR, DE, DK, SE or SI; ""
	// for US.
	PagingCharset string `json:"paging_charset,omitempty"`
	// ISMReportLevels adds the signal levels to the ISM decodes (rtl_433
	// -M level, DEC-039).
	ISMReportLevels bool `json:"ism_report_levels,omitempty"`
}

// StateFAX are the settings of the FAX decoders a node starts.
type StateFAX struct {
	// LPM is the line rate in lines per minute (30 to 480).
	LPM int `json:"lpm"`
	// MinLength is the shortest page saved to Files, in lines (50 to 450).
	MinLength int `json:"min_length"`
	// MaxLength ends a page, in lines (500 to 8000).
	MaxLength   int  `json:"max_length"`
	PostProcess bool `json:"postprocess"`
	Color       bool `json:"color"`
	AM          bool `json:"am"`
}

// StateWaterfall are the waterfall levels (dB) and palette of the receiver.
type StateWaterfall struct {
	MinDB   int    `json:"min_db"`
	MaxDB   int    `json:"max_db"`
	Palette string `json:"palette"`
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
