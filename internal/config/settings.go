package config

// Settings is the [settings] table of hub.toml: admin settings (TECHNICAL_SPEC
// §7.4 "Locking semantics"). Each leaf maps 1:1 to a DB settings key (the
// dotted path without "settings."). A leaf set in a file or the env is
// locked: read-only in the admin UI and refused by the API (409
// setting_locked). Unset leaves fall back to the DB setting, then to the
// default (ADR 0010).
//
// These structs are the source of the settings schema: keys, types,
// defaults (DefaultSettings), descriptions and constraints. One validator
// (ValidateSetting) checks every value against that schema, whatever its
// source. Annotations specific to settings use jsonschema_extras: x-apply
// (live|restart, default live), x-public (readable anonymously), x-widget
// (form control override) and x-label (short label); x-min-duration and
// x-max-duration bound durations.
type Settings struct {
	ListenPolicy string `toml:"listen_policy" env:"LISTEN_POLICY" jsonschema:"enum=anonymous,enum=registered" jsonschema_extras:"x-public=true,x-label=Listen policy" jsonschema_description:"Global listen policy: anonymous lets visitors listen without an account, registered requires a signed-in listener. A device can override it in its node config."`

	Receiver  SettingsReceiver  `toml:"receiver" envPrefix:"RECEIVER__" jsonschema:"description=Receiver identity and policies."`
	Bandplan  SettingsBandplan  `toml:"bandplan" envPrefix:"BANDPLAN__" jsonschema:"description=Band plan."`
	UI        SettingsUI        `toml:"ui" envPrefix:"UI__" jsonschema:"description=Look and feel."`
	Bookmarks SettingsBookmarks `toml:"bookmarks" envPrefix:"BOOKMARKS__" jsonschema:"description=Web-derived bookmarks."`
	Session   SettingsSession   `toml:"session" envPrefix:"SESSION__" jsonschema:"description=Session lifetimes."`
	Auth      SettingsAuth      `toml:"auth" envPrefix:"AUTH__" jsonschema:"description=Sign-in throttling."`
	Retention SettingsRetention `toml:"retention" envPrefix:"RETENTION__" jsonschema:"description=Retention of DB-backed stores."`
}

// SettingsReceiver is the [settings.receiver] table (ADM-003).
type SettingsReceiver struct {
	Name             string   `toml:"name" env:"NAME" jsonschema:"minLength=1,maxLength=64" jsonschema_extras:"x-public=true,x-label=Station name" jsonschema_description:"Station name, shown in the top bar and the page titles."`
	Location         string   `toml:"location" env:"LOCATION" jsonschema:"maxLength=128" jsonschema_extras:"x-public=true,x-label=Location" jsonschema_description:"Station location, as free text (for example a town and a country)."`
	AltitudeM        int      `toml:"altitude_m" env:"ALTITUDE_M" jsonschema:"minimum=-500,maximum=9000" jsonschema_extras:"x-public=true,x-label=Antenna altitude (m)" jsonschema_description:"Antenna altitude above sea level, in metres."`
	AdminEmail       string   `toml:"admin_email" env:"ADMIN_EMAIL" jsonschema:"format=email,maxLength=254" jsonschema_extras:"x-label=Contact e-mail" jsonschema_description:"Contact e-mail address of the station admin."`
	AdminEmailPublic bool     `toml:"admin_email_public" env:"ADMIN_EMAIL_PUBLIC" jsonschema_extras:"x-label=Publish the contact e-mail" jsonschema_description:"Publish the contact e-mail in the public station information."`
	GPS              GeoPoint `toml:"gps" env:"GPS" jsonschema_extras:"x-public=true,x-label=Position" jsonschema_description:"Station position in decimal degrees (WGS 84). In the env: \"<lat>,<lon>\"."`
	Country          string   `toml:"country" env:"COUNTRY" jsonschema:"pattern=^[A-Z]{2}$" jsonschema_extras:"x-public=true,x-label=Country" jsonschema_description:"ISO 3166-1 alpha-2 country code (for example FR). It selects the country bookmark pack."`
	HelpURL          string   `toml:"help_url" env:"HELP_URL" jsonschema:"format=uri,maxLength=2048" jsonschema_extras:"x-public=true,x-label=Help link" jsonschema_description:"Help link in the header: an http or https URL."`
	PhotoTitle       string   `toml:"photo_title" env:"PHOTO_TITLE" jsonschema:"maxLength=128" jsonschema_extras:"x-public=true,x-label=Panorama title" jsonschema_description:"Title of the station panorama."`
	PhotoDesc        string   `toml:"photo_desc" env:"PHOTO_DESC" jsonschema:"maxLength=4000" jsonschema_extras:"x-public=true,x-widget=markdown,x-label=Panorama description" jsonschema_description:"Description of the station panorama, in Markdown (raw HTML is not rendered)."`
	UsagePolicyText  string   `toml:"usage_policy_text" env:"USAGE_POLICY_TEXT" jsonschema:"maxLength=20000" jsonschema_extras:"x-widget=markdown,x-label=Usage policy" jsonschema_description:"Usage policy shown at /policy (UI-003), in Markdown (raw HTML is not rendered). Empty: the built-in default policy."`
	UsagePolicyURL   string   `toml:"usage_policy_url" env:"USAGE_POLICY_URL" jsonschema:"format=uri-reference,maxLength=2048" jsonschema_extras:"x-public=true,x-label=Usage policy link" jsonschema_description:"Usage policy link of the footer: a path on this hub (/policy) or an http or https URL."`
}

// SettingsBandplan is the [settings.bandplan] table.
type SettingsBandplan struct {
	Region int `toml:"region" env:"REGION" jsonschema:"minimum=0,maximum=3" jsonschema_extras:"x-public=true,x-label=IARU region" jsonschema_description:"IARU region of the band plan and of the region bookmark pack: 1, 2 or 3, or 0 for all regions."`
}

// SettingsUI is the [settings.ui] table (ADM-006, UI-001).
type SettingsUI struct {
	ThemeMode       string           `toml:"theme_mode" env:"THEME_MODE" jsonschema:"enum=light,enum=dark,enum=auto" jsonschema_extras:"x-public=true,x-label=Theme" jsonschema_description:"Theme mode (UI-001): light or dark, or auto to follow the visitor's prefers-color-scheme. A change applies on the next full page load."`
	ShortcutSet     string           `toml:"shortcut_set" env:"SHORTCUT_SET" jsonschema:"enum=default,enum=off" jsonschema_extras:"x-public=true,x-label=Keyboard shortcuts" jsonschema_description:"Keyboard shortcut set: default, or off to disable single-key shortcuts."`
	TuningPrecision int              `toml:"tuning_precision" env:"TUNING_PRECISION" jsonschema:"minimum=0,maximum=6" jsonschema_extras:"x-public=true,x-label=Frequency precision" jsonschema_description:"Number of decimal digits of the frequency display."`
	RecorderEnabled bool             `toml:"recorder_enabled" env:"RECORDER_ENABLED" jsonschema_extras:"x-public=true,x-label=Browser recorder" jsonschema_description:"Show the browser recorder (REC-001). A convenience switch, not an enforcement."`
	Layout          SettingsUILayout `toml:"layout" envPrefix:"LAYOUT__" jsonschema:"description=Default layout options."`
}

// SettingsUILayout is the [settings.ui.layout] table: the initial layout of
// the receiver page (FEATURE_SPEC §10 ui_layout_defaults).
type SettingsUILayout struct {
	SidePanelOpen   bool   `toml:"side_panel_open" env:"SIDE_PANEL_OPEN" jsonschema_extras:"x-public=true,x-label=Side panel open" jsonschema_description:"Open the side panel by default on desktop."`
	DefaultTab      string `toml:"default_tab" env:"DEFAULT_TAB" jsonschema:"enum=decoders,enum=bookmarks,enum=info" jsonschema_extras:"x-public=true,x-label=Default tab" jsonschema_description:"Side panel tab shown first."`
	Spectrum        bool   `toml:"spectrum" env:"SPECTRUM" jsonschema_extras:"x-public=true,x-label=Spectrum shown" jsonschema_description:"Show the spectrum above the waterfall by default."`
	Bandplan        bool   `toml:"bandplan" env:"BANDPLAN" jsonschema_extras:"x-public=true,x-label=Band plan shown" jsonschema_description:"Show the band plan by default."`
	FrequencyFormat string `toml:"frequency_format" env:"FREQUENCY_FORMAT" jsonschema:"enum=radio,enum=locale" jsonschema_extras:"x-public=true,x-label=Frequency format" jsonschema_description:"Frequency format: radio-style (14.074.000) or the browser locale."`
}

// SettingsBookmarks is the [settings.bookmarks] table.
type SettingsBookmarks struct {
	EIBiRangeKm     int `toml:"eibi_range_km" env:"EIBI_RANGE_KM" jsonschema:"minimum=0,maximum=20000" jsonschema_extras:"x-public=true,x-label=EIBi range (km)" jsonschema_description:"Range of EIBi auto-bookmarks around the station, in km. 0 disables them."`
	RepeaterRangeKm int `toml:"repeater_range_km" env:"REPEATER_RANGE_KM" jsonschema:"minimum=0,maximum=20000" jsonschema_extras:"x-public=true,x-label=Repeater range (km)" jsonschema_description:"Range of repeater auto-bookmarks around the station, in km. 0 disables them."`
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
	LoginRateLimit Rate                `toml:"login_rate_limit" env:"LOGIN_RATE_LIMIT" jsonschema_extras:"x-label=Sign-in attempts per client address" jsonschema_description:"Sign-in attempts allowed per client address: <count>/<window> (for example 5/1m), at most 100 per window of at least 1m."`
	Lockout        SettingsAuthLockout `toml:"lockout" envPrefix:"LOCKOUT__" jsonschema:"description=Per-account throttling after failed sign-ins."`
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
}

// DefaultSettings returns the built-in defaults of the settings.
func DefaultSettings() Settings {
	return Settings{
		ListenPolicy: "anonymous",
		Receiver:     SettingsReceiver{Name: "MeshSDR", UsagePolicyURL: "/policy"},
		UI: SettingsUI{
			ThemeMode: "auto", ShortcutSet: "default", TuningPrecision: 2, RecorderEnabled: true,
			Layout: SettingsUILayout{SidePanelOpen: true, DefaultTab: "decoders", Bandplan: true, FrequencyFormat: "radio"},
		},
		Session: SettingsSession{
			IdleTimeout: MustDuration("24h"), AbsoluteTimeout: MustDuration("24h"), RememberMeTimeout: MustDuration("30d"),
		},
		Auth: SettingsAuth{
			LoginRateLimit: MustRate("5/1m"),
			Lockout: SettingsAuthLockout{
				DelayAfter: 5, LockAfter: 10, LockFor: MustDuration("15m"), MaxLock: MustDuration("24h"),
			},
		},
		Retention: SettingsRetention{Sessions: MustDuration("30d"), AuditLog: MustDuration("365d")},
	}
}
