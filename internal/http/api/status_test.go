package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/http/api"
)

type fakeStatus struct{ st api.StationStatus }

func (f fakeStatus) Status(context.Context) (api.StationStatus, error) { return f.st, nil }

func getJSON(t *testing.T, url string) (int, http.Header, []byte) {
	t.Helper()

	res, err := http.Get(url) //nolint:noctx // test server
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = res.Body.Close() }()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}

	return res.StatusCode, res.Header, b
}

// TestStatus covers API-003: the document shape, the e-mail published only
// when it is public, and the /status.json alias serving the same body.
func TestStatus(t *testing.T) {
	alt := 25
	devices := []api.StatusDeviceInfo{
		{ID: "hf", Name: "HF", Type: "rtlsdr", Online: true, Listeners: 2, Preset: &api.StatusPresetInfo{Name: "20 m", CenterFreq: 14_100_000, SampRate: 2_048_000}},
		{ID: "vhf", Name: "VHF", Type: "airspy"},
	}

	for _, tt := range []struct {
		name      string
		st        api.StationStatus
		wantEmail bool
		wantPos   bool
		wantLoc   bool
	}{
		{"email public", api.StationStatus{Name: "Attic", Location: "Lille", Lat: 50.6, Lon: 3.06, HasPosition: true, Altitude: &alt, Version: "1.2.3",
			AdminEmail: "admin@example.org", AdminEmailPublic: true, Devices: devices}, true, true, true},
		{"email private", api.StationStatus{Name: "Attic", Version: "1.2.3", AdminEmail: "admin@example.org", Devices: devices}, false, false, false},
		{"public without an e-mail", api.StationStatus{Name: "Attic", Version: "dev", AdminEmailPublic: true}, false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := api.NewStatusHandlers(fakeStatus{tt.st}, discard)

			root := chi.NewRouter()
			root.Mount("/api/v1", newHandler(t, api.Server{StatusHandlers: h}, anonymous))
			root.Method(http.MethodGet, "/status.json", h.Alias())

			srv := httptest.NewServer(root)
			t.Cleanup(srv.Close)

			code, _, body := getJSON(t, srv.URL+"/api/v1/status")
			if code != http.StatusOK {
				t.Fatalf("GET /api/v1/status = %d %s", code, body)
			}

			var got struct {
				Name        string           `json:"name"`
				Location    *string          `json:"location"`
				Position    map[string]any   `json:"position"`
				Altitude    *int             `json:"altitude"`
				Version     string           `json:"version"`
				AdminEmail  *string          `json:"admin_email"`
				DeviceCount int              `json:"device_count"`
				Devices     []map[string]any `json:"devices"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}

			if got.Name != "Attic" || got.Version != tt.st.Version || got.DeviceCount != len(tt.st.Devices) || got.Devices == nil ||
				len(got.Devices) != len(tt.st.Devices) {
				t.Errorf("status = %s", body)
			}

			if (got.AdminEmail != nil) != tt.wantEmail || (got.Position != nil) != tt.wantPos || (got.Location != nil) != tt.wantLoc {
				t.Errorf("email/position/location presence = %v/%v/%v, body %s", got.AdminEmail != nil, got.Position != nil, got.Location != nil, body)
			}

			if (got.Altitude != nil) != tt.wantPos || (tt.wantPos && *got.Altitude != 25) {
				t.Errorf("altitude = %v, body %s", got.Altitude, body)
			}

			if tt.wantEmail && *got.AdminEmail != "admin@example.org" {
				t.Errorf("admin_email = %q", *got.AdminEmail)
			}

			if len(tt.st.Devices) == 2 {
				hf, vhf := got.Devices[0], got.Devices[1]
				preset, _ := hf["preset"].(map[string]any)

				if hf["id"] != "hf" || hf["type"] != "rtlsdr" || hf["online"] != true || hf["listeners"] != float64(2) ||
					preset["name"] != "20 m" || preset["center_freq"] != float64(14_100_000) || preset["samp_rate"] != float64(2_048_000) {
					t.Errorf("hf = %v", hf)
				}

				if _, has := vhf["preset"]; has || vhf["online"] != false {
					t.Errorf("vhf = %v", vhf)
				}

				for _, d := range got.Devices {
					if _, leaks := d["node_id"]; leaks {
						t.Errorf("device carries its node: %v", d)
					}
				}
			}

			code, header, alias := getJSON(t, srv.URL+"/status.json")
			if code != http.StatusOK || header.Get("Content-Type") != "application/json" {
				t.Fatalf("GET /status.json = %d %s", code, header.Get("Content-Type"))
			}

			var a, b any
			if err := json.Unmarshal(alias, &a); err != nil {
				t.Fatal(err)
			}

			if err := json.Unmarshal(body, &b); err != nil {
				t.Fatal(err)
			}

			ja, _ := json.Marshal(a)
			jb, _ := json.Marshal(b)

			if string(ja) != string(jb) {
				t.Errorf("alias differs:\n%s\n%s", alias, body)
			}
		})
	}
}
