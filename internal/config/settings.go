package config

import "slices"

// Settings is the [settings] table of hub.toml: admin settings (TECHNICAL_SPEC
// §7.4 "Locking semantics"). Each leaf maps 1:1 to a DB settings key (the
// dotted path without "settings."). A leaf set in a file or the env is
// locked: read-only in the admin UI and refused by the API (409
// setting_locked). Unset leaves fall back to the DB setting, then to the
// default (ADR 0010).
//
// These structs are the source of the settings schema: keys, types,
// defaults (DefaultSettings), descriptions and constraints. One validator
// (DecodeSetting) checks every value against that schema, whatever its
// source. Annotations specific to settings use jsonschema_extras: x-apply
// (live|restart, default live), x-public (readable anonymously), x-widget
// (form control override) and x-label (short label); x-min-duration and
// x-max-duration bound durations.
type Settings struct {
	ListenPolicy string `toml:"listen_policy" env:"LISTEN_POLICY" jsonschema:"enum=anonymous,enum=registered" jsonschema_extras:"x-public=true,x-label=Listen policy" jsonschema_description:"Global listen policy: anonymous lets visitors listen without an account, registered requires a signed-in listener. A device can override it in its node config."`
	// WFMDeemphasis is the broadcast FM de-emphasis time constant in µs
	// (DEM-016), pushed to the nodes in the desired state.
	// AudioCompression is the codec the receivers ask the nodes for
	// (DEM-010): adpcm (ADPCM IMA, 4:1) or pcm (16-bit PCM).
	AudioCompression string `toml:"audio_compression" env:"AUDIO_COMPRESSION" jsonschema:"enum=adpcm,enum=pcm" jsonschema_extras:"x-public=true,x-label=Audio compression" jsonschema_description:"Audio sent to the listeners: adpcm (ADPCM, a quarter of the bandwidth) or pcm (uncompressed 16-bit PCM). Applies to receivers opened from now on."`
	WFMDeemphasis    int    `toml:"wfm_deemphasis" env:"WFM_DEEMPHASIS" jsonschema:"enum=50,enum=75" jsonschema_extras:"x-label=WFM de-emphasis (µs)" jsonschema_description:"Broadcast FM de-emphasis time constant, in microseconds: 50 (Europe and most of the world) or 75 (Americas, South Korea). Nodes apply it to their WFM demodulators."`

	// The fax_* keys are the HF FAX decoder settings (DEC-038, ADM-028):
	// Admin › Decoding, pushed to the nodes in the desired state.
	FAXLPM         int  `toml:"fax_lpm" env:"FAX_LPM" jsonschema:"minimum=30,maximum=480" jsonschema_extras:"x-label=FAX transmission speed (lines per minute)" jsonschema_description:"Line rate of the FAX transmissions, in lines per minute (30 to 480; weather FAX uses 120). Applies to FAX decoders started from now on."`
	FAXMinLength   int  `toml:"fax_min_length" env:"FAX_MIN_LENGTH" jsonschema:"minimum=50,maximum=450" jsonschema_extras:"x-label=Shortest FAX page kept (lines)" jsonschema_description:"A FAX page shorter than this many lines is not saved to Files (50 to 450)."`
	FAXMaxLength   int  `toml:"fax_max_length" env:"FAX_MAX_LENGTH" jsonschema:"minimum=500,maximum=8000" jsonschema_extras:"x-label=Longest FAX page (lines)" jsonschema_description:"A FAX page ends after this many lines (500 to 8000: the hub keeps images of at most 16 megapixels). Applies to FAX decoders started from now on."`
	FAXPostprocess bool `toml:"fax_postprocess" env:"FAX_POSTPROCESS" jsonschema_extras:"x-label=Reduce FAX noise" jsonschema_description:"Post-process the received FAX lines to reduce noise. Applies to FAX decoders started from now on."`
	FAXColor       bool `toml:"fax_color" env:"FAX_COLOR" jsonschema_extras:"x-label=Colour FAX" jsonschema_description:"Receive colour FAX pages (three channels per line) instead of greyscale. Applies to FAX decoders started from now on."`
	FAXAM          bool `toml:"fax_am" env:"FAX_AM" jsonschema_extras:"x-label=FAX amplitude modulation" jsonschema_description:"Decode the FAX lines from the amplitude instead of the frequency of the audio. Applies to FAX decoders started from now on."`

	Receiver  SettingsReceiver  `toml:"receiver" envPrefix:"RECEIVER__" jsonschema:"description=Receiver identity and policies."`
	Privacy   SettingsPrivacy   `toml:"privacy" envPrefix:"PRIVACY__" jsonschema:"description=Privacy of client data."`
	UI        SettingsUI        `toml:"ui" envPrefix:"UI__" jsonschema:"description=Look and feel."`
	Bandplan  SettingsBandplan  `toml:"bandplan" envPrefix:"BANDPLAN__" jsonschema:"description=Band plan and region bookmark pack."`
	Waterfall SettingsWaterfall `toml:"waterfall" envPrefix:"WATERFALL__" jsonschema:"description=Waterfall defaults of the receiver (ADR 0026)."`
	Session   SettingsSession   `toml:"session" envPrefix:"SESSION__" jsonschema:"description=Session lifetimes."`
	Auth      SettingsAuth      `toml:"auth" envPrefix:"AUTH__" jsonschema:"description=Sign-in throttling."`
	Retention SettingsRetention `toml:"retention" envPrefix:"RETENTION__" jsonschema:"description=Retention of DB-backed stores."`
	Files     SettingsFiles     `toml:"files" envPrefix:"FILES__" jsonschema:"description=Retention of the files the nodes send (FIL-004)."`
	Grid      SettingsGrid      `toml:"grid" envPrefix:"GRID__" jsonschema:"description=Node health (GRID-009)."`
	Decoders  SettingsDecoders  `toml:"decoders" envPrefix:"DECODERS__" jsonschema:"description=Decoding settings pushed to the nodes (Admin › Decoding)."`
	Map       SettingsMap       `toml:"map" envPrefix:"MAP__" jsonschema:"description=Map layers, retention and report filtering (Admin › Map)."`
	Links     SettingsLinks     `toml:"links" envPrefix:"LINKS__" jsonschema:"description=Lookup link templates (MAP-015)."`

	Invitations   SettingsInvitations   `toml:"invitations" envPrefix:"INVITATIONS__" jsonschema:"description=Invitations (ACC-002)."`
	PasswordReset SettingsPasswordReset `toml:"password_reset" envPrefix:"PASSWORD_RESET__" jsonschema:"description=Password reset links (ACC-003)."`
}

// SettingsReceiver is the [settings.receiver] table (ADM-003).
type SettingsReceiver struct {
	Name     string   `toml:"name" env:"NAME" jsonschema:"minLength=1,maxLength=64" jsonschema_extras:"x-public=true,x-label=Station name" jsonschema_description:"Station name, shown in the top bar and the page titles."`
	Location string   `toml:"location" env:"LOCATION" jsonschema:"maxLength=128" jsonschema_extras:"x-public=true,x-label=Location" jsonschema_description:"Station location, as free text (for example a town and a country)."`
	GPS      GeoPoint `toml:"gps" env:"GPS" jsonschema_extras:"x-label=Position" jsonschema_description:"Station position in decimal degrees (WGS 84). Published (status, map) at the centre of its 4-character locator unless map.precise_receivers is set. In the env: \"<lat>,<lon>\"."`
	// AltitudeM is the antenna altitude; it stays at its default (0) until
	// an admin sets it, and the public status then includes it.
	AltitudeM int `toml:"altitude_m" env:"ALTITUDE_M" jsonschema:"minimum=-500,maximum=9000" jsonschema_extras:"x-public=true,x-label=Antenna altitude (m)" jsonschema_description:"Antenna altitude above sea level, in metres (optional: published in the public status once set)."`
	// AdminEmail is the contact e-mail; the public status (API-003) shows
	// it only when AdminEmailPublic is true.
	AdminEmail       string `toml:"admin_email" env:"ADMIN_EMAIL" jsonschema:"format=email,maxLength=254" jsonschema_extras:"x-label=Contact e-mail" jsonschema_description:"Contact e-mail of the station administrator."`
	AdminEmailPublic bool   `toml:"admin_email_public" env:"ADMIN_EMAIL_PUBLIC" jsonschema_extras:"x-label=Publish the contact e-mail" jsonschema_description:"Include the contact e-mail in the public status (GET /api/v1/status). Off: the e-mail stays private."`
	HelpURL          string `toml:"help_url" env:"HELP_URL" jsonschema:"format=uri,maxLength=2048" jsonschema_extras:"x-public=true,x-label=Help link" jsonschema_description:"Help link in the header: an http or https URL."`
	PhotoTitle       string `toml:"photo_title" env:"PHOTO_TITLE" jsonschema:"maxLength=128" jsonschema_extras:"x-public=true,x-label=Panorama title" jsonschema_description:"Title of the station panorama."`
	PhotoDesc        string `toml:"photo_desc" env:"PHOTO_DESC" jsonschema:"maxLength=4000" jsonschema_extras:"x-public=true,x-widget=markdown,x-label=Panorama description" jsonschema_description:"Description of the station panorama, in Markdown (raw HTML is not rendered)."`
	UsagePolicyText  string `toml:"usage_policy_text" env:"USAGE_POLICY_TEXT" jsonschema:"maxLength=20000" jsonschema_extras:"x-widget=markdown,x-label=Usage policy" jsonschema_description:"Usage policy shown at /policy (UI-003), in Markdown (raw HTML is not rendered). Empty: the built-in default policy."`
	UsagePolicyURL   string `toml:"usage_policy_url" env:"USAGE_POLICY_URL" jsonschema:"format=uri-reference,maxLength=2048" jsonschema_extras:"x-public=true,x-label=Usage policy link" jsonschema_description:"Usage policy link of the footer: a path on this hub (/policy) or an http or https URL."`
}

// SettingsPrivacy is the [settings.privacy] table (PRS-001).
type SettingsPrivacy struct {
	MaskIPs bool `toml:"mask_ips" env:"MASK_IPS" jsonschema_extras:"x-label=Mask client addresses" jsonschema_description:"Mask client IP addresses in Admin › Connections: IPv4 to the /24 network, IPv6 to the /48 prefix. An admin can reveal one address at a time; each reveal is audited."`
}

// SettingsUI is the [settings.ui] table (ADM-006, UI-001).
type SettingsUI struct {
	ThemeMode       string `toml:"theme_mode" env:"THEME_MODE" jsonschema:"enum=light,enum=dark,enum=auto" jsonschema_extras:"x-public=true,x-label=Theme" jsonschema_description:"Theme mode (UI-001): light or dark, or auto to follow the visitor's prefers-color-scheme. A change applies on the next full page load."`
	ShortcutSet     string `toml:"shortcut_set" env:"SHORTCUT_SET" jsonschema:"enum=default,enum=off" jsonschema_extras:"x-public=true,x-label=Keyboard shortcuts" jsonschema_description:"Keyboard shortcut set: default, or off to disable single-key shortcuts."`
	RecorderEnabled bool   `toml:"recorder_enabled" env:"RECORDER_ENABLED" jsonschema_extras:"x-public=true,x-label=Browser recorder" jsonschema_description:"Show the Record button and the R shortcut of the receiver to every listener (REC-001); admins always have them. A convenience switch: it cannot prevent recording, because the audio is streamed to the browser anyway."`
}

// SettingsBandplan is the [settings.bandplan] table (RX-029, BMK-002).
type SettingsBandplan struct {
	Region string `toml:"region" env:"REGION" jsonschema:"enum=r1,enum=r2,enum=r3" jsonschema_extras:"x-public=true,x-label=Band plan region,x-enum-labels=R1,x-enum-labels=R2,x-enum-labels=R3" jsonschema_description:"IARU region of the band plan and of the region bookmark pack: r1 (Europe, Africa, Middle East), r2 (Americas) or r3 (Asia, Pacific)."`
}

// SettingsWaterfall is the [settings.waterfall] table: the one source of
// the receiver waterfall levels and palette (ADR 0026), pushed to the nodes
// in the desired state. Listeners still have the Auto levels button.
type SettingsWaterfall struct {
	MinDB   int    `toml:"min_db" env:"MIN_DB" jsonschema:"minimum=-200,maximum=50" jsonschema_extras:"x-public=true,x-label=Lowest level (dB)" jsonschema_description:"Waterfall level shown with the darkest colour, in dB (-200 to 50). Must be below the highest level."`
	MaxDB   int    `toml:"max_db" env:"MAX_DB" jsonschema:"minimum=-200,maximum=50" jsonschema_extras:"x-public=true,x-label=Highest level (dB)" jsonschema_description:"Waterfall level shown with the brightest colour, in dB (-200 to 50)."`
	Palette string `toml:"palette" env:"PALETTE" jsonschema:"enum=default,enum=turbo" jsonschema_extras:"x-public=true,x-label=Palette" jsonschema_description:"Waterfall colour palette: default or turbo."`
}

// SettingsSession is the [settings.session] table (AUTH-003, ADR 0009).
type SettingsSession struct {
	IdleTimeout       Duration `toml:"idle_timeout" env:"IDLE_TIMEOUT" jsonschema_extras:"x-min-duration=5m,x-max-duration=90d,x-label=Idle timeout" jsonschema_description:"A session ends after this long without activity."`
	AbsoluteTimeout   Duration `toml:"absolute_timeout" env:"ABSOLUTE_TIMEOUT" jsonschema_extras:"x-min-duration=5m,x-max-duration=365d,x-label=Session lifetime" jsonschema_description:"A session ends this long after sign-in, whatever its activity."`
	RememberMeTimeout Duration `toml:"remember_me_timeout" env:"REMEMBER_ME_TIMEOUT" jsonschema_extras:"x-min-duration=1h,x-max-duration=365d,x-label=Remembered session lifetime" jsonschema_description:"Lifetime of a session opened with \"Keep me signed in\"."`
}

// SettingsAuth is the [settings.auth] table: login throttling (AUTH-002,
// ADR 0009). It is not the config-only [auth] bootstrap table.
type SettingsAuth struct {
	PasswordMinLength int                 `toml:"password_min_length" env:"PASSWORD_MIN_LENGTH" jsonschema:"minimum=8,maximum=256" jsonschema_extras:"x-label=Minimum password length" jsonschema_description:"Minimum length of a new password, in characters (8 to 256; ADR 0011). Existing passwords are not affected."`
	LoginRateLimit    Rate                `toml:"login_rate_limit" env:"LOGIN_RATE_LIMIT" jsonschema_extras:"x-label=Sign-in attempts per client address" jsonschema_description:"Sign-in attempts allowed per client address: <count>/<window> (for example 5/1m), at most 100 per window of at least 1m."`
	Lockout           SettingsAuthLockout `toml:"lockout" envPrefix:"LOCKOUT__" jsonschema:"description=Per-account throttling after failed sign-ins."`
}

// SettingsAuthLockout is the [settings.auth.lockout] table.
type SettingsAuthLockout struct {
	DelayAfter int      `toml:"delay_after" env:"DELAY_AFTER" jsonschema:"minimum=1,maximum=100" jsonschema_extras:"x-label=Delay after failures" jsonschema_description:"From this many consecutive failures, each attempt waits 1 s, 2 s, 4 s and so on."`
	LockAfter  int      `toml:"lock_after" env:"LOCK_AFTER" jsonschema:"minimum=2,maximum=1000" jsonschema_extras:"x-label=Lock after failures" jsonschema_description:"This many consecutive failures lock the account. Must be greater than delay_after."`
	LockFor    Duration `toml:"lock_for" env:"LOCK_FOR" jsonschema_extras:"x-min-duration=1m,x-max-duration=7d,x-label=Lock duration" jsonschema_description:"First lock duration, doubled on every further failure."`
	MaxLock    Duration `toml:"max_lock" env:"MAX_LOCK" jsonschema_extras:"x-min-duration=1m,x-max-duration=30d,x-label=Longest lock" jsonschema_description:"Longest lock duration. Must not be shorter than lock_for."`
}

// SettingsRetention is the [settings.retention] table (ADM-011, TECHNICAL_SPEC
// §7.3 "Retention jobs").
type SettingsRetention struct {
	Sessions Duration `toml:"sessions" env:"SESSIONS" jsonschema_extras:"x-min-duration=1d,x-max-duration=3650d,x-label=Ended sessions" jsonschema_description:"Ended sessions (expired or revoked) are deleted after this long."`
	AuditLog Duration `toml:"audit_log" env:"AUDIT_LOG" jsonschema_extras:"x-min-duration=30d,x-max-duration=3650d,x-label=Audit log" jsonschema_description:"Audit log entries are deleted after this long (at least 30 days)."`
	// Connections is the retention of closed presence rows (GRID-017).
	Connections Duration `toml:"connections" env:"CONNECTIONS" jsonschema_extras:"x-min-duration=1d,x-max-duration=3650d,x-label=Closed connections" jsonschema_description:"Closed connections of the presence registry are deleted after this long."`
	// DecodedMessages bounds the decoded messages (DEC-047, ADR 0028).
	DecodedMessages SettingsDecodedRetention `toml:"decoded_messages" envPrefix:"DECODED_MESSAGES__" jsonschema:"description=Retention of the decoded messages (DEC-047)."`
}

// SettingsDecodedRetention is the [settings.retention.decoded_messages]
// table: one age for every mode, plus a row cap (ADR 0028).
type SettingsDecodedRetention struct {
	MaxAge  Duration `toml:"max_age" env:"MAX_AGE" jsonschema_extras:"x-min-duration=1d,x-max-duration=3650d,x-label=Decoded messages" jsonschema_description:"Decoded messages are deleted after this long."`
	MaxRows int      `toml:"max_rows" env:"MAX_ROWS" jsonschema:"minimum=1000,maximum=100000000" jsonschema_extras:"x-label=Decoded messages kept at most" jsonschema_description:"Beyond this many decoded messages, the oldest are deleted (1000 to 100000000)."`
}

// SettingsDecoders is the [settings.decoders] table (Admin › Decoding):
// the decoding settings the hub pushes to the nodes in the desired state.
type SettingsDecoders struct {
	// MaxRestarts is the crash-loop threshold of decoder processes (ADR 0017
	// decision 4, DIAG-003).
	MaxRestarts int `toml:"max_restarts" env:"MAX_RESTARTS" jsonschema:"minimum=1,maximum=100" jsonschema_extras:"x-label=Restarts before a decoder gives up" jsonschema_description:"A decoder process that exits unexpectedly this many times within 5 minutes stops restarting: the session shows an error and retries every 10 minutes, or when the listener selects the decoder again (1 to 100). Applies to sessions started from now on."`
	// DigimodesFFTSize is the size of the secondary FFT of the text
	// decoders (DEC-004).
	DigimodesFFTSize int `toml:"digimodes_fft_size" env:"DIGIMODES_FFT_SIZE" jsonschema:"enum=512,enum=1024,enum=2048,enum=4096" jsonschema_extras:"x-label=Decoder waterfall FFT size" jsonschema_description:"Size of the FFT of the decoder waterfall of PSK, RTTY, SITOR-B and CW (Receiver › Decoders): 512, 1024, 2048 or 4096 bins over 12 kHz. Applies to decoders started from now on."`
	// ShowCW: the CW decoder also prints dots and dashes (DEC-012).
	ShowCW bool `toml:"cw_showcw" env:"CW_SHOWCW" jsonschema_extras:"x-label=Show CW symbols" jsonschema_description:"The CW decoder also prints the dots and dashes it receives. Applies to decoders started from now on."`
	// WSJTDecodingDepth and WSJTDecodingDepths are the depths of the WSJT
	// decoders (DEC-024).
	WSJTDecodingDepth  int                `toml:"wsjt_decoding_depth" env:"WSJT_DECODING_DEPTH" jsonschema:"minimum=1,maximum=3" jsonschema_extras:"x-label=WSJT decoding depth" jsonschema_description:"Decoding depth of the WSJT-X decoders (FT8, FT4, JT65, JT9, WSPR, FST4, FST4W, Q65) without a depth of their own: 1 fast, 2 normal, 3 deep (more decodes, more CPU)."`
	WSJTDecodingDepths SettingsWSJTDepths `toml:"wsjt_decoding_depths" envPrefix:"WSJT_DECODING_DEPTHS__" jsonschema:"description=Decoding depth per WSJT mode (0: the WSJT decoding depth)."`
	// FST4Intervals, FST4WIntervals and Q65Combinations are the slots the
	// FST4, FST4W and Q65 decoders decode (DEC-021, DEC-022, DEC-023).
	FST4Intervals   []string `toml:"fst4_enabled_intervals" env:"FST4_ENABLED_INTERVALS" jsonschema:"minItems=1,maxItems=7,enum=15,enum=30,enum=60,enum=120,enum=300,enum=900,enum=1800" jsonschema_extras:"x-label=FST4 periods (seconds)" jsonschema_description:"T/R periods the FST4 decoder decodes, one per line: 15, 30, 60, 120, 300, 900 or 1800 seconds. Each period decodes its own slots."`
	FST4WIntervals  []string `toml:"fst4w_enabled_intervals" env:"FST4W_ENABLED_INTERVALS" jsonschema:"minItems=1,maxItems=4,enum=120,enum=300,enum=900,enum=1800" jsonschema_extras:"x-label=FST4W periods (seconds)" jsonschema_description:"T/R periods the FST4W decoder decodes, one per line: 120, 300, 900 or 1800 seconds."`
	Q65Combinations []string `toml:"q65_enabled_combinations" env:"Q65_ENABLED_COMBINATIONS" jsonschema:"minItems=1,maxItems=22,enum=A15,enum=B15,enum=C15,enum=A30,enum=B30,enum=C30,enum=D30,enum=A60,enum=B60,enum=C60,enum=D60,enum=E60,enum=A120,enum=B120,enum=C120,enum=D120,enum=E120,enum=A300,enum=B300,enum=C300,enum=D300,enum=E300" jsonschema_extras:"x-label=Q65 submodes and periods" jsonschema_description:"Q65 submode (A to E) and T/R period (seconds) combinations the Q65 decoder decodes, one per line, such as A30 or E120. Only the combinations narrower than 2700 Hz are valid."`
	// JS8Profiles and JS8DecodingDepth configure the JS8Call decoder
	// (DEC-029).
	JS8Profiles      []string `toml:"js8_enabled_profiles" env:"JS8_ENABLED_PROFILES" jsonschema:"minItems=1,maxItems=4,enum=normal,enum=slow,enum=fast,enum=turbo" jsonschema_extras:"x-label=JS8Call speeds" jsonschema_description:"JS8Call speeds the decoder decodes, one per line: normal (15 s), slow (30 s), fast (10 s) or turbo (6 s)."`
	JS8DecodingDepth int      `toml:"js8_decoding_depth" env:"JS8_DECODING_DEPTH" jsonschema:"minimum=1,maximum=3" jsonschema_extras:"x-label=JS8Call decoding depth" jsonschema_description:"Decoding depth of the JS8Call decoder: 1 fast, 2 normal, 3 deep."`
	// PagingFilter and PagingCharset configure the paging decoder
	// (DEC-033, ADM-027).
	PagingFilter  bool   `toml:"paging_filter" env:"PAGING_FILTER" jsonschema_extras:"x-label=Readable pages only" jsonschema_description:"The paging decoder keeps only readable messages: POCSAG alphanumeric pages with text and FLEX alphanumeric pages that look like words. Off: every page, numeric and tone-only ones included. Applies to sessions started from now on."`
	PagingCharset string `toml:"paging_charset" env:"PAGING_CHARSET" jsonschema:"enum=US,enum=FR,enum=DE,enum=DK,enum=SE,enum=SI" jsonschema_extras:"x-label=POCSAG charset" jsonschema_description:"Character set of POCSAG alphanumeric pages: US (ASCII), or the national variant FR, DE, DK, SE or SI. Applies to sessions started from now on."`
	// ISMReportLevels keeps the rtl_433 signal levels in the ISM decodes
	// (DEC-039, ADM-026).
	ISMReportLevels bool `toml:"ism_report_levels" env:"ISM_REPORT_LEVELS" jsonschema_extras:"x-label=ISM signal levels" jsonschema_description:"The ISM and Wireless M-Bus decoders keep the signal level, SNR and noise of each message (rtl_433 -M level). Applies to sessions started from now on."`
}

// SettingsWSJTDepths is the [settings.decoders.wsjt_decoding_depths]
// table: the decoding depth of each WSJT mode, 0 for the global one
// (DEC-024).
type SettingsWSJTDepths struct {
	FT8   int `toml:"ft8" env:"FT8" jsonschema:"minimum=0,maximum=3" jsonschema_extras:"x-label=FT8 depth" jsonschema_description:"0 to 3; 0: the WSJT decoding depth."`
	FT4   int `toml:"ft4" env:"FT4" jsonschema:"minimum=0,maximum=3" jsonschema_extras:"x-label=FT4 depth" jsonschema_description:"0 to 3; 0: the WSJT decoding depth."`
	JT65  int `toml:"jt65" env:"JT65" jsonschema:"minimum=0,maximum=3" jsonschema_extras:"x-label=JT65 depth" jsonschema_description:"0 to 3; 0: the WSJT decoding depth."`
	JT9   int `toml:"jt9" env:"JT9" jsonschema:"minimum=0,maximum=3" jsonschema_extras:"x-label=JT9 depth" jsonschema_description:"0 to 3; 0: the WSJT decoding depth."`
	WSPR  int `toml:"wspr" env:"WSPR" jsonschema:"minimum=0,maximum=3" jsonschema_extras:"x-label=WSPR depth" jsonschema_description:"0 to 3; 0: the WSJT decoding depth. Above 1, wsprd searches deeper."`
	FST4  int `toml:"fst4" env:"FST4" jsonschema:"minimum=0,maximum=3" jsonschema_extras:"x-label=FST4 depth" jsonschema_description:"0 to 3; 0: the WSJT decoding depth."`
	FST4W int `toml:"fst4w" env:"FST4W" jsonschema:"minimum=0,maximum=3" jsonschema_extras:"x-label=FST4W depth" jsonschema_description:"0 to 3; 0: the WSJT decoding depth."`
	Q65   int `toml:"q65" env:"Q65" jsonschema:"minimum=0,maximum=3" jsonschema_extras:"x-label=Q65 depth" jsonschema_description:"0 to 3; 0: the WSJT decoding depth."`
}

// BaseLayers are the ids of the map base layers the hub knows (MAP-004):
// keyless tile providers the browser loads directly.
var BaseLayers = []string{
	"osm", "opentopomap", "esri_world_imagery", "esri_world_street_map", "esri_world_topo_map",
	"cartodb_positron", "cartodb_dark_matter", "cartodb_voyager",
}

// SettingsMap is the [settings.map] table (MAP-004, MAP-007, MAP-011,
// MAP-012, MAP-013): Admin › Map.
type SettingsMap struct {
	BaseLayers       []string `toml:"base_layers" env:"BASE_LAYERS" jsonschema:"minItems=1,maxItems=8,enum=osm,enum=opentopomap,enum=esri_world_imagery,enum=esri_world_street_map,enum=esri_world_topo_map,enum=cartodb_positron,enum=cartodb_dark_matter,enum=cartodb_voyager" jsonschema_extras:"x-public=true,x-label=Base layers" jsonschema_description:"Base layers offered on the map, one per line: osm, opentopomap, esri_world_imagery, esri_world_street_map, esri_world_topo_map, cartodb_positron, cartodb_dark_matter or cartodb_voyager."`
	DefaultBaseLayer string   `toml:"default_base_layer" env:"DEFAULT_BASE_LAYER" jsonschema:"enum=osm,enum=opentopomap,enum=esri_world_imagery,enum=esri_world_street_map,enum=esri_world_topo_map,enum=cartodb_positron,enum=cartodb_dark_matter,enum=cartodb_voyager" jsonschema_extras:"x-public=true,x-label=Default base layer,x-enum-labels=OpenStreetMap,x-enum-labels=OpenTopoMap,x-enum-labels=Esri World Imagery,x-enum-labels=Esri World Street Map,x-enum-labels=Esri World Topo Map,x-enum-labels=CARTO Positron,x-enum-labels=CARTO Dark Matter,x-enum-labels=CARTO Voyager" jsonschema_description:"Base layer the map opens with. Must be one of the offered base layers."`
	// PositionRetentionS is the lifetime of a map position without a TTL
	// of its own (MAP-012).
	PositionRetentionS int `toml:"position_retention_s" env:"POSITION_RETENTION_S" jsonschema:"minimum=60,maximum=604800" jsonschema_extras:"x-public=true,x-label=Position lifetime (s)" jsonschema_description:"A position, a locator or a station heard again restarts its lifetime; without a new report it leaves the map after this many seconds (60 to 604800)."`
	MaxCalls           int `toml:"max_calls" env:"MAX_CALLS" jsonschema:"minimum=0,maximum=100" jsonschema_extras:"x-public=true,x-label=Call lines shown" jsonschema_description:"Most recent call lines (QSOs between two located stations) kept on the map (0 to 100; 0: no call lines)."`
	CallRetentionS     int `toml:"call_retention_s" env:"CALL_RETENTION_S" jsonschema:"minimum=10,maximum=86400" jsonschema_extras:"x-public=true,x-label=Call line lifetime (s)" jsonschema_description:"A call line leaves the map this many seconds after the call was heard (10 to 86400)."`
	// IgnoreIndirectReports and PreferRecentReports filter the reports
	// before they are written (MAP-013).
	IgnoreIndirectReports bool `toml:"ignore_indirect_reports" env:"IGNORE_INDIRECT_REPORTS" jsonschema_extras:"x-label=Ignore indirect reports" jsonschema_description:"Drop the positions heard through a digipeater or relayed as third-party traffic: the map shows only the stations heard directly."`
	PreferRecentReports   bool `toml:"prefer_recent_reports" env:"PREFER_RECENT_REPORTS" jsonschema_extras:"x-label=Keep the most recent report" jsonschema_description:"A report older than the one the map shows for the same station (a delayed or replayed packet) is dropped. Off: the last report received wins."`
	// PreciseReceivers shows the receivers at their configured position;
	// off, at the centre of their 4-character locator (SR-32).
	PreciseReceivers bool `toml:"precise_receivers" env:"PRECISE_RECEIVERS" jsonschema_extras:"x-label=Show the exact receiver positions" jsonschema_description:"Publish the exact configured positions of the station and the receivers (map, GET /api/v1/status). Off: the centre of their 4-character locator (about 100 by 200 km), which keeps the station location private."`
}

// SettingsLinks is the [settings.links] table (MAP-015, ADM-032): lookup
// links with one {} placeholder, replaced by the URL-encoded value.
type SettingsLinks struct {
	CallsignURL string `toml:"callsign_url" env:"CALLSIGN_URL" jsonschema:"maxLength=2048" jsonschema_extras:"x-public=true,x-label=Callsign lookup" jsonschema_description:"Callsign lookup link: an http or https URL with exactly one {} placeholder for the callsign. Empty: no link."`
	VesselURL   string `toml:"vessel_url" env:"VESSEL_URL" jsonschema:"maxLength=2048" jsonschema_extras:"x-public=true,x-label=Vessel lookup" jsonschema_description:"Vessel lookup link: an http or https URL with exactly one {} placeholder for the MMSI. Empty: no link."`
	FlightURL   string `toml:"flight_url" env:"FLIGHT_URL" jsonschema:"maxLength=2048" jsonschema_extras:"x-public=true,x-label=Flight lookup" jsonschema_description:"Flight lookup link: an http or https URL with exactly one {} placeholder for the flight number. Empty: no link."`
	ModesURL    string `toml:"modes_url" env:"MODES_URL" jsonschema:"maxLength=2048" jsonschema_extras:"x-public=true,x-label=Mode S lookup" jsonschema_description:"Aircraft lookup link: an http or https URL with exactly one {} placeholder for the ICAO 24-bit address. Empty: no link."`
	SondeURL    string `toml:"sonde_url" env:"SONDE_URL" jsonschema:"maxLength=2048" jsonschema_extras:"x-public=true,x-label=Sonde lookup" jsonschema_description:"Radiosonde lookup link: an http or https URL with exactly one {} placeholder for the sonde serial. Empty: no link."`
	GeoIPURL    string `toml:"geoip_url" env:"GEOIP_URL" jsonschema:"maxLength=2048" jsonschema_extras:"x-label=IP address lookup" jsonschema_description:"IP address lookup link of the admin connections view: an http or https URL with exactly one {} placeholder for the address. Empty: no link."`
}

// SettingsFiles is the [settings.files] table: retention of the files the
// nodes send (FIL-004), applied after each new file and by a periodic job,
// oldest files first.
type SettingsFiles struct {
	RetentionCount int `toml:"retention_count" env:"RETENTION_COUNT" jsonschema:"minimum=1,maximum=100000" jsonschema_extras:"x-label=Files kept per kind" jsonschema_description:"The newest files of each kind (SSTV, FAX, text log…) are kept, up to this many (1 to 100000); older ones are deleted."`
	RetentionDays  int `toml:"retention_days" env:"RETENTION_DAYS" jsonschema:"minimum=0,maximum=3650" jsonschema_extras:"x-label=Maximum age (days)" jsonschema_description:"Files older than this many days are deleted (0 to 3650). 0 keeps files whatever their age."`
	MaxTotalBytes  int `toml:"max_total_bytes" env:"MAX_TOTAL_BYTES" jsonschema:"minimum=0,maximum=1099511627776" jsonschema_extras:"x-label=Total size cap (bytes)" jsonschema_description:"When the files take more than this many bytes, the oldest are deleted (0 to 1 TiB). 0: no size cap."`
}

// SettingsGrid is the [settings.grid] table: node heartbeats and health
// (GRID-009, ADR 0018).
type SettingsGrid struct {
	HeartbeatIntervalS int `toml:"heartbeat_interval_s" env:"HEARTBEAT_INTERVAL_S" jsonschema:"minimum=1,maximum=300" jsonschema_extras:"x-label=Heartbeat interval (s)" jsonschema_description:"Nodes report a heartbeat this often, in seconds (1 to 300). Two missed heartbeats mark a node degraded. A node picks up a new interval when its control channel reconnects."`
	OfflineAfterS      int `toml:"offline_after_s" env:"OFFLINE_AFTER_S" jsonschema:"minimum=5,maximum=3600" jsonschema_extras:"x-label=Offline after (s)" jsonschema_description:"A node without a heartbeat for this many seconds is marked offline (5 to 3600). Must be more than twice the heartbeat interval."`
}

// SettingsInvitations is the [settings.invitations] table (ACC-002, ADR
// 0011).
type SettingsInvitations struct {
	TTLHours int `toml:"ttl_hours" env:"TTL_HOURS" jsonschema:"minimum=1,maximum=720" jsonschema_extras:"x-label=Invitation validity (hours)" jsonschema_description:"An invitation link expires this many hours after it is created (1 to 720). Existing invitations keep their expiry."`
}

// SettingsPasswordReset is the [settings.password_reset] table (ACC-003,
// ADR 0011).
type SettingsPasswordReset struct {
	TTLMinutes int `toml:"ttl_minutes" env:"TTL_MINUTES" jsonschema:"minimum=5,maximum=1440" jsonschema_extras:"x-label=Reset link validity (minutes)" jsonschema_description:"A password reset link expires this many minutes after it is sent (5 to 1440). Links already sent keep their expiry."`
}

// DefaultSettings returns the built-in defaults of the settings.
func DefaultSettings() Settings {
	return Settings{
		ListenPolicy:     "anonymous",
		AudioCompression: "adpcm",
		WFMDeemphasis:    50,
		FAXLPM:           120, FAXMinLength: 200, FAXMaxLength: 1500, FAXPostprocess: true,
		Receiver: SettingsReceiver{Name: "MeshSDR", UsagePolicyURL: "/policy"},
		Privacy:  SettingsPrivacy{MaskIPs: true},
		UI: SettingsUI{
			ThemeMode: "auto", ShortcutSet: "default", RecorderEnabled: true,
		},
		Bandplan:  SettingsBandplan{Region: "r1"},
		Waterfall: SettingsWaterfall{MinDB: -88, MaxDB: -20, Palette: "turbo"},
		Session: SettingsSession{
			IdleTimeout: MustDuration("24h"), AbsoluteTimeout: MustDuration("24h"), RememberMeTimeout: MustDuration("30d"),
		},
		Auth: SettingsAuth{
			PasswordMinLength: 10,
			LoginRateLimit:    MustRate("5/1m"),
			Lockout: SettingsAuthLockout{
				DelayAfter: 5, LockAfter: 10, LockFor: MustDuration("15m"), MaxLock: MustDuration("24h"),
			},
		},
		Retention: SettingsRetention{
			Sessions: MustDuration("30d"), AuditLog: MustDuration("365d"), Connections: MustDuration("30d"),
			DecodedMessages: SettingsDecodedRetention{MaxAge: MustDuration("30d"), MaxRows: 1_000_000},
		},
		Files: SettingsFiles{RetentionCount: 20},
		Grid:  SettingsGrid{HeartbeatIntervalS: 10, OfflineAfterS: 60},
		Decoders: SettingsDecoders{
			MaxRestarts: 5, DigimodesFFTSize: 2048, WSJTDecodingDepth: 3, WSJTDecodingDepths: SettingsWSJTDepths{JT65: 1},
			FST4Intervals: []string{"15", "30"}, FST4WIntervals: []string{"120", "300"}, Q65Combinations: []string{"A30", "E120", "C60"},
			JS8Profiles: []string{"normal", "slow"}, JS8DecodingDepth: 3, PagingCharset: "US",
		},
		Map: SettingsMap{
			BaseLayers: slices.Clone(BaseLayers), DefaultBaseLayer: "osm",
			PositionRetentionS: 7200, MaxCalls: 5, CallRetentionS: 300, PreferRecentReports: true,
		},
		Links: SettingsLinks{
			CallsignURL: "https://www.qrzcq.com/call/{}",
			VesselURL:   "https://www.vesselfinder.com/vessels/details/{}",
			FlightURL:   "https://flightaware.com/live/flight/{}",
			ModesURL:    "https://flightaware.com/live/modes/{}/redirect",
			SondeURL:    "https://sondehub.org/{}",
			GeoIPURL:    "https://www.geolocation.com/?ip={}#ipresult",
		},
		Invitations:   SettingsInvitations{TTLHours: 168},
		PasswordReset: SettingsPasswordReset{TTLMinutes: 30},
	}
}
