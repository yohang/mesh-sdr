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
	// WFMDeemphasis is the broadcast FM de-emphasis time constant in µs
	// (DEM-016), pushed to the nodes in the desired state.
	// AudioCompression is the codec the receivers ask the nodes for
	// (DEM-010): adpcm (ADPCM IMA, 4:1) or pcm (16-bit PCM).
	AudioCompression string `toml:"audio_compression" env:"AUDIO_COMPRESSION" jsonschema:"enum=adpcm,enum=pcm" jsonschema_extras:"x-public=true,x-label=Audio compression" jsonschema_description:"Audio sent to the listeners: adpcm (ADPCM, a quarter of the bandwidth) or pcm (uncompressed 16-bit PCM). Applies to receivers opened from now on."`
	WFMDeemphasis    int    `toml:"wfm_deemphasis" env:"WFM_DEEMPHASIS" jsonschema:"enum=50,enum=75" jsonschema_extras:"x-label=WFM de-emphasis (µs)" jsonschema_description:"Broadcast FM de-emphasis time constant, in microseconds: 50 (Europe and most of the world) or 75 (Americas, South Korea). Nodes apply it to their WFM demodulators."`

	Receiver  SettingsReceiver  `toml:"receiver" envPrefix:"RECEIVER__" jsonschema:"description=Receiver identity and policies."`
	Privacy   SettingsPrivacy   `toml:"privacy" envPrefix:"PRIVACY__" jsonschema:"description=Privacy of client data."`
	UI        SettingsUI        `toml:"ui" envPrefix:"UI__" jsonschema:"description=Look and feel."`
	Bandplan  SettingsBandplan  `toml:"bandplan" envPrefix:"BANDPLAN__" jsonschema:"description=Band plan and region bookmark pack."`
	Waterfall SettingsWaterfall `toml:"waterfall" envPrefix:"WATERFALL__" jsonschema:"description=Waterfall defaults of the receiver (ADR 0026)."`
	Session   SettingsSession   `toml:"session" envPrefix:"SESSION__" jsonschema:"description=Session lifetimes."`
	Auth      SettingsAuth      `toml:"auth" envPrefix:"AUTH__" jsonschema:"description=Sign-in throttling."`
	Retention SettingsRetention `toml:"retention" envPrefix:"RETENTION__" jsonschema:"description=Retention of DB-backed stores."`
	Grid      SettingsGrid      `toml:"grid" envPrefix:"GRID__" jsonschema:"description=Node health (GRID-009)."`

	Invitations   SettingsInvitations   `toml:"invitations" envPrefix:"INVITATIONS__" jsonschema:"description=Invitations (ACC-002)."`
	PasswordReset SettingsPasswordReset `toml:"password_reset" envPrefix:"PASSWORD_RESET__" jsonschema:"description=Password reset links (ACC-003)."`
}

// SettingsReceiver is the [settings.receiver] table (ADM-003).
type SettingsReceiver struct {
	Name     string   `toml:"name" env:"NAME" jsonschema:"minLength=1,maxLength=64" jsonschema_extras:"x-public=true,x-label=Station name" jsonschema_description:"Station name, shown in the top bar and the page titles."`
	Location string   `toml:"location" env:"LOCATION" jsonschema:"maxLength=128" jsonschema_extras:"x-public=true,x-label=Location" jsonschema_description:"Station location, as free text (for example a town and a country)."`
	GPS      GeoPoint `toml:"gps" env:"GPS" jsonschema_extras:"x-public=true,x-label=Position" jsonschema_description:"Station position in decimal degrees (WGS 84). In the env: \"<lat>,<lon>\"."`
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
	ThemeMode   string `toml:"theme_mode" env:"THEME_MODE" jsonschema:"enum=light,enum=dark,enum=auto" jsonschema_extras:"x-public=true,x-label=Theme" jsonschema_description:"Theme mode (UI-001): light or dark, or auto to follow the visitor's prefers-color-scheme. A change applies on the next full page load."`
	ShortcutSet string `toml:"shortcut_set" env:"SHORTCUT_SET" jsonschema:"enum=default,enum=off" jsonschema_extras:"x-public=true,x-label=Keyboard shortcuts" jsonschema_description:"Keyboard shortcut set: default, or off to disable single-key shortcuts."`
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
		Receiver:         SettingsReceiver{Name: "MeshSDR", UsagePolicyURL: "/policy"},
		Privacy:          SettingsPrivacy{MaskIPs: true},
		UI: SettingsUI{
			ThemeMode: "auto", ShortcutSet: "default",
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
		},
		Grid:          SettingsGrid{HeartbeatIntervalS: 10, OfflineAfterS: 60},
		Invitations:   SettingsInvitations{TTLHours: 168},
		PasswordReset: SettingsPasswordReset{TTLMinutes: 30},
	}
}
