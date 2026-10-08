package settings

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// formPages are the admin pages made of setting forms. Every settings key
// belongs to exactly one section (checked by a test), so every key is
// editable in the UI.
var formPages = []formPage{
	{
		Section: "site", Title: "Site", Path: "/admin/site",
		Intro: "Station information shown to visitors (ADM-003).",
		Forms: []sectionSpec{
			{ID: "station", Title: "Station", Keys: []string{
				"receiver.name", "receiver.location", "receiver.gps",
			}},
			{ID: "contact", Title: "Help", Keys: []string{"receiver.help_url"}},
			{ID: "panorama", Title: "Panorama text", Keys: []string{"receiver.photo_title", "receiver.photo_desc"}},
			{ID: "policy", Title: "Usage policy", Keys: []string{"receiver.usage_policy_text", "receiver.usage_policy_url"}},
		},
	},
	{
		Section: "access", Title: "Access", Path: "/admin/access",
		Intro: "Who may listen, and how sign-in is protected (ADM-005).",
		Forms: []sectionSpec{
			{ID: "listening", Title: "Listening", Keys: []string{"listen_policy"}},
			{ID: "sessions", Title: "Sessions", Description: "New lifetimes apply to sessions opened from now on; activity extends open sessions with the new idle timeout.",
				Keys: []string{"session.idle_timeout", "session.absolute_timeout", "session.remember_me_timeout"}},
			{ID: "passwords", Title: "Passwords", Keys: []string{"auth.password_min_length"}},
			{ID: "links", Title: "Invitation and reset links", Description: "New lifetimes apply to links created from now on.",
				Keys: []string{"invitations.ttl_hours", "password_reset.ttl_minutes"}},
			{ID: "sign-in", Title: "Sign-in throttling", Keys: []string{
				"auth.login_rate_limit", "auth.lockout.delay_after", "auth.lockout.lock_after", "auth.lockout.lock_for", "auth.lockout.max_lock",
			}},
		},
	},
	{
		Section: "look-and-feel", Title: "Look & feel", Path: "/admin/look-and-feel",
		Intro: "The look and feel is the same for every visitor: there is no per-user theme (ADM-006, UI-001).",
		Forms: []sectionSpec{
			{ID: "theme", Title: "Theme", Description: "A change applies on the next full page load.", Keys: []string{"ui.theme_mode"}},
			{ID: "display", Title: "Display defaults", Keys: []string{"ui.shortcut_set"}},
			{ID: "waterfall", Title: "Waterfall", Description: "Levels and palette every receiver starts with; listeners can still set automatic levels.",
				Keys: []string{"waterfall.min_db", "waterfall.max_db", "waterfall.palette"}},
		},
	},
	{
		Section: "retention", Title: "Data & retention", Path: "/admin/retention",
		Intro: "How long each DB-backed store keeps its rows (ADM-011). Retention jobs apply the policies hourly or daily.",
		Forms: []sectionSpec{
			{ID: "retention", Title: "Retention policies", Keys: []string{
				"retention.sessions", "retention.audit_log", "retention.connections",
			}},
		},
	},
	{
		Section: "node-health", Title: "Node health", Path: "/admin/grid",
		Intro: "How often nodes report, when the hub marks a silent node degraded or offline (GRID-009), and the demodulation settings pushed to the nodes.",
		Forms: []sectionSpec{
			{ID: "heartbeats", Title: "Heartbeats", Description: "Nodes take a new heartbeat interval when their control channel reconnects.",
				Keys: []string{"grid.heartbeat_interval_s", "grid.offline_after_s"}},
			{ID: "demodulation", Title: "Demodulation", Description: "Nodes apply the de-emphasis to their running demodulators; the audio compression applies to receivers opened from now on.",
				Keys: []string{"audio_compression", "wfm_deemphasis"}},
		},
	},
}

// overview serves the admin landing page (ADM-001).
func (m *Module) overview(w http.ResponseWriter, r *http.Request) {
	snap := m.d.Store.Snapshot()

	var sum overviewSummary

	for _, e := range snap.All() {
		sum.Settings++

		switch e.Source() {
		case SourceConfig:
			sum.Locked++
		case SourceDB:
			sum.DB++
		case SourceDefault:
		}
	}

	stores, err := m.d.Retention.Stores(r.Context())
	if err != nil {
		m.d.Logger.WarnContext(r.Context(), "retention view unavailable", slog.Any("error", err))

		sum.JobsUnavailable = true
	}

	for _, s := range stores {
		if s.LastFailed {
			sum.FailedJobs = append(sum.FailedJobs, s.Label)
		}
	}

	if m.d.Schedules != nil {
		sum.ShowSchedules = true

		if sum.DisabledSchedules, err = m.d.Schedules.NeedingAttention(r.Context()); err != nil {
			m.d.Logger.WarnContext(r.Context(), "schedules unavailable", slog.Any("error", err))

			sum.SchedulesUnavailable = true
		}
	}

	m.page(w, r, http.StatusOK, "Administration", "overview", overviewPage(sum), nil)
}

// overviewSummary is the health summary of the landing page.
type overviewSummary struct {
	Settings, Locked, DB int
	FailedJobs           []string
	JobsUnavailable      bool
	ShowSchedules        bool
	DisabledSchedules    int
	SchedulesUnavailable bool
}

// system serves the effective configuration (ADM-010).
func (m *Module) system(w http.ResponseWriter, r *http.Request) {
	m.page(w, r, http.StatusOK, "System", "system", systemPage(m.d.Config.View()), nil)
}

// storesTable renders the stores of the retention view with a notice.
func (m *Module) storesTable(r *http.Request, notice string) templ.Component {
	rows, err := m.d.Retention.Stores(r.Context())
	if err != nil {
		m.d.Logger.ErrorContext(r.Context(), "retention view", slog.Any("error", err))

		return storesView(nil, "", "The stores could not be read. Try again later.")
	}

	return storesView(rows, notice, "")
}

// purge runs the retention job of a store now ("purge now").
func (m *Module) purge(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, FormBodyLimit)
	if err := r.ParseForm(); err != nil {
		m.d.Render.Error(w, r, http.StatusBadRequest)

		return
	}

	store := r.PostForm.Get("store")
	status, notice, failure := http.StatusOK, "", ""

	n, err := m.d.Retention.Purge(r.Context(), store)

	var de *shared.Error

	switch {
	case err == nil:
		notice = "Purged " + store + ": " + strconv.FormatInt(n, 10) + " rows deleted."
	case errors.As(err, &de) && de.Kind() == shared.KindNotFound:
		m.d.Render.Error(w, r, http.StatusNotFound)

		return
	case errors.As(err, &de) && de.Kind() == shared.KindConflict:
		status, failure = http.StatusConflict, "A purge of "+store+" is already running. Try again in a moment."
	default:
		m.d.Logger.ErrorContext(r.Context(), "purge now", slog.String("store", store), slog.Any("error", err))

		status, failure = http.StatusInternalServerError, "The purge of "+store+" failed. See the hub logs."
	}

	rows, lerr := m.d.Retention.Stores(r.Context())
	if lerr != nil {
		m.d.Logger.ErrorContext(r.Context(), "retention view", slog.Any("error", lerr))
	}

	fragment := storesView(rows, notice, failure)

	p := formPages[3]
	content := formPageView(p, []sectionView{buildSection(p.Forms[0], p.Path, m.d.Store.Snapshot())}, fragment)

	m.page(w, r, status, p.Title, p.Section, content, fragment)
}

// sourceText describes where a value comes from, for people.
func sourceText(source Source, origin string) string {
	switch source {
	case SourceConfig:
		return lockText(origin)
	case SourceDB:
		return "Set by an admin."
	default:
		return "Default value."
	}
}

func lockText(origin string) string {
	if v, ok := strings.CutPrefix(origin, "env:"); ok {
		return "Locked: set by the environment variable " + v + "."
	}

	return "Locked: set in the configuration file (" + origin + ")."
}

// entryValue is a config entry value as shown to people.
func entryValue(e ConfigEntry) string {
	if e.Secret {
		if e.Set {
			return "set (hidden)"
		}

		return "not set"
	}

	var list []string
	if err := json.Unmarshal(e.Value, &list); err == nil {
		if len(list) == 0 {
			return "none"
		}

		return strings.Join(list, ", ")
	}

	v, err := NewValue(e.Value)
	if err != nil {
		return ""
	}

	return displayValue(InputText, v)
}

func formatDuration(d time.Duration) string {
	const day = 24 * time.Hour

	if d > 0 && d%day == 0 {
		return strconv.FormatInt(int64(d/day), 10) + "d"
	}

	return d.String()
}

func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}

	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

func lastRun(r RetentionRow) string {
	switch {
	case r.Running:
		return "running"
	case r.LastFinished.IsZero():
		return "never"
	case r.LastFailed:
		return "failed at " + r.LastFinished.UTC().Format("2006-01-02 15:04 UTC") + ": " + r.LastError
	default:
		return r.LastFinished.UTC().Format("2006-01-02 15:04 UTC") + ", " + strconv.FormatInt(r.LastRows, 10) + " rows deleted"
	}
}

func joinComma(s []string) string { return strings.Join(s, ", ") }

// shorten bounds long values (the usage policy) in tables.
func shorten(s string) string {
	const maxRunes = 160

	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}

	return s
}
