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

// DeviceFeatures is the public feature summary of one device (API-001):
// what it is, whether it is online and which modes it offers.
type DeviceFeatures struct {
	ID     shared.DeviceID
	Node   domain.NodeID
	Name   string
	Online bool
	// Modes are the modes the device's node reports available (mode:*
	// capabilities), sorted; empty when the node has not reported or the
	// device's driver is missing.
	Modes []string
	// ListenPolicy is the effective listen policy of the device (its node
	// config override, else the global one): anonymous or registered.
	ListenPolicy string
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

// Features builds the public feature summary from the device registry and
// the capability reports.
type Features struct {
	devices DeviceLister
	caps    CapabilityReader
	policy  GlobalListenPolicy
}

// NewFeatures returns the use case.
func NewFeatures(devices DeviceLister, caps CapabilityReader, policy GlobalListenPolicy) *Features {
	return &Features{devices: devices, caps: caps, policy: policy}
}

// Summary returns the enabled devices in registry order with their modes.
// A global policy that cannot be read counts as registered: the summary
// fails closed.
func (f *Features) Summary(ctx context.Context) ([]DeviceFeatures, error) {
	devices, err := f.devices.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}

	global, err := f.policy.ListenPolicy(ctx)
	if err != nil || (global != ListenAnonymous && global != ListenRegistered) {
		global = ListenRegistered
	}

	reports := map[domain.NodeID][]domain.Capability{}
	out := []DeviceFeatures{}

	for _, d := range devices {
		flags := d.Flags()
		if !flags.Enabled {
			continue
		}

		caps, ok := reports[d.Node()]
		if !ok {
			r, err := f.caps.Get(ctx, d.Node())
			if err != nil && !errors.Is(err, domain.ErrCapabilitiesNotReported) {
				return nil, fmt.Errorf("capabilities of %s: %w", d.Node(), err)
			}

			caps = r.Capabilities()
			reports[d.Node()] = caps
		}

		policy := global
		if flags.ListenPolicy == ListenAnonymous || flags.ListenPolicy == ListenRegistered {
			policy = flags.ListenPolicy
		}

		out = append(out, DeviceFeatures{
			ID: d.ID(), Node: d.Node(), Name: d.Name(), Online: d.Online(),
			Modes: modes(caps, d.Type()), ListenPolicy: policy,
		})
	}

	return out, nil
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

// Device returns the effective listen policy of an enabled device; ok is
// false for an unknown or disabled device, which nobody listens to.
func (p *ListenPolicies) Device(ctx context.Context, id string) (policy string, ok bool, err error) {
	devices, err := p.devices.List(ctx)
	if err != nil {
		return "", false, fmt.Errorf("list devices: %w", err)
	}

	for _, d := range devices {
		if d.ID().String() == id && d.Flags().Enabled {
			return effective(d, p.global(ctx)), true, nil
		}
	}

	return "", false, nil
}

// AnyAnonymous reports whether some enabled device is anonymous-listenable.
func (p *ListenPolicies) AnyAnonymous(ctx context.Context) (bool, error) {
	devices, err := p.devices.List(ctx)
	if err != nil {
		return false, fmt.Errorf("list devices: %w", err)
	}

	global := p.global(ctx)

	for _, d := range devices {
		if d.Flags().Enabled && effective(d, global) == ListenAnonymous {
			return true, nil
		}
	}

	return false, nil
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
