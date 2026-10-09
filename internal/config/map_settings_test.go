package config

import "testing"

// Lookup link templates are empty, or an http or https URL with exactly
// one {} placeholder (ADM-032); the map base layers are the known ones.
func TestMapSettings(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		ok         bool
	}{
		{"links.callsign_url", `"https://www.qrzcq.com/call/{}"`, true},
		{"links.geoip_url", `"https://www.geolocation.com/?ip={}#ipresult"`, true},
		{"links.modes_url", `"http://example.org/{}/redirect"`, true},
		{"links.sonde_url", `""`, true},
		{"links.callsign_url", `"https://www.qrzcq.com/call/"`, false},
		{"links.callsign_url", `"https://example.org/{}/{}"`, false},
		{"links.callsign_url", `"javascript:alert({})"`, false},
		{"links.vessel_url", `"ftp://example.org/{}"`, false},
		{"links.flight_url", `"/local/{}"`, false},
		{"links.flight_url", `"https://user@example.org/{}"`, false},
		{"links.flight_url", `"https://example.org/a b/{}"`, false},
		{"map.base_layers", `["osm","cartodb_dark_matter"]`, true},
		{"map.base_layers", `[]`, false},
		{"map.base_layers", `["stadia"]`, false},
		{"map.default_base_layer", `"opentopomap"`, true},
		{"map.default_base_layer", `"google"`, false},
		{"map.position_retention_s", `7200`, true},
		{"map.position_retention_s", `30`, false},
		{"map.max_calls", `0`, true},
		{"map.max_calls", `101`, false},
		{"map.call_retention_s", `5`, false},
		{"map.precise_receivers", `true`, true},
	} {
		if _, vs, err := DecodeSetting(tc.key, []byte(tc.value)); err != nil || (len(vs) == 0) != tc.ok {
			t.Errorf("%s = %s: %v %v", tc.key, tc.value, vs, err)
		}
	}

	for _, tt := range []struct {
		name, toml string
		bad        bool
	}{
		{"default layer offered", "[settings.map]\nbase_layers = [\"osm\", \"opentopomap\"]\ndefault_base_layer = \"opentopomap\"\n", false},
		{"default layer not offered", "[settings.map]\nbase_layers = [\"opentopomap\"]\ndefault_base_layer = \"osm\"\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"hub.toml": minimalHub + tt.toml})

			if _, _, err := LoadHub(Options{Dir: dir, Env: map[string]string{}}); (err != nil) != tt.bad {
				t.Errorf("err = %v, want error %v", err, tt.bad)
			}
		})
	}
}

// node.gps and devices.<id>.gps (MAP-007) load from the file, node.gps also
// from the env.
func TestNodeGPS(t *testing.T) {
	dir := writeFiles(t, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\ngps = { lat = 50.63, lon = 3.06 }\n" +
		"[devices.hf]\nname = \"HF\"\ntype = \"rtl_sdr\"\nfreq_range = { min = 1_000, max = \"30MHz\" }\nsample_rates = [250_000]\n" +
		"gps = { lat = 48.85, lon = 2.35 }\n"})

	cfg, _, err := LoadNode(Options{Dir: dir, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}

	if g := cfg.Node.GPS; !g.IsSet() || g.Lat() != 50.63 || g.Lon() != 3.06 {
		t.Errorf("node gps %+v", g)
	}

	if g := cfg.Devices["hf"].GPS; !g.IsSet() || g.Lat() != 48.85 {
		t.Errorf("device gps %+v", g)
	}

	cfg, meta, err := LoadNode(Options{Dir: dir, Env: map[string]string{"MESHSDR_NODE__GPS": "45.76,4.83"}})
	if err != nil || cfg.Node.GPS.Lat() != 45.76 || meta.Origins.Of("node.gps").String() != "env:MESHSDR_NODE__GPS" {
		t.Errorf("env node gps %+v %v", cfg.Node.GPS, err)
	}

	bad := writeFiles(t, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\ngps = { lat = 95, lon = 3 }\n"})
	if _, _, err := LoadNode(Options{Dir: bad, Env: map[string]string{}}); err == nil {
		t.Error("invalid node gps accepted")
	}
}
