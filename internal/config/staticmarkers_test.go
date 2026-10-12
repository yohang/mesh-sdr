package config

import (
	"errors"
	"strings"
	"testing"
)

// Static marker files are read from map.static_markers_dir, resolved
// against the config dir; an invalid file is a config error naming it.
func TestStaticMarkers(t *testing.T) {
	const good = `[{"id": "lille", "name": "Lille club", "lat": 50.63, "lon": 3.06, "comment": "Thursdays", "url": "https://example.org/"},
		{"id": "b2", "name": "Beacon", "lat": -10, "lon": 179.5}]`

	tests := []struct {
		name, file, want string
	}{
		{"valid", good, ""},
		{"empty array", `[]`, ""},
		{"too large", "[]" + strings.Repeat(" ", 1<<20), "larger than"},
		{"trailing data", `[] [{"id": "a"}]`, "data after the array"},
		{"file field", `[{"id": "a", "File": "x", "name": "A", "lat": 1, "lon": 2}]`, "unknown field"},
		{"not an array", `{"id": "a"}`, "a.json: want a JSON array"},
		{"unknown field", `[{"id": "a", "name": "A", "lat": 1, "lon": 2, "colour": "red"}]`, "unknown field"},
		{"bad id", `[{"id": "-a", "name": "A", "lat": 1, "lon": 2}]`, "marker 1: id"},
		{"repeated id", `[{"id": "a", "name": "A", "lat": 1, "lon": 2}, {"id": "a", "name": "B", "lat": 1, "lon": 2}]`, "marker 2: id \"a\" is repeated"},
		{"no name", `[{"id": "a", "name": " ", "lat": 1, "lon": 2}]`, "name must be"},
		{"control character", `[{"id": "a", "name": "A\u0007", "lat": 1, "lon": 2}]`, "name must be"},
		{"latitude", `[{"id": "a", "name": "A", "lat": 91, "lon": 2}]`, "lat must be"},
		{"longitude", `[{"id": "a", "name": "A", "lat": 1, "lon": -181}]`, "lon must be"},
		{"long comment", `[{"id": "a", "name": "A", "lat": 1, "lon": 2, "comment": "` + strings.Repeat("x", 513) + `"}]`, "comment must be"},
		{"javascript url", `[{"id": "a", "name": "A", "lat": 1, "lon": 2, "url": "javascript:alert(1)"}]`, "url must be"},
		{"url without host", `[{"id": "a", "name": "A", "lat": 1, "lon": 2, "url": "https:///x"}]`, "url must be"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{
				"hub.toml":          minimalHub + "[map]\nstatic_markers_dir = \"markers\"\n",
				"markers/a.json":    tt.file,
				"markers/notes.txt": "ignored",
			})

			cfg, _, err := LoadHub(Options{Dir: dir, Env: map[string]string{}})

			var cerr *Error
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}

				markers, err := ReadStaticMarkers(cfg.Map.StaticMarkersDir)
				if err != nil || (tt.file == good && (len(markers) != 2 || markers[0].File != "a" || markers[0].URL != "https://example.org/")) {
					t.Errorf("markers %+v, %v", markers, err)
				}

				return
			}

			if !errors.As(err, &cerr) || cerr.Problems[0].Key != "map.static_markers_dir" || !strings.Contains(cerr.Problems[0].Message, tt.want) {
				t.Errorf("error %v, want %q", err, tt.want)
			}
		})
	}

	// A missing directory is a config error; no directory set, no markers.
	dir := writeFiles(t, map[string]string{"hub.toml": minimalHub + "[map]\nstatic_markers_dir = \"nope\"\n"})
	if _, _, err := LoadHub(Options{Dir: dir, Env: map[string]string{}}); err == nil {
		t.Error("missing directory accepted")
	}

	dir = writeFiles(t, map[string]string{"hub.toml": minimalHub})
	if cfg, _, err := LoadHub(Options{Dir: dir, Env: map[string]string{}}); err != nil || cfg.Map.StaticMarkersDir != "" {
		t.Errorf("unset: %q, %v", cfg.Map.StaticMarkersDir, err)
	}
}
