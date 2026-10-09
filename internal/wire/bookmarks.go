package wire

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/yohang/mesh-sdr/internal/bookmarks"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// SyncBookmarks stores the shipped bookmark packs as the builtin rows of
// adapter (`meshsdr hub migrate`, after the migrations).
func SyncBookmarks(ctx context.Context, adapter *db.DB, logger *slog.Logger) (bookmarks.SyncResult, error) {
	m, err := bookmarks.New(bookmarks.Deps{DB: adapter, Now: time.Now, Logger: component(logger, "bookmarks.app.sync")})
	if err != nil {
		return bookmarks.SyncResult{}, err
	}

	return m.Sync(ctx)
}

// bookmarksDeps are the hub parts the bookmarks module uses.
type bookmarksDeps struct {
	adapter  *db.DB
	features *gridapp.Features
	presets  *presets.Service
	region   func() string
	audit    audit.Appender
	broker   events.Publisher
	policies *gridapp.ListenPolicies
	idm      *identityhttp.Module
	render   bookmarks.Renderer
	isAdmin  func(ctx context.Context) bool
	now      func() time.Time
	logger   *slog.Logger
}

// newBookmarks builds the bookmarks module of the hub.
func newBookmarks(d bookmarksDeps) (*bookmarks.Module, error) {
	m, err := bookmarks.New(bookmarks.Deps{
		DB: d.adapter, Audit: d.audit, Devices: bookmarkDevices{features: d.features},
		Presets: bookmarkPresets{presets: d.presets}, Region: d.region,
		CanListen: listenAs(d.policies, d.idm.Principal),
		User:      currentUser,
		Changed:   bookmarkChanged(d.broker, d.policies, component(d.logger, "wire.bookmarks")),
		Render:    d.render, Guard: d.idm.Require(identitydomain.RoleOperator),
		AdminSections: func(r *http.Request) []layout.AdminSection {
			if d.isAdmin(r.Context()) {
				return layout.AdminSections
			}

			return layout.OperatorAdminSections
		},
		IDs: shared.NewUUIDv7Generator(), Now: d.now, Logger: component(d.logger, "bookmarks.app"),
	})
	if err != nil {
		return nil, fmt.Errorf("bookmarks: %w", err)
	}

	return m, nil
}

// listenAs tells the bookmarks whether the caller of ctx may listen to a
// device (the listen policies).
func listenAs(p *gridapp.ListenPolicies, principal func(context.Context) identitydomain.Principal,
) func(context.Context, shared.DeviceID) (bool, error) {
	return func(ctx context.Context, device shared.DeviceID) (bool, error) {
		return p.CanListen(ctx, principal(ctx).IsAnonymous(), device.String())
	}
}

// bookmarkDevices gives the enabled devices to the bookmarks: the public
// feature summary (listen policy, modes, active preset).
type bookmarkDevices struct{ features *gridapp.Features }

// Devices implements bookmarks.Devices.
func (b bookmarkDevices) Devices(ctx context.Context) ([]bookmarks.Device, error) {
	summary, err := b.features.Summary(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]bookmarks.Device, 0, len(summary.Devices))
	for _, d := range summary.Devices {
		out = append(out, bookmarks.Device{ID: d.ID, Name: d.Name, ActivePreset: d.ActivePreset, Modes: d.Modes})
	}

	return out, nil
}

// bookmarkPresets gives the presets to the bookmarks (preset scopes).
type bookmarkPresets struct{ presets *presets.Service }

// Presets implements bookmarks.Presets.
func (b bookmarkPresets) Presets(ctx context.Context) ([]bookmarks.Preset, error) {
	list, err := b.presets.List(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]bookmarks.Preset, 0, len(list))
	for _, p := range list {
		out = append(out, bookmarks.Preset{ID: p.ID(), Name: p.Name()})
	}

	return out, nil
}

// bookmarkChangedEvent is the bookmark.changed payload (§6.6).
type bookmarkChangedEvent struct {
	Op       string                 `json:"op"`
	Bookmark bookmarks.BookmarkView `json:"bookmark"`
}

// bookmarkChanged publishes `bookmark.changed` on the devices topic after
// a committed change of a hub bookmark. A bookmark scoped to one device
// goes to the viewers who may listen to it (and staff); the others go to
// every subscriber of the topic.
func bookmarkChanged(b events.Publisher, policies *gridapp.ListenPolicies, logger *slog.Logger) func(context.Context, bookmarks.Change) {
	return func(ctx context.Context, c bookmarks.Change) {
		ev := events.Event{
			Topic: topicDevices, Type: rxv1.TypeBookmarkChanged.String(),
			Payload: bookmarkChangedEvent{Op: c.Op, Bookmark: bookmarks.View(c.Bookmark)},
		}

		if dev := c.Bookmark.Scope().Device(); !dev.IsZero() {
			ev.Audience = staff

			view, err := policies.View(ctx)
			if err != nil {
				logger.ErrorContext(ctx, "listen policies for bookmark.changed", slog.Any("error", err))
			} else {
				ev.Audience = func(v events.Viewer) bool { return v.Staff || view.CanListen(v.Anonymous(), dev.String()) }
			}
		}

		b.Publish(ctx, ev)
	}
}

// operatorLinks adds the Bookmarks › Manage link to the user menu of
// operators and admins.
func operatorLinks(user func(r *http.Request) *layout.User, operator func(ctx context.Context) bool) func(r *http.Request) *layout.User {
	return func(r *http.Request) *layout.User {
		u := user(r)
		if u != nil && operator(r.Context()) {
			u.Links = append(u.Links, layout.Link{Label: "Bookmarks", Href: bookmarks.ManagePath})
		}

		return u
	}
}
