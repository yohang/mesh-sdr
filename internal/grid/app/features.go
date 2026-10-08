package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Listen policies of a device (§7.4 listen_policy).
const (
	ListenAnonymous  = "anonymous"
	ListenRegistered = "registered"
)

// GlobalListenPolicy reads the hub-wide listen policy (the listen_policy
// setting), which a device's node config may override.
type GlobalListenPolicy interface {
	ListenPolicy(ctx context.Context) (string, error)
}

// DeviceFeatures is the public feature summary of one device (API-001,
// UI-021): what it is, whether it can be listened to, which modes it
// offers, its active preset and its listeners.
type DeviceFeatures struct {
	ID     shared.DeviceID
	Node   domain.NodeID
	Name   string
	Online bool
	// NodeOnline reports whether the device's node has its control channel
	// up (hub presence): Online is false for an idle device, which only
	// runs while someone listens.
	NodeOnline bool
	// State is the device's runtime state in the registry (SRC-025).
	State domain.RuntimeState
	// Modes are the modes the device's node reports available (mode:*
	// capabilities), sorted; empty when the node has not reported or the
	// device's driver is missing.
	Modes []string
	// ListenPolicy is the effective listen policy of the device (its node
	// config override, else the global one): anonymous or registered.
	ListenPolicy string
	// Listeners counts the open media connections attached to the device
	// (connections registry).
	Listeners int
	// ActivePreset is the preset the node reported active (zero: none);
	// PresetName names it ("" when unknown to the hub).
	ActivePreset shared.UUID
	PresetName   string
}

// NodeFeatures describes the node of listed devices: its name, whether it
// is online and its last telemetry while online (RX-035: CPU and temperature,
// the §6.6 public subset; nothing else of the heartbeat).
type NodeFeatures struct {
	ID        domain.NodeID
	Name      string
	Online    bool
	Telemetry *Telemetry
}

// Telemetry is the public part of a node heartbeat (TECHNICAL_SPEC §6.6
// node.status public subset).
type Telemetry struct {
	// CPU is the busy ratio, 0 to 1.
	CPU   float64
	TempC *float64
}

// Summary is the feature summary: the enabled devices in registry order
// and their nodes.
type Summary struct {
	Devices []DeviceFeatures
	Nodes   []NodeFeatures
}

// DeviceLister lists the device registry (domain.DeviceRepository).
type DeviceLister interface {
	List(ctx context.Context) ([]*domain.Device, error)
}

// CapabilityReader reads the last capability report of a node
// (domain.CapabilityRepository).
type CapabilityReader interface {
	Get(ctx context.Context, id domain.NodeID) (domain.CapabilityReport, error)
}

// NodeLinks lists the nodes whose control channel is up (Tracker).
type NodeLinks interface {
	Connected() []domain.NodeID
}

// NodeLister lists the node registry (domain.NodeRepository).
type NodeLister interface {
	List(ctx context.Context) ([]*domain.Node, error)
}

// DeviceListeners counts the listeners by device (Presence).
type DeviceListeners interface {
	ListenersByDevice(ctx context.Context) (map[string]int, error)
}

// LatestTelemetry gives the last heartbeat sample of a node (History).
type LatestTelemetry interface {
	Latest(id domain.NodeID) (LoadSample, bool)
}

// PresetNamer names a preset ("" when unknown).
type PresetNamer func(ctx context.Context, id shared.UUID) string

// FeaturesDeps are the dependencies of Features. Links, Nodes, Listeners,
// Telemetry and PresetName may be nil: every node is then offline, nodes
// are named by their id, devices have no listeners, nodes no telemetry
// and presets no name.
type FeaturesDeps struct {
	Devices    DeviceLister
	Caps       CapabilityReader
	Policy     GlobalListenPolicy
	Links      NodeLinks
	Nodes      NodeLister
	Listeners  DeviceListeners
	Telemetry  LatestTelemetry
	PresetName PresetNamer
}

// Features builds the public feature summary from the device registry, the
// capability reports, the node links, the connections registry and the
// heartbeat history.
type Features struct{ d FeaturesDeps }

// NewFeatures returns the use case.
func NewFeatures(d FeaturesDeps) *Features { return &Features{d: d} }

// Summary returns the enabled devices in registry order with their modes,
// and the nodes of those devices in order of first appearance. A global
// policy that cannot be read counts as registered: the summary fails
// closed.
func (f *Features) Summary(ctx context.Context) (Summary, error) {
	devices, err := f.d.Devices.List(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("list devices: %w", err)
	}

	global, err := f.d.Policy.ListenPolicy(ctx)
	if err != nil || (global != ListenAnonymous && global != ListenRegistered) {
		global = ListenRegistered
	}

	connected := map[domain.NodeID]bool{}
	if f.d.Links != nil {
		for _, id := range f.d.Links.Connected() {
			connected[id] = true
		}
	}

	listeners := map[string]int{}
	if f.d.Listeners != nil {
		if listeners, err = f.d.Listeners.ListenersByDevice(ctx); err != nil {
			return Summary{}, fmt.Errorf("count listeners: %w", err)
		}
	}

	names, err := f.nodeNames(ctx)
	if err != nil {
		return Summary{}, err
	}

	reports := map[domain.NodeID][]domain.Capability{}
	out := Summary{Devices: []DeviceFeatures{}, Nodes: []NodeFeatures{}}

	for _, d := range devices {
		flags := d.Flags()
		if !flags.Enabled {
			continue
		}

		caps, ok := reports[d.Node()]
		if !ok {
			r, err := f.d.Caps.Get(ctx, d.Node())
			if err != nil && !errors.Is(err, domain.ErrCapabilitiesNotReported) {
				return Summary{}, fmt.Errorf("capabilities of %s: %w", d.Node(), err)
			}

			caps = r.Capabilities()
			reports[d.Node()] = caps

			out.Nodes = append(out.Nodes, f.node(d.Node(), names, connected[d.Node()]))
		}

		policy := global
		if flags.ListenPolicy == ListenAnonymous || flags.ListenPolicy == ListenRegistered {
			policy = flags.ListenPolicy
		}

		state, _, _ := d.State()
		df := DeviceFeatures{
			ID: d.ID(), Node: d.Node(), Name: d.Name(), Online: d.Online(), NodeOnline: connected[d.Node()], State: state,
			Modes: modes(caps, d.Type()), ListenPolicy: policy, Listeners: listeners[d.ID().String()],
			ActivePreset: d.ActivePreset(),
		}

		if !df.ActivePreset.IsZero() && f.d.PresetName != nil {
			df.PresetName = f.d.PresetName(ctx, df.ActivePreset)
		}

		out.Devices = append(out.Devices, df)
	}

	return out, nil
}

// nodeNames returns the node names by id (none without a node lister).
func (f *Features) nodeNames(ctx context.Context) (map[domain.NodeID]string, error) {
	names := map[domain.NodeID]string{}
	if f.d.Nodes == nil {
		return names, nil
	}

	nodes, err := f.d.Nodes.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	for _, n := range nodes {
		names[n.ID()] = n.Name().String()
	}

	return names, nil
}

// node describes a node of the summary; its telemetry only while online.
func (f *Features) node(id domain.NodeID, names map[domain.NodeID]string, online bool) NodeFeatures {
	n := NodeFeatures{ID: id, Name: names[id], Online: online}
	if n.Name == "" {
		n.Name = id.String()
	}

	if online && f.d.Telemetry != nil {
		if s, ok := f.d.Telemetry.Latest(id); ok {
			n.Telemetry = &Telemetry{CPU: s.CPU, TempC: s.TempC}
		}
	}

	return n
}

// modes returns the available mode:* capabilities, unless the node reports
// the driver of the device type missing.
func modes(caps []domain.Capability, deviceType string) []string {
	out := []string{}

	for _, c := range caps {
		if c.Key() == "driver:"+deviceType && !c.Available() {
			return []string{}
		}

		if m, ok := strings.CutPrefix(c.Key(), "mode:"); ok && c.Available() {
			out = append(out, m)
		}
	}

	slices.Sort(out)

	return out
}

// ListenPolicies tells who may listen to the devices of the registry: the
// effective listen policy of a device (its node config override, else the
// global listen_policy), and whether anonymous visitors may listen to any
// device (TECHNICAL_SPEC §5.9, §6.6 "Topic access").
type ListenPolicies struct {
	devices DeviceLister
	policy  GlobalListenPolicy
}

// NewListenPolicies returns the use case.
func NewListenPolicies(devices DeviceLister, policy GlobalListenPolicy) *ListenPolicies {
	return &ListenPolicies{devices: devices, policy: policy}
}

// global returns the global policy; one that cannot be read counts as
// registered (fail closed).
func (p *ListenPolicies) global(ctx context.Context) string {
	g, err := p.policy.ListenPolicy(ctx)
	if err != nil || (g != ListenAnonymous && g != ListenRegistered) {
		return ListenRegistered
	}

	return g
}

func effective(d *domain.Device, global string) string {
	if lp := d.Flags().ListenPolicy; lp == ListenAnonymous || lp == ListenRegistered {
		return lp
	}

	return global
}

// Effective returns the effective listen policy of every enabled device, by
// device id: one consistent view for many checks.
func (p *ListenPolicies) Effective(ctx context.Context) (map[string]string, error) {
	devices, err := p.devices.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}

	global := p.global(ctx)
	out := make(map[string]string, len(devices))

	for _, d := range devices {
		if d.Flags().Enabled {
			out[d.ID().String()] = effective(d, global)
		}
	}

	return out, nil
}
