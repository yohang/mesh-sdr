package api

import (
	"context"
	"time"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

// NodeAdmin is the node registry seen by the REST API.
type NodeAdmin interface {
	List(ctx context.Context) ([]*domain.Node, error)
	Get(ctx context.Context, id string) (*domain.Node, error)
	Add(ctx context.Context, actor string, in gridapp.NewNodeInput) (gridapp.Issued, error)
	Update(ctx context.Context, actor, id string, name, url *string, disabled *bool, expectedVersion int) (*domain.Node, error)
	Delete(ctx context.Context, actor, id string) error
	IssueToken(ctx context.Context, actor, id string) (gridapp.Issued, error)
}

// LoadHistory returns the recent heartbeats of a node.
type LoadHistory interface {
	Samples(id domain.NodeID) []gridapp.LoadSample
}

// GridHandlers serve the grid endpoints (nodes, devices, connections).
// Access is enforced by the x-meshsdr-access policy of openapi.yaml.
type GridHandlers struct {
	nodes   NodeAdmin
	history LoadHistory
}

// NewGridHandlers returns the handlers.
func NewGridHandlers(nodes NodeAdmin, history LoadHistory) GridHandlers {
	return GridHandlers{nodes: nodes, history: history}
}

func optString(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}

	return &t
}

func nodeDTO(n *domain.Node) Node {
	s := n.Snapshot()
	rt := s.Runtime

	health := NodeHealth(rt.Status)

	switch s.Enrollment {
	case domain.EnrollmentPending:
		health = "enrolling"
	case domain.EnrollmentRevoked:
		health = "revoked"
	case domain.EnrollmentEnrolled:
	}

	out := Node{
		Id: s.ID, Name: s.Name, Url: s.URL, Origin: NodeOrigin(s.Origin), LockedFields: s.LockedFields,
		Disabled: s.Disabled, EnrollmentState: NodeEnrollmentState(s.Enrollment), Health: health,
		Status: NodeStatus(rt.Status), StatusHint: optString(rt.StatusHint),
		EnrollmentExpiresAt: optTime(s.KeyExpiresAt), EnrolledAt: optTime(s.EnrolledAt),
		CertSerial: optString(s.CertSerial), CertNotAfter: optTime(s.CertNotAfter),
		LastHeartbeatAt: optTime(rt.LastHeartbeatAt), SoftwareVersion: optString(rt.SoftwareVersion),
		ProtocolVersion: optString(rt.ProtocolVersion), Hostname: optString(rt.Hostname),
		ClockOffsetMs: rt.ClockOffsetMS, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, Version: s.Version,
	}

	if c := n.Certificate(); !c.IsZero() {
		out.CertFingerprint = optString(c.FingerprintString())
	}

	if rt.CPUCores > 0 {
		c := rt.CPUCores
		out.CpuCores = &c
	}

	return out
}

func issuedDTO(i gridapp.Issued) NodeEnrollment {
	return NodeEnrollment{
		Node: nodeDTO(i.Node), EnrollmentToken: i.Token.String(), CaFingerprint: i.CAFingerprint, ExpiresAt: optTime(i.ExpiresAt),
	}
}

// actor is the audit actor of REST calls (no user identity yet).
const actor = gridapp.ActorUser

// ListNodes implements StrictServerInterface.
func (h GridHandlers) ListNodes(ctx context.Context, _ ListNodesRequestObject) (ListNodesResponseObject, error) {
	nodes, err := h.nodes.List(ctx)
	if err != nil {
		return nil, err
	}

	out := ListNodes200JSONResponse{Items: make([]Node, 0, len(nodes))}
	for _, n := range nodes {
		out.Items = append(out.Items, nodeDTO(n))
	}

	return out, nil
}

// CreateNode implements StrictServerInterface.
func (h GridHandlers) CreateNode(ctx context.Context, req CreateNodeRequestObject) (CreateNodeResponseObject, error) {
	in := gridapp.NewNodeInput{ID: req.Body.Id, URL: req.Body.Url}
	if req.Body.Name != nil {
		in.Name = *req.Body.Name
	}

	issued, err := h.nodes.Add(ctx, actor, in)
	if err != nil {
		return nil, err
	}

	return CreateNode201JSONResponse(issuedDTO(issued)), nil
}

// GetNode implements StrictServerInterface.
func (h GridHandlers) GetNode(ctx context.Context, req GetNodeRequestObject) (GetNodeResponseObject, error) {
	n, err := h.nodes.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}

	samples := h.history.Samples(n.ID())
	out := GetNode200JSONResponse{Node: nodeDTO(n), LoadHistory: make([]LoadSample, 0, len(samples))}

	for _, s := range samples {
		out.LoadHistory = append(out.LoadHistory, LoadSample{
			At: s.At, Cpu: s.CPU, Load1: s.Load1, TempC: s.TempC,
			MemAvailableBytes: int64(min(s.MemAvailableBytes, 1<<62)), MemTotalBytes: int64(min(s.MemTotalBytes, 1<<62)),
		})
	}

	return out, nil
}

// UpdateNode implements StrictServerInterface.
func (h GridHandlers) UpdateNode(ctx context.Context, req UpdateNodeRequestObject) (UpdateNodeResponseObject, error) {
	b := req.Body

	n, err := h.nodes.Update(ctx, actor, req.Id, b.Name, b.Url, b.Disabled, b.Version)
	if err != nil {
		return nil, err
	}

	return UpdateNode200JSONResponse(nodeDTO(n)), nil
}

// DeleteNode implements StrictServerInterface.
func (h GridHandlers) DeleteNode(ctx context.Context, req DeleteNodeRequestObject) (DeleteNodeResponseObject, error) {
	if err := h.nodes.Delete(ctx, actor, req.Id); err != nil {
		return nil, err
	}

	return DeleteNode204Response{}, nil
}

// IssueNodeEnrollmentToken implements StrictServerInterface.
func (h GridHandlers) IssueNodeEnrollmentToken(ctx context.Context, req IssueNodeEnrollmentTokenRequestObject) (IssueNodeEnrollmentTokenResponseObject, error) {
	issued, err := h.nodes.IssueToken(ctx, actor, req.Id)
	if err != nil {
		return nil, err
	}

	return IssueNodeEnrollmentToken200JSONResponse(issuedDTO(issued)), nil
}
