package api

import (
	"context"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// FeatureSummary builds the public feature summary (grid/app.Features).
type FeatureSummary interface {
	Summary(ctx context.Context) ([]gridapp.DeviceFeatures, error)
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
// from anonymous callers).
func (h FeatureHandlers) GetFeatures(ctx context.Context, _ GetFeaturesRequestObject) (GetFeaturesResponseObject, error) {
	all, err := h.features.Summary(ctx)
	if err != nil {
		return nil, err
	}

	signedIn := h.authz.Authorize(ctx, domain.RoleListener) == nil
	out := GetFeatures200JSONResponse{Devices: []DeviceFeatures{}}

	for _, d := range all {
		if d.ListenPolicy != gridapp.ListenAnonymous && !signedIn {
			continue
		}

		out.Devices = append(out.Devices, DeviceFeatures{
			Id: d.ID.String(), NodeId: d.Node.String(), Name: d.Name, Online: d.Online, NodeOnline: d.NodeOnline, Modes: d.Modes,
		})
	}

	return out, nil
}
