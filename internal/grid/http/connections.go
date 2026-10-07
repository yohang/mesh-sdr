package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// UserNames gives the display names of users (identity); unknown ids are
// left out.
type UserNames interface {
	Names(ctx context.Context, ids []shared.UUID) (map[shared.UUID]string, error)
}

// connectionRow is one open connection of Admin › Connections.
type connectionRow struct {
	Kind     string
	User     string
	IP       string
	Where    string
	OpenedAt time.Time
	LastSeen time.Time
}

// connectionsView is Admin › Connections.
type connectionsView struct {
	Rows      []connectionRow
	Listeners int
	Viewers   int
}

func kindText(k domain.ConnectionKind) string {
	switch k {
	case domain.ConnectionMedia:
		return "listener"
	case domain.ConnectionMap:
		return "map viewer"
	case domain.ConnectionEvents:
		return "page"
	}

	return string(k)
}

// connectionsPage lists the open connections of the presence registry
// across every node (GRID-017; PRS-001 and GRID-022 complete it in M1). The
// table refetches itself when the listener count changes.
func (m *AdminModule) connectionsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	conns, err := m.d.Connections.List(ctx)
	if err != nil {
		m.d.Logger.ErrorContext(ctx, "list connections", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	ids := make([]shared.UUID, 0, len(conns))

	for _, c := range conns {
		if u := c.Info().UserID; !u.IsZero() {
			ids = append(ids, u)
		}
	}

	names := map[shared.UUID]string{}

	if len(ids) > 0 && m.d.Users != nil {
		if names, err = m.d.Users.Names(ctx, ids); err != nil {
			m.d.Logger.WarnContext(ctx, "user names of connections", slog.Any("error", err))

			names = map[shared.UUID]string{}
		}
	}

	var v connectionsView

	for _, c := range conns {
		i := c.Info()
		row := connectionRow{Kind: kindText(i.Kind), User: "anonymous", IP: orDash(i.IP), OpenedAt: c.OpenedAt(), LastSeen: c.LastHeartbeat()}

		if !i.UserID.IsZero() {
			row.User = names[i.UserID]
			if row.User == "" {
				row.User = "deleted user"
			}
		}

		switch {
		case i.NodeID != "" && i.DeviceID != "":
			row.Where = i.NodeID + " / " + i.DeviceID
		case i.NodeID != "":
			row.Where = i.NodeID
		case i.DeviceID != "":
			row.Where = i.DeviceID
		default:
			row.Where = "—"
		}

		if i.Mode != "" {
			row.Where += " (" + i.Mode + ")"
		}

		if i.Kind == domain.ConnectionMedia {
			v.Listeners++
		} else {
			v.Viewers++
		}

		v.Rows = append(v.Rows, row)
	}

	m.page(w, r, http.StatusOK, "Connections", "connections", connectionsPage(v), connectionsTable(v))
}
