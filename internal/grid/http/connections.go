package http

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// ActionRevealIP is the audit action of "Reveal" on a masked address
// (PRS-001).
const ActionRevealIP = "connections.ip.reveal"

// PresetBand describes a preset for the connections page.
type PresetBand struct {
	Name                 string
	CenterFreq, SampRate int64
}

// MaskIP masks a client address (privacy.mask_ips): IPv4 to its /24
// network ("192.0.2.x"), IPv6 to its /48 prefix ("2001:db8:1::/48"). An
// address that does not parse is not shown at all.
func MaskIP(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "—"
	}

	a = a.Unmap().WithZone("")

	if a.Is4() {
		b := a.As4()

		return fmt.Sprintf("%d.%d.%d.x", b[0], b[1], b[2])
	}

	p, err := a.Prefix(48)
	if err != nil {
		return "—"
	}

	return p.String()
}

// UserNames gives the display names of users (identity); unknown ids are
// left out.
type UserNames interface {
	Names(ctx context.Context, ids []shared.UUID) (map[shared.UUID]string, error)
}

// connectionRow is one open connection of Admin › Connections.
type connectionRow struct {
	ID       string
	Kind     string
	User     string
	IP       string
	Masked   bool // IP is masked: the row offers "Reveal"
	Device   string
	Preset   string
	Band     string
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
		return "receiver"
	case domain.ConnectionMap:
		return "map"
	case domain.ConnectionEvents:
		return "events"
	}

	return string(k)
}

// maskIPs reports whether addresses are masked (privacy.mask_ips, on when
// the setting cannot be read).
func (m *AdminModule) maskIPs(ctx context.Context) bool {
	return m.d.MaskIPs == nil || m.d.MaskIPs(ctx)
}

// connectionsView reads the open connections. revealed is the id of the
// row whose address is shown in full ("" none).
func (m *AdminModule) connectionsView(ctx context.Context, revealed string) (connectionsView, error) {
	conns, err := m.d.Connections.List(ctx)
	if err != nil {
		return connectionsView{}, err
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

	var (
		v       connectionsView
		mask    = m.maskIPs(ctx)
		devices = map[string]*domain.Device{}
	)

	for _, c := range conns {
		i := c.Info()
		row := connectionRow{
			ID: i.ID.String(), Kind: kindText(i.Kind), User: "anonymous", IP: orDash(i.IP),
			Device: "—", Preset: "—", Band: "—", OpenedAt: c.OpenedAt(), LastSeen: c.LastHeartbeat(),
		}

		if mask && i.IP != "" && i.ID.String() != revealed {
			row.IP, row.Masked = MaskIP(i.IP), true
		}

		if !i.UserID.IsZero() {
			row.User = names[i.UserID]
			if row.User == "" {
				row.User = "deleted user"
			}
		}

		switch {
		case i.NodeID != "" && i.DeviceID != "":
			row.Device = i.NodeID + " / " + i.DeviceID
		case i.NodeID != "":
			row.Device = i.NodeID
		case i.DeviceID != "":
			row.Device = i.DeviceID
		}

		if i.Mode != "" {
			row.Device += " (" + i.Mode + ")"
		}

		row.Preset, row.Band = m.presetOf(ctx, i.DeviceID, devices)
		if row.Band == "—" {
			row.Band = m.bandOf(ctx, c, devices)
		}

		if i.Kind == domain.ConnectionMedia {
			v.Listeners++
		} else {
			v.Viewers++
		}

		v.Rows = append(v.Rows, row)
	}

	return v, nil
}

// presetOf names the active preset of a device and its band; "—" when
// unknown.
func (m *AdminModule) presetOf(ctx context.Context, deviceID string, cache map[string]*domain.Device) (string, string) {
	if deviceID == "" || m.d.PresetBand == nil {
		return "—", "—"
	}

	d := m.deviceOf(ctx, deviceID, cache)
	if d == nil || d.ActivePreset().IsZero() {
		return "—", "—"
	}

	p, ok := m.d.PresetBand(ctx, d.ActivePreset())
	if !ok {
		return "—", "—"
	}

	return p.Name, layout.FormatHz(p.CenterFreq) + " (" + layout.FormatHz(p.SampRate) + " wide)"
}

// bandOf names the band plan band (bandplan.region) of a connection without
// a preset band: the band of its tuned frequency when the node reported
// one, else of its device's centre frequency; "—" outside every band.
func (m *AdminModule) bandOf(ctx context.Context, c *domain.Connection, cache map[string]*domain.Device) string {
	if m.d.BandAt == nil {
		return "—"
	}

	hz := c.Snapshot().TunedFreq
	if hz == nil {
		if d := m.deviceOf(ctx, c.Info().DeviceID, cache); d != nil {
			hz = d.CenterFreq()
		}
	}

	if hz == nil {
		return "—"
	}

	if name := m.d.BandAt(ctx, *hz); name != "" {
		return name
	}

	return "—"
}

// deviceOf reads a device once per page; nil when unknown.
func (m *AdminModule) deviceOf(ctx context.Context, deviceID string, cache map[string]*domain.Device) *domain.Device {
	if deviceID == "" {
		return nil
	}

	d, ok := cache[deviceID]
	if !ok {
		var err error
		if d, err = m.d.Devices.Get(ctx, deviceID); err != nil {
			d = nil
		}

		cache[deviceID] = d
	}

	return d
}

// connectionsPage lists the open connections of the presence registry
// across every node (GRID-017, PRS-001). The table refetches itself when
// the listener count changes.
func (m *AdminModule) connectionsPage(w http.ResponseWriter, r *http.Request) {
	v, err := m.connectionsView(r.Context(), "")
	if err != nil {
		m.d.Logger.ErrorContext(r.Context(), "list connections", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	m.page(w, r, http.StatusOK, "Connections", "connections", connectionsPage(v), connectionsTable(v))
}

// revealIP shows the full address of one connection (PRS-001): admin only,
// logged to the audit log before the address is sent. The row stays
// revealed until the table refreshes.
func (m *AdminModule) revealIP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, err := shared.ParseUUID(chi.URLParam(r, "id"))
	if err != nil {
		m.d.Render.Error(w, r, http.StatusNotFound)

		return
	}

	conns, err := m.d.Connections.List(ctx)
	if err != nil {
		m.d.Logger.ErrorContext(ctx, "list connections", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	found := false

	for _, c := range conns {
		if c.Info().ID == id {
			found = true

			break
		}
	}

	if !found {
		m.d.Render.Error(w, r, http.StatusNotFound)

		return
	}

	if err := m.d.Audit.Append(ctx, audit.Record{Action: ActionRevealIP, TargetType: "connection", TargetID: id.String()}); err != nil {
		m.d.Logger.ErrorContext(ctx, "audit ip reveal", slog.String("connection_id", id.String()), slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	v, err := m.connectionsView(ctx, id.String())
	if err != nil {
		m.d.Logger.ErrorContext(ctx, "list connections", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	m.page(w, r, http.StatusOK, "Connections", "connections", connectionsPage(v), connectionsTable(v))
}
