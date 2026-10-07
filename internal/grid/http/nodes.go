package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/http/problem"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Admin › Nodes (GRID-005, GRID-009, GRID-015): the node registry with its
// health, certificates, devices, capabilities and load history, read by
// operators; adding, editing, disabling, re-enrolling, revoking and
// removing nodes is for admins. The forms are the only way to do it: there
// is no /api/v1 twin (ADR 0023).

// NodeAdmin is the node registry and its admin use cases (grid app.Nodes).
type NodeAdmin interface {
	List(ctx context.Context) ([]*domain.Node, error)
	Get(ctx context.Context, id string) (*domain.Node, error)
	Add(ctx context.Context, actor string, in app.NewNodeInput) (app.Issued, error)
	Update(ctx context.Context, actor, id string, name, url *string, disabled *bool, expectedVersion int) (*domain.Node, error)
	Delete(ctx context.Context, actor, id string) error
	Revoke(ctx context.Context, actor, id string) (*domain.Node, error)
	IssueToken(ctx context.Context, actor, id string) (app.Issued, error)
}

// LoadHistory gives the recent heartbeats of a node (RAM ring).
type LoadHistory interface {
	Samples(id domain.NodeID) []app.LoadSample
}

// CapabilityReports reads and refreshes the capability reports.
type CapabilityReports interface {
	Get(ctx context.Context, id string) (domain.CapabilityReport, error)
	Probe(ctx context.Context, id string) error
}

// ConnectionRegistry reads the open connections (presence registry).
type ConnectionRegistry interface {
	List(ctx context.Context) ([]*domain.Connection, error)
}

// certWarnAfter is the share of a node certificate's life after which the
// page warns: the hub renews at two thirds, so a certificate past it is
// not being renewed.
const certWarnAfter = 2.0 / 3

// nodeRow is one node of the list.
type nodeRow struct {
	Node      *domain.Node
	Devices   int
	Listeners int
	CertWarn  bool
}

// nodesView is the node list.
type nodesView struct {
	Rows     []nodeRow
	CanAdmin bool
}

// nodeForm is the edit form of a node.
type nodeForm struct {
	ID, Name, URL string
	Version       int
}

// platform is the host part of a capability report.
type platform struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUModel string `json:"cpu_model"`
	CPUCores int    `json:"cpu_cores"`
	RAMBytes uint64 `json:"ram_bytes"`
	Hostname string `json:"hostname"`
}

// capsView is the capability summary of a node.
type capsView struct {
	ProductVersion string
	Protocols      string
	Platform       platform
	ReportedAt     time.Time
	Capabilities   []domain.Capability
}

// nodeView is the detail page of a node.
type nodeView struct {
	Node      *domain.Node
	CanAdmin  bool
	Devices   []*domain.Device
	Listeners int
	Caps      *capsView
	Load      loadView
	CertWarn  bool
	Form      nodeForm
	Notice    string
	Failure   string
}

// tokenView shows an enrollment token, once.
type tokenView struct {
	Node          *domain.Node
	Token         string
	CAFingerprint string
	ExpiresAt     time.Time
	Added         bool
}

func (v nodeView) locked(field string) bool { return slices.Contains(v.Node.LockedFields(), field) }

func certWarn(n *domain.Node, now time.Time) bool {
	c := n.Certificate()
	if c.IsZero() || n.EnrolledAt().IsZero() {
		return false
	}

	// The certificate was issued at enrollment or at its last renewal;
	// node leaves live 90 days (ADR 0008).
	const life = 90 * 24 * time.Hour

	return c.NotAfter().Sub(now) < time.Duration(float64(life)*(1-certWarnAfter))
}

// nodesPage serves the node list; its table is the live fragment.
func (m *AdminModule) nodesPage(w http.ResponseWriter, r *http.Request) {
	v, err := m.nodesView(r)
	if err != nil {
		m.d.Logger.ErrorContext(r.Context(), "list nodes", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	m.page(w, r, http.StatusOK, "Nodes", "nodes", nodesPage(v), nodesTable(v))
}

func (m *AdminModule) nodesView(r *http.Request) (nodesView, error) {
	ctx := r.Context()

	nodes, err := m.d.Nodes.List(ctx)
	if err != nil {
		return nodesView{}, err
	}

	devices, err := m.d.Devices.List(ctx)
	if err != nil {
		return nodesView{}, err
	}

	conns, err := m.d.Connections.List(ctx)
	if err != nil {
		return nodesView{}, err
	}

	perNode := map[string]int{}
	for _, d := range devices {
		perNode[d.Node().String()]++
	}

	listeners := map[string]int{}

	for _, c := range conns {
		if i := c.Info(); i.Kind == domain.ConnectionMedia && i.NodeID != "" {
			listeners[i.NodeID]++
		}
	}

	now := m.now()
	v := nodesView{CanAdmin: m.d.IsAdmin(r)}

	for _, n := range nodes {
		id := n.ID().String()
		v.Rows = append(v.Rows, nodeRow{Node: n, Devices: perNode[id], Listeners: listeners[id], CertWarn: certWarn(n, now)})
	}

	return v, nil
}

// nodeView reads the node of the path; status is 404 or 500 on failure.
func (m *AdminModule) nodeView(r *http.Request) (nodeView, int) {
	ctx := r.Context()

	n, err := m.d.Nodes.Get(ctx, chi.URLParam(r, "id"))

	switch {
	case errors.Is(err, domain.ErrNodeNotFound):
		return nodeView{}, http.StatusNotFound
	case err != nil:
		m.d.Logger.ErrorContext(ctx, "get node", slog.Any("error", err))

		return nodeView{}, http.StatusInternalServerError
	}

	v := nodeView{
		Node: n, CanAdmin: m.d.IsAdmin(r), CertWarn: certWarn(n, m.now()),
		Load: newLoadView(m.d.History.Samples(n.ID())),
		Form: nodeForm{ID: n.ID().String(), Name: n.Name().String(), URL: n.URL().String(), Version: n.Version()},
	}

	if v.Devices, err = m.d.Devices.ListByNode(ctx, n.ID()); err != nil {
		m.d.Logger.ErrorContext(ctx, "list node devices", slog.Any("error", err))

		return nodeView{}, http.StatusInternalServerError
	}

	conns, err := m.d.Connections.List(ctx)
	if err != nil {
		m.d.Logger.ErrorContext(ctx, "list connections", slog.Any("error", err))

		return nodeView{}, http.StatusInternalServerError
	}

	for _, c := range conns {
		if i := c.Info(); i.Kind == domain.ConnectionMedia && i.NodeID == n.ID().String() {
			v.Listeners++
		}
	}

	rep, err := m.d.Capabilities.Get(ctx, n.ID().String())

	switch {
	case err == nil:
		c := &capsView{
			ProductVersion: rep.ProductVersion(), Protocols: strings.Join(rep.Protocols(), ", "),
			ReportedAt: rep.ReportedAt(), Capabilities: rep.Capabilities(),
		}
		_ = json.Unmarshal(rep.Platform(), &c.Platform)
		v.Caps = c
	case !errors.Is(err, domain.ErrCapabilitiesNotReported):
		m.d.Logger.ErrorContext(ctx, "get node capabilities", slog.Any("error", err))
	}

	return v, http.StatusOK
}

func (m *AdminModule) renderNode(w http.ResponseWriter, r *http.Request, status int, v nodeView) {
	m.page(w, r, status, "Node "+v.Node.Name().String(), "nodes", nodePage(v), nodeLive(v))
}

// nodePage serves the detail of a node; its status part is the live
// fragment.
func (m *AdminModule) nodePage(w http.ResponseWriter, r *http.Request) {
	v, status := m.nodeView(r)
	if status != http.StatusOK {
		m.d.Render.Error(w, r, status)

		return
	}

	m.renderNode(w, r, http.StatusOK, v)
}

// newNodePage serves the form that adds a node (GRID-005).
func (m *AdminModule) newNodePage(w http.ResponseWriter, r *http.Request) {
	m.page(w, r, http.StatusOK, "Add a node", "nodes", newNodePage(nodeForm{}, ""), nil)
}

// failure maps an error of a node action to a status and a message for
// people; unexpected errors are logged.
func (m *AdminModule) failure(ctx context.Context, action string, err error) (int, string) {
	var de *shared.Error

	switch {
	case errors.Is(err, domain.ErrVersionConflict):
		return http.StatusConflict, "The node changed since this page was loaded: review it and try again."
	case errors.Is(err, app.ErrGridDisabled):
		return http.StatusServiceUnavailable, "The hub internal CA is not configured (tls.ca_cert): nodes cannot be enrolled."
	case errors.As(err, &de):
		return problem.StatusOf(de.Kind()), sentence(de.Message())
	}

	m.d.Logger.ErrorContext(ctx, action, slog.Any("error", err))

	return http.StatusInternalServerError, "The action failed. Try again later, or see the hub logs."
}

// sentence capitalises a domain message and ends it with a full stop.
func sentence(s string) string {
	if s == "" {
		return s
	}

	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}

	return s
}

// addNode declares a node and shows its enrollment token once.
func (m *AdminModule) addNode(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}

	f := nodeForm{ID: strings.TrimSpace(r.PostForm.Get("id")), Name: strings.TrimSpace(r.PostForm.Get("name")), URL: strings.TrimSpace(r.PostForm.Get("url"))}

	issued, err := m.d.Nodes.Add(r.Context(), app.ActorUser, app.NewNodeInput{ID: f.ID, Name: f.Name, URL: f.URL})
	if err != nil {
		status, msg := m.failure(r.Context(), "add node", err)
		m.page(w, r, status, "Add a node", "nodes", newNodePage(f, msg), nil)

		return
	}

	m.showToken(w, r, issued, true)
}

func (m *AdminModule) showToken(w http.ResponseWriter, r *http.Request, issued app.Issued, added bool) {
	m.page(w, r, http.StatusOK, "Enrollment token", "nodes", tokenPage(tokenView{
		Node: issued.Node, Token: issued.Token.String(), CAFingerprint: issued.CAFingerprint, ExpiresAt: issued.ExpiresAt, Added: added,
	}), nil)
}

// nodeAction runs an admin action on the node of the path, then shows its
// page again with the outcome.
func (m *AdminModule) nodeAction(w http.ResponseWriter, r *http.Request, action string, run func(id string) (string, error)) {
	if !parseForm(w, r) {
		return
	}

	id := chi.URLParam(r, "id")

	msg, err := run(id)

	v, status := m.nodeView(r)
	if status != http.StatusOK {
		m.d.Render.Error(w, r, status)

		return
	}

	status = http.StatusOK
	if err != nil {
		status, v.Failure = m.failure(r.Context(), action, err)
	} else {
		v.Notice = msg
	}

	m.renderNode(w, r, status, v)
}

func (m *AdminModule) editNode(w http.ResponseWriter, r *http.Request) {
	m.nodeAction(w, r, "update node", func(id string) (string, error) {
		version, err := strconv.Atoi(r.PostForm.Get("version"))
		if err != nil {
			return "", domain.ErrVersionConflict
		}

		var name, url *string

		if v, ok := r.PostForm["name"]; ok {
			s := strings.TrimSpace(v[0])
			name = &s
		}

		if v, ok := r.PostForm["url"]; ok {
			s := strings.TrimSpace(v[0])
			url = &s
		}

		_, err = m.d.Nodes.Update(r.Context(), app.ActorUser, id, name, url, nil, version)

		return "The node is saved.", err
	})
}

func (m *AdminModule) setDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.nodeAction(w, r, "update node", func(id string) (string, error) {
			n, err := m.d.Nodes.Get(r.Context(), id)
			if err != nil {
				return "", err
			}

			if _, err := m.d.Nodes.Update(r.Context(), app.ActorUser, id, nil, nil, &disabled, n.Version()); err != nil {
				return "", err
			}

			if disabled {
				return "The node is disabled: the hub closed its control channel and its listeners can no longer connect.", nil
			}

			return "The node is enabled: the hub connects to it again.", nil
		})
	}
}

func (m *AdminModule) probeNode(w http.ResponseWriter, r *http.Request) {
	m.nodeAction(w, r, "probe node", func(id string) (string, error) {
		return "The node was asked to report its capabilities again: reload this page in a moment.", m.d.Capabilities.Probe(r.Context(), id)
	})
}

func (m *AdminModule) revokeNode(w http.ResponseWriter, r *http.Request) {
	m.nodeAction(w, r, "revoke node", func(id string) (string, error) {
		_, err := m.d.Nodes.Revoke(r.Context(), app.ActorUser, id)

		return "The node certificate is revoked: issue a new enrollment token to enroll the node again.", err
	})
}

// issueToken re-enrolls a node: its certificate is revoked and a new token
// is shown once.
func (m *AdminModule) issueToken(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}

	issued, err := m.d.Nodes.IssueToken(r.Context(), app.ActorUser, chi.URLParam(r, "id"))
	if err == nil {
		m.showToken(w, r, issued, false)

		return
	}

	v, status := m.nodeView(r)
	if status != http.StatusOK {
		m.d.Render.Error(w, r, status)

		return
	}

	status, v.Failure = m.failure(r.Context(), "issue enrollment token", err)
	m.renderNode(w, r, status, v)
}

func (m *AdminModule) deleteNode(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}

	err := m.d.Nodes.Delete(r.Context(), app.ActorUser, chi.URLParam(r, "id"))
	if err == nil {
		redirect(w, r, "/admin/nodes")

		return
	}

	v, status := m.nodeView(r)
	if status != http.StatusOK {
		m.d.Render.Error(w, r, status)

		return
	}

	status, v.Failure = m.failure(r.Context(), "delete node", err)
	m.renderNode(w, r, status, v)
}

// redirect sends the browser to path after an action: htmx follows
// HX-Redirect, other clients a 303.
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", path)
		w.WriteHeader(http.StatusNoContent)

		return
	}

	http.Redirect(w, r, path, http.StatusSeeOther)
}

// formBodyLimit bounds the forms of the node pages.
const formBodyLimit = 16 << 10

func parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, formBodyLimit)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)

		return false
	}

	return true
}
