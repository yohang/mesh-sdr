package api

import (
	"context"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// FeatureSummary builds the public feature summary (grid/app.Features).
type FeatureSummary interface {
	Summary(ctx context.Context) (gridapp.Summary, error)
}

// FeatureHandlers serve GET /features (API-001).
type FeatureHandlers struct {
	authz    Authorizer
	features FeatureSummary
}

// NewFeatureHandlers returns the handlers.
func NewFeatureHandlers(authz Authorizer, features FeatureSummary) FeatureHandlers {
	return FeatureHandlers{authz: authz, features: features}
}

// GetFeatures implements StrictServerInterface: the summary, without the
// devices the caller may not listen to (a registered-only device is hidden
// from anonymous callers), and only the nodes of the devices listed.
func (h FeatureHandlers) GetFeatures(ctx context.Context, _ GetFeaturesRequestObject) (GetFeaturesResponseObject, error) {
	all, err := h.features.Summary(ctx)
	if err != nil {
		return nil, err
	}

	signedIn := h.authz.Authorize(ctx, domain.RoleListener) == nil
	out := GetFeatures200JSONResponse{Devices: []DeviceFeatures{}, Nodes: []NodeFeatures{}}
	listed := map[string]bool{}

	for _, d := range all.Devices {
		if d.ListenPolicy != gridapp.ListenAnonymous && !signedIn {
			continue
		}

		df := DeviceFeatures{
			Id: d.ID.String(), NodeId: d.Node.String(), Name: d.Name, Online: d.Online, NodeOnline: d.NodeOnline,
			State: DeviceFeaturesState(d.State), Modes: d.Modes, LoginRequired: d.ListenPolicy != gridapp.ListenAnonymous,
			Listeners: d.Listeners,
		}

		if !d.ActivePreset.IsZero() {
			df.ActivePreset = &PresetRef{Id: d.ActivePreset.String(), Name: d.PresetName}
		}

		out.Devices = append(out.Devices, df)
		listed[df.NodeId] = true
	}

	for _, n := range all.Nodes {
		if !listed[n.ID.String()] {
			continue
		}

		nf := NodeFeatures{Id: n.ID.String(), Name: n.Name, Online: n.Online}
		if t := n.Telemetry; t != nil {
			cpu := t.CPU
			nf.Cpu, nf.TempC = &cpu, t.TempC
		}

		out.Nodes = append(out.Nodes, nf)
	}

	return out, nil
}
