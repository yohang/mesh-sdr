package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/http/api"
	idomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

var discard = slog.New(slog.DiscardHandler)

// roleAuthz authorizes callers holding role, like the identity module.
type roleAuthz struct{ role idomain.Role }

func (a roleAuthz) Authorize(_ context.Context, need idomain.Role) error {
	switch {
	case a.role >= need:
		return nil
	case a.role == idomain.RoleAnonymous:
		return idomain.ErrUnauthenticated
	default:
		return idomain.ErrForbidden
	}
}

var (
	anonymous = roleAuthz{idomain.RoleAnonymous}
	listener  = roleAuthz{idomain.RoleListener}
	admin     = roleAuthz{idomain.RoleAdmin}
)

type summary []gridapp.DeviceFeatures

func (s summary) Summary(context.Context) ([]gridapp.DeviceFeatures, error) { return s, nil }

// TestFeatures covers API-001's public summary: open to every role, the
// registered-only devices hidden from anonymous callers.
func TestFeatures(t *testing.T) {
	s := summary{
		{ID: shared.MustDeviceID("hf"), Node: domain.MustNodeID("attic"), Name: "HF", Online: true, Modes: []string{"ft8"}, ListenPolicy: "anonymous"},
		{ID: shared.MustDeviceID("vhf"), Node: domain.MustNodeID("attic"), Name: "VHF", Modes: []string{}, ListenPolicy: "registered"},
	}

	for _, tt := range []struct {
		name  string
		authz roleAuthz
		want  []string
	}{
		{"anonymous", anonymous, []string{"hf"}},
		{"listener", listener, []string{"hf", "vhf"}},
		{"admin", admin, []string{"hf", "vhf"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(api.NewHandler(api.Server{FeatureHandlers: api.NewFeatureHandlers(tt.authz, s)}, tt.authz, discard))
			t.Cleanup(srv.Close)

			res, err := http.Get(srv.URL + "/features")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()

			var got struct {
				Devices []map[string]any `json:"devices"`
			}
			if err := json.NewDecoder(res.Body).Decode(&got); err != nil || res.StatusCode != http.StatusOK {
				t.Fatalf("GET /features = %d, %v", res.StatusCode, err)
			}

			if len(got.Devices) != len(tt.want) {
				t.Fatalf("devices = %v, want %v", got.Devices, tt.want)
			}

			for i, id := range tt.want {
				if got.Devices[i]["id"] != id || got.Devices[i]["node_id"] != "attic" || got.Devices[i]["modes"] == nil {
					t.Errorf("device %d = %v", i, got.Devices[i])
				}

				if _, leaks := got.Devices[i]["listen_policy"]; leaks {
					t.Errorf("device %d carries its listen policy", i)
				}
			}
		})
	}
}
