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

type summary gridapp.Summary

func (s summary) Summary(context.Context) (gridapp.Summary, error) { return gridapp.Summary(s), nil }

// TestFeatures covers API-001's public summary: open to every role, the
// registered-only devices hidden from anonymous callers.
func TestFeatures(t *testing.T) {
	preset := shared.MustParseUUID("0192c3a4-5b6c-7d8e-9f01-23456789abcd")
	cpu, temp := 0.25, 40.0
	s := summary{
		Devices: []gridapp.DeviceFeatures{
			{
				ID: shared.MustDeviceID("hf"), Node: domain.MustNodeID("attic"), Name: "HF", Online: true, State: domain.StateRunning,
				Modes: []string{"ft8"}, ListenPolicy: "anonymous", Listeners: 2, ActivePreset: preset, PresetName: "40 m",
			},
			{ID: shared.MustDeviceID("vhf"), Node: domain.MustNodeID("attic"), Name: "VHF", State: domain.StateStopped, Modes: []string{}, ListenPolicy: "registered"},
			{ID: shared.MustDeviceID("uhf"), Node: domain.MustNodeID("roof"), Name: "UHF", State: domain.StateStopped, Modes: []string{}, ListenPolicy: "registered"},
		},
		Nodes: []gridapp.NodeFeatures{
			{ID: domain.MustNodeID("attic"), Name: "Attic", Online: true, Telemetry: &gridapp.Telemetry{CPU: cpu, TempC: &temp}},
			{ID: domain.MustNodeID("roof"), Name: "Roof"},
		},
	}

	for _, tt := range []struct {
		name  string
		authz roleAuthz
		want  []string
		nodes []string
	}{
		{"anonymous", anonymous, []string{"hf"}, []string{"attic"}},
		{"listener", listener, []string{"hf", "vhf", "uhf"}, []string{"attic", "roof"}},
		{"admin", admin, []string{"hf", "vhf", "uhf"}, []string{"attic", "roof"}},
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
				Nodes   []map[string]any `json:"nodes"`
			}
			if err := json.NewDecoder(res.Body).Decode(&got); err != nil || res.StatusCode != http.StatusOK {
				t.Fatalf("GET /features = %d, %v", res.StatusCode, err)
			}

			if len(got.Devices) != len(tt.want) {
				t.Fatalf("devices = %v, want %v", got.Devices, tt.want)
			}

			for i, id := range tt.want {
				if got.Devices[i]["id"] != id || got.Devices[i]["node_id"] == nil || got.Devices[i]["modes"] == nil ||
					got.Devices[i]["state"] == nil || got.Devices[i]["listeners"] == nil {
					t.Errorf("device %d = %v", i, got.Devices[i])
				}

				if _, leaks := got.Devices[i]["listen_policy"]; leaks {
					t.Errorf("device %d carries its listen policy", i)
				}

				// The lock of the picker (SRC-023).
				if got.Devices[i]["login_required"] != (id != "hf") {
					t.Errorf("device %d login_required = %v", i, got.Devices[i]["login_required"])
				}
			}

			hf := got.Devices[0]
			if hf["state"] != "running" || hf["listeners"] != 2.0 {
				t.Errorf("hf = %v", hf)
			}

			if p, _ := hf["active_preset"].(map[string]any); p["id"] != preset.String() || p["name"] != "40 m" {
				t.Errorf("hf active_preset = %v", hf["active_preset"])
			}

			if _, ok := got.Devices[len(got.Devices)-1]["active_preset"]; ok && len(got.Devices) > 1 {
				t.Errorf("vhf has an active preset: %v", got.Devices[1])
			}

			// Only the nodes of listed devices, with the public telemetry
			// only (no address, version or load).
			if len(got.Nodes) != len(tt.nodes) {
				t.Fatalf("nodes = %v, want %v", got.Nodes, tt.nodes)
			}

			for i, id := range tt.nodes {
				if got.Nodes[i]["id"] != id {
					t.Errorf("node %d = %v, want %s", i, got.Nodes[i], id)
				}

				for k := range got.Nodes[i] {
					switch k {
					case "id", "name", "online", "cpu", "temp_c":
					default:
						t.Errorf("node %d carries %q", i, k)
					}
				}
			}

			if a := got.Nodes[0]; a["name"] != "Attic" || a["online"] != true || a["cpu"] != cpu || a["temp_c"] != temp {
				t.Errorf("attic = %v", a)
			}

		})
	}
}
