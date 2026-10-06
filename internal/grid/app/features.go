package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
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
	ID     domain.DeviceID
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
