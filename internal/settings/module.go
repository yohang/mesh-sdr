package settings

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/web/layout"
)

//go:generate go tool templ generate

// FormBodyLimit bounds the body of the admin forms (the usage policy is the
// largest field: 20 000 characters).
const FormBodyLimit = 256 << 10

// RetentionRow is one store of the retention view.
type RetentionRow struct {
	Store, Label, SettingKey string
	Retention                time.Duration
	// Policy describes a retention that is not one duration (files);
	// "" shows Retention.
	Policy       string
	Rows         int64
	Bytes        int64
	Sized        bool
	Running      bool
	LastFinished time.Time
	LastFailed   bool
	LastError    string
	LastRows     int64
}

// Retention is the retention view and "purge now".
type Retention interface {
	Stores(ctx context.Context) ([]RetentionRow, error)
	Purge(ctx context.Context, store string) (int64, error)
}

// Renderer renders pages in the app shell (internal/web/render).
type Renderer interface {
	Page(w http.ResponseWriter, r *http.Request, status int, page layout.Page, content, fragment templ.Component)
	Error(w http.ResponseWriter, r *http.Request, status int)
}

// Deps are the dependencies of the module.
type Deps struct {
	Render    Renderer
	Guard     func(http.Handler) http.Handler // admin role and network (identity)
	Store     *Store
	Config    *EffectiveConfig
	Retention Retention
	User      func(ctx context.Context) shared.UUID // signed-in user of a request
	Images    ImagesSection                         // receiver images of the Site page; nil: none
	// Schedules counts the schedules the hub disabled, for the overview;
	// nil: not shown.
	Schedules ScheduleHealth
	Logger    *slog.Logger
}

// ScheduleHealth counts the schedules the hub disabled (GRID-016): they
// need an admin.
type ScheduleHealth interface {
	NeedingAttention(ctx context.Context) (int, error)
}

// ImagesSection renders the receiver images section of the Site page.
type ImagesSection interface {
	Section(r *http.Request) templ.Component
}

// Module is the admin pages router module (internal/http.Module).
type Module struct{ d Deps }

// New returns the module.
func New(d Deps) *Module { return &Module{d: d} }

// Middlewares implements internal/http.Module.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module.
func (m *Module) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(m.d.Guard, noIndex)

		get := func(pattern string, h http.HandlerFunc) {
			r.Get(pattern, h)
			r.Head(pattern, h)
		}

		get("/admin", m.overview)
		get("/admin/system", m.system)
		r.Post("/admin/retention/purge", m.purge)

		for _, p := range formPages {
			get(p.Path, m.formPage(p))
			r.Post(p.Path, m.save(p))
		}
	})
}

func noIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex")
		next.ServeHTTP(w, r)
	})
}

// formPage is an admin page made of form sections.
type formPage struct {
	Section string // admin section id (layout.AdminSections)
	Title   string
	Intro   string
	Path    string
	Forms   []sectionSpec
}

func (m *Module) page(w http.ResponseWriter, r *http.Request, status int, title, section string, content, fragment templ.Component) {
	m.d.Render.Page(w, r, status, layout.Page{Title: title, Section: layout.SectionAdmin},
		layout.AdminPage(section, content), fragment)
}

// formPage serves a page of form sections.
func (m *Module) formPage(p formPage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.page(w, r, http.StatusOK, p.Title, p.Section, m.formPageContent(r, p, nil), nil)
	}
}

func (m *Module) formPageContent(r *http.Request, p formPage, result *sectionView) templ.Component {
	snap := m.d.Store.Snapshot()
	views := make([]sectionView, 0, len(p.Forms))

	for _, spec := range p.Forms {
		if result != nil && result.Spec.ID == spec.ID {
			views = append(views, *result)

			continue
		}

		views = append(views, buildSection(spec, p.Path, snap))
	}

	var extra templ.Component

	switch p.Section {
	case "site":
		if m.d.Images != nil {
			extra = m.d.Images.Section(r)
		}
	case "access":
		extra = adminNetworks(configEntry(m.d.Config.View(), "admin.allowed_networks"))
	case "retention":
		extra = m.storesTable(r, "")
	}

	return formPageView(p, views, extra)
}

func configEntry(v ConfigView, key string) ConfigEntry {
	for _, e := range v.Entries {
		if e.Key == key {
			return e
		}
	}

	return ConfigEntry{Key: key}
}

// save handles the submission of one section of a page.
func (m *Module) save(p formPage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, FormBodyLimit)
		if err := r.ParseForm(); err != nil {
			status := http.StatusBadRequest

			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				status = http.StatusRequestEntityTooLarge
			}

			m.d.Render.Error(w, r, status)

			return
		}

		var spec *sectionSpec

		for i := range p.Forms {
			if p.Forms[i].ID == r.PostForm.Get("section") {
				spec = &p.Forms[i]
			}
		}

		if spec == nil {
			m.d.Render.Error(w, r, http.StatusBadRequest)

			return
		}

		snap := m.d.Store.Snapshot()
		view, changes := parseSection(buildSection(*spec, p.Path, snap), snap, r.PostForm)
		status := http.StatusOK

		switch {
		case hasFieldErrors(view):
			status = http.StatusUnprocessableEntity
		case len(changes) == 0:
			view.Notice = "No changes to save."
		default:
			status = m.apply(r, &view, *spec, p.Path, changes)
		}

		fieldProblems(&view)

		m.page(w, r, status, p.Title, p.Section, m.formPageContent(r, p, &view), sectionForm(view))
	}
}

func (m *Module) apply(r *http.Request, view *sectionView, spec sectionSpec, action string, changes []Change) int {
	set, err := NewChangeSet(changes...)
	if err == nil {
		var snap *Snapshot

		if snap, err = m.d.Store.Apply(r.Context(), m.d.User(r.Context()), set); err == nil {
			*view = buildSection(spec, action, snap)
			view.Notice = "Saved."

			return http.StatusOK
		}
	}

	applyErrors(view, err)

	switch {
	case errors.Is(err, ErrInvalidSetting):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrVersionConflict), errors.Is(err, ErrSettingLocked), errors.Is(err, ErrSecretsUnavailable):
		return http.StatusConflict
	default:
		m.d.Logger.ErrorContext(r.Context(), "save settings", slog.String("section", spec.ID), slog.Any("error", err))

		return http.StatusInternalServerError
	}
}

func hasFieldErrors(v sectionView) bool {
	for _, f := range v.Fields {
		if f.Error != "" {
			return true
		}
	}

	return false
}
