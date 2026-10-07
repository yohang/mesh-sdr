package api

import (
	"context"
	"encoding/json"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	idomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// NodeAdmin is the node registry seen by the REST API.
type NodeAdmin interface {
	List(ctx context.Context) ([]*domain.Node, error)
	Get(ctx context.Context, id string) (*domain.Node, error)
	Add(ctx context.Context, actor string, in gridapp.NewNodeInput) (gridapp.Issued, error)
	Update(ctx context.Context, actor, id string, name, url *string, disabled *bool, expectedVersion int) (*domain.Node, error)
	Delete(ctx context.Context, actor, id string) error
	IssueToken(ctx context.Context, actor, id string) (gridapp.Issued, error)
	Revoke(ctx context.Context, actor, id string) (*domain.Node, error)
}

// CapabilityReports reads and refreshes capability reports.
type CapabilityReports interface {
	Get(ctx context.Context, id string) (domain.CapabilityReport, error)
	Probe(ctx context.Context, id string) error
}

// DeviceRegistry reads the device registry and forgets missing devices.
type DeviceRegistry interface {
	List(ctx context.Context) ([]*domain.Device, error)
	Get(ctx context.Context, id string) (*domain.Device, error)
	Forget(ctx context.Context, actor, id string) error
}

// ConnectionRegistry reads the presence registry.
type ConnectionRegistry interface {
	Count(ctx context.Context) (int, error)
	// Listeners counts the open media connections.
	Listeners(ctx context.Context) (int, error)
	List(ctx context.Context) ([]*domain.Connection, error)
}

// LoadHistory returns the recent heartbeats of a node.
type LoadHistory interface {
	Samples(id domain.NodeID) []gridapp.LoadSample
}

// GridHandlers serve the grid endpoints (nodes, devices, connections).
// Access is enforced by the x-meshsdr-access policy of openapi.yaml; authz
// only decides what an anonymous-level operation shows to admins.
type GridHandlers struct {
	authz   Authorizer
	nodes   NodeAdmin
	history LoadHistory
	caps    CapabilityReports
	devices DeviceRegistry
	conns   ConnectionRegistry
}

// NewGridHandlers returns the handlers.
func NewGridHandlers(authz Authorizer, nodes NodeAdmin, history LoadHistory, caps CapabilityReports, devices DeviceRegistry,
	conns ConnectionRegistry,
) GridHandlers {
	return GridHandlers{authz: authz, nodes: nodes, history: history, caps: caps, devices: devices, conns: conns}
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

// RevokeNode implements StrictServerInterface.
func (h GridHandlers) RevokeNode(ctx context.Context, req RevokeNodeRequestObject) (RevokeNodeResponseObject, error) {
	n, err := h.nodes.Revoke(ctx, actor, req.Id)
	if err != nil {
		return nil, err
	}

	return RevokeNode200JSONResponse(nodeDTO(n)), nil
}

// IssueNodeEnrollmentToken implements StrictServerInterface.
func (h GridHandlers) IssueNodeEnrollmentToken(ctx context.Context, req IssueNodeEnrollmentTokenRequestObject) (IssueNodeEnrollmentTokenResponseObject, error) {
	issued, err := h.nodes.IssueToken(ctx, actor, req.Id)
	if err != nil {
		return nil, err
	}

	return IssueNodeEnrollmentToken200JSONResponse(issuedDTO(issued)), nil
}

func object(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)

	return out
}

// GetNodeCapabilities implements StrictServerInterface.
func (h GridHandlers) GetNodeCapabilities(ctx context.Context, req GetNodeCapabilitiesRequestObject) (GetNodeCapabilitiesResponseObject, error) {
	rep, err := h.caps.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}

	out := GetNodeCapabilities200JSONResponse{
		NodeId: rep.Node().String(), ReportedAt: rep.ReportedAt(), CapabilitiesHash: rep.Hash(), ProductVersion: rep.ProductVersion(),
		Protocols: rep.Protocols(), Platform: object(rep.Platform()), Document: object(rep.Document()), Capabilities: []NodeCapability{},
	}

	for _, c := range rep.Capabilities() {
		out.Capabilities = append(out.Capabilities, NodeCapability{
			Capability: c.Key(), Available: c.Available(), Status: NodeCapabilityStatus(c.Status()),
			Version: optString(c.Version()), Detail: object(c.Detail()), Error: optString(c.Error()),
		})
	}

	return out, nil
}

// ProbeNodeCapabilities implements StrictServerInterface.
func (h GridHandlers) ProbeNodeCapabilities(ctx context.Context, req ProbeNodeCapabilitiesRequestObject) (ProbeNodeCapabilitiesResponseObject, error) {
	if err := h.caps.Probe(ctx, req.Id); err != nil {
		return nil, err
	}

	return ProbeNodeCapabilities202Response{}, nil
}

func deviceDTO(d *domain.Device) Device {
	s := d.Snapshot()
	out := Device{
		Id: s.ID, NodeId: s.Node, Name: s.Name, Type: s.Type, FreqMin: s.FreqMin, FreqMax: s.FreqMax,
		SampleRates: s.SampleRates, Enabled: s.Flags.Enabled, OperatorCanRetune: s.Flags.OperatorCanRetune,
		AlwaysOn: s.Flags.AlwaysOn, SchedulerEnabled: s.Flags.SchedulerEnabled, Online: s.Online,
		RuntimeState: DeviceRuntimeState(s.State), RuntimeStateAt: s.StateAt, RuntimeReason: optString(s.Reason),
		CenterFreq: s.CenterFreq, SortOrder: s.SortOrder, ReportedAt: s.ReportedAt,
	}

	if since, ok := d.Missing(); ok {
		out.MissingSince = &since
	}

	if s.Flags.ListenPolicy != "" {
		lp := DeviceListenPolicy(s.Flags.ListenPolicy)
		out.ListenPolicy = &lp
	}

	if !s.ActivePreset.IsZero() {
		var u openapi_types.UUID
		copy(u[:], s.ActivePreset.Bytes())
		out.ActivePresetId = &u
	}

	if out.SampleRates == nil {
		out.SampleRates = []int64{}
	}

	return out
}

// ListDevices implements StrictServerInterface.
func (h GridHandlers) ListDevices(ctx context.Context, _ ListDevicesRequestObject) (ListDevicesResponseObject, error) {
	devices, err := h.devices.List(ctx)
	if err != nil {
		return nil, err
	}

	out := ListDevices200JSONResponse{Items: make([]Device, 0, len(devices))}
	for _, d := range devices {
		out.Items = append(out.Items, deviceDTO(d))
	}

	return out, nil
}

// GetDevice implements StrictServerInterface.
func (h GridHandlers) GetDevice(ctx context.Context, req GetDeviceRequestObject) (GetDeviceResponseObject, error) {
	d, err := h.devices.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}

	return GetDevice200JSONResponse(deviceDTO(d)), nil
}

// ForgetDevice implements StrictServerInterface (ADM-009).
func (h GridHandlers) ForgetDevice(ctx context.Context, req ForgetDeviceRequestObject) (ForgetDeviceResponseObject, error) {
	if err := h.devices.Forget(ctx, actor, req.Id); err != nil {
		return nil, err
	}

	return ForgetDevice204Response{}, nil
}

func uuidPtr(u shared.UUID) *openapi_types.UUID {
	if u.IsZero() {
		return nil
	}

	var out openapi_types.UUID
	copy(out[:], u.Bytes())

	return &out
}

// ListConnections implements StrictServerInterface: the count for
// everyone, the connections for admins (§6.10).
func (h GridHandlers) ListConnections(ctx context.Context, _ ListConnectionsRequestObject) (ListConnectionsResponseObject, error) {
	count, err := h.conns.Count(ctx)
	if err != nil {
		return nil, err
	}

	listeners, err := h.conns.Listeners(ctx)
	if err != nil {
		return nil, err
	}

	out := ListConnections200JSONResponse{Count: count, Listeners: listeners}

	if h.authz.Authorize(ctx, idomain.RoleAdmin) != nil {
		return out, nil
	}

	conns, err := h.conns.List(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]Connection, 0, len(conns))

	for _, c := range conns {
		i := c.Info()
		id := uuidPtr(i.ID)
		items = append(items, Connection{
			Id: *id, Kind: ConnectionKind(i.Kind), UserId: uuidPtr(i.UserID), NodeId: optString(i.NodeID),
			DeviceId: optString(i.DeviceID), Mode: optString(i.Mode), Ip: i.IP, OpenedAt: c.OpenedAt(), LastHeartbeatAt: c.LastHeartbeat(),
		})
	}

	out.Items = &items

	return out, nil
}
