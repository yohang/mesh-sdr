package app

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

// GlobalListenPolicy reads the hub-wide listen policy (the listen_policy
// setting), which a device's node config may override.
type GlobalListenPolicy interface {
	ListenPolicy(ctx context.Context) (string, error)
}

// ListenPolicies is the single source of who may listen to the devices of
// the registry (TECHNICAL_SPEC §5.9, §6.6 "Topic access", ADR 0018, ADR
// 0026): the global listen_policy, the effective policy of every enabled
// device (its node config override, else the global one) and the rule
// applying it to a caller. Every path fails closed: a global policy that
// is missing, invalid or unreadable counts as registered, and a device
// the view does not list (disabled, unknown) cannot be listened to.
//
// The view of the devices is cached: Refresh reloads it when the policies
// may have changed (a listen_policy setting change, a device report, a
// forgotten device). Loads run under the lock, one at a time.
type ListenPolicies struct {
	devices DeviceLister
	policy  GlobalListenPolicy
	logger  *slog.Logger

	mu   sync.Mutex
	view *ListenView
}

// NewListenPolicies returns the use case; devices may be nil for a caller
// that only reads the global policy.
func NewListenPolicies(devices DeviceLister, policy GlobalListenPolicy, logger *slog.Logger) *ListenPolicies {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &ListenPolicies{devices: devices, policy: policy, logger: logger}
}

// Global returns the global listen policy, read at each call; one that is
// missing, invalid or unreadable counts as registered.
func (p *ListenPolicies) Global(ctx context.Context) string {
	g, err := p.policy.ListenPolicy(ctx)
	if err == nil && (g == domain.ListenAnonymous || g == domain.ListenRegistered) {
		return g
	}

	p.logger.WarnContext(ctx, "listen policy unavailable or invalid, falling back to registered",
		slog.String("value", g), slog.Any("error", err))

	return domain.ListenRegistered
}

// View returns the cached view, loading it on first use.
func (p *ListenPolicies) View(ctx context.Context) (ListenView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.view != nil {
		return *p.view, nil
	}

	v, err := p.load(ctx)
	if err != nil {
		return ListenView{}, err
	}

	p.view = &v

	return v, nil
}

// Refresh reloads the view and reports whether it changed. A failed
// reload drops the view: the readers fail closed until a load succeeds.
// Loads are serialised, so an older load never replaces a newer one.
func (p *ListenPolicies) Refresh(ctx context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	v, err := p.load(ctx)
	if err != nil {
		p.view = nil

		return false, err
	}

	changed := p.view == nil || !maps.Equal(p.view.devices, v.devices)
	p.view = &v

	return changed, nil
}

// CanListen reports whether a caller (anonymous or signed in) may listen
// to a device, from the cached view.
func (p *ListenPolicies) CanListen(ctx context.Context, anonymous bool, device string) (bool, error) {
	v, err := p.View(ctx)
	if err != nil {
		return false, err
	}

	return v.CanListen(anonymous, device), nil
}

func (p *ListenPolicies) load(ctx context.Context) (ListenView, error) {
	devices, err := p.devices.List(ctx)
	if err != nil {
		return ListenView{}, fmt.Errorf("list devices: %w", err)
	}

	global := p.Global(ctx)
	out := make(map[string]string, len(devices))

	for _, d := range devices {
		if d.Flags().Enabled {
			out[d.ID().String()] = effective(d, global)
		}
	}

	return ListenView{devices: out}, nil
}

// ListenView is one consistent view of who may listen to what: the
// effective listen policy of every enabled device.
type ListenView struct {
	devices map[string]string
}

// Policy returns the effective listen policy of a device listed by the
// view.
func (v ListenView) Policy(device string) (string, bool) {
	lp, ok := v.devices[device]

	return lp, ok
}

// CanListen reports whether a caller (anonymous or signed in) may listen
// to a device; a device the view does not list cannot be listened to.
func (v ListenView) CanListen(anonymous bool, device string) bool {
	lp, ok := v.devices[device]

	return ok && allowed(lp, anonymous)
}

// AnyAnonymous reports whether some device is anonymous-listenable.
func (v ListenView) AnyAnonymous() bool {
	for _, lp := range v.devices {
		if lp == domain.ListenAnonymous {
			return true
		}
	}

	return false
}

// Listenable returns the listed devices a caller (anonymous or signed in)
// may listen to, sorted.
func (v ListenView) Listenable(anonymous bool) []string {
	var out []string

	for d, lp := range v.devices {
		if allowed(lp, anonymous) {
			out = append(out, d)
		}
	}

	slices.Sort(out)

	return out
}

// CanListen reports whether a caller (anonymous or signed in) may listen
// to the device of the summary.
func (d DeviceFeatures) CanListen(anonymous bool) bool { return allowed(d.ListenPolicy, anonymous) }

// effective returns the effective listen policy of a device: its node
// config override, else the global one. An invalid override (never
// accepted by the registry) counts as registered.
func effective(d *domain.Device, global string) string {
	switch lp := d.Flags().ListenPolicy; lp {
	case "":
		return global
	case domain.ListenAnonymous, domain.ListenRegistered:
		return lp
	default:
		return domain.ListenRegistered
	}
}

// allowed applies an effective listen policy: anonymous admits everyone,
// registered the signed-in users, anything else nobody.
func allowed(policy string, anonymous bool) bool {
	switch policy {
	case domain.ListenAnonymous:
		return true
	case domain.ListenRegistered:
		return !anonymous
	default:
		return false
	}
}
