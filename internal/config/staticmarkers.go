package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// HubMap is the [map] table of the hub (MAP-017).
type HubMap struct {
	StaticMarkersDir string `toml:"static_markers_dir" env:"STATIC_MARKERS_DIR" jsonschema:"description=Directory of static map marker files (*.json\\, each an array of {id\\, name\\, lat\\, lon\\, comment?\\, url?})\\, read at hub start and shown read-only on the map to every visitor (MAP-017). Relative paths are resolved against the config dir. Empty: no static markers."`
}

// StaticMarker is one marker of a static marker file (MAP-017).
type StaticMarker struct {
	// File is the name of its file without .json: its id is unique there.
	File    string  `json:"-"`
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Comment string  `json:"comment"`
	URL     string  `json:"url"`
}

// Bounds of the static marker files.
const (
	maxMarkerFileSize = 1 << 20
	maxMarkers        = 10000
	maxMarkerName     = 128
	maxMarkerComment  = 512
	maxMarkerURL      = 512
)

// markerID is the pattern of a marker id and of a marker file name.
var markerID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ReadStaticMarkers reads the *.json files of dir but hidden ones, in
// lexical order: each
// is a JSON array of {id, name, lat, lon, comment?, url?}. An unreadable
// or invalid file is an error naming it.
func ReadStaticMarkers(dir string) ([]StaticMarker, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%s is not a readable directory", dir)
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}

	var out []StaticMarker

	for _, path := range files {
		// Hidden files (editor backups, .#locks) are not markers.
		if strings.HasPrefix(filepath.Base(path), ".") {
			continue
		}

		markers, err := readMarkerFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}

		out = append(out, markers...)
		if len(out) > maxMarkers {
			return nil, fmt.Errorf("more than %d static markers", maxMarkers)
		}
	}

	return out, nil
}

// readMarkerFile reads and checks one marker file.
func readMarkerFile(path string) ([]StaticMarker, error) {
	name := strings.TrimSuffix(filepath.Base(path), ".json")
	if !markerID.MatchString(name) {
		return nil, errors.New("file name must match " + markerID.String() + " before .json")
	}

	// A FIFO or device would block or never end: regular files only, and
	// the cap holds even if the file grows after the check.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	b, err := io.ReadAll(io.LimitReader(f, maxMarkerFileSize+1))
	if err != nil {
		return nil, err
	}

	if len(b) > maxMarkerFileSize {
		return nil, fmt.Errorf("larger than %d bytes", maxMarkerFileSize)
	}

	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()

	var markers []StaticMarker
	if err := dec.Decode(&markers); err != nil {
		return nil, fmt.Errorf("want a JSON array of {id, name, lat, lon, comment?, url?}: %w", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("want a JSON array of {id, name, lat, lon, comment?, url?}: data after the array")
	}

	seen := map[string]bool{}

	for i := range markers {
		m := &markers[i]
		m.File = name

		if err := m.check(); err != nil {
			return nil, fmt.Errorf("marker %d: %w", i+1, err)
		}

		if seen[m.ID] {
			return nil, fmt.Errorf("marker %d: id %q is repeated", i+1, m.ID)
		}

		seen[m.ID] = true
	}

	return markers, nil
}

// check validates a marker.
func (m StaticMarker) check() error {
	switch {
	case !markerID.MatchString(m.ID):
		return fmt.Errorf("id %q must match %s", m.ID, markerID)
	case strings.TrimSpace(m.Name) == "" || !plainText(m.Name, 1, maxMarkerName):
		return fmt.Errorf("name must be 1 to %d printable characters", maxMarkerName)
	case math.IsNaN(m.Lat) || m.Lat < -90 || m.Lat > 90:
		return errors.New("lat must be -90..90")
	case math.IsNaN(m.Lon) || m.Lon < -180 || m.Lon > 180:
		return errors.New("lon must be -180..180")
	case !plainText(m.Comment, 0, maxMarkerComment):
		return fmt.Errorf("comment must be at most %d printable characters", maxMarkerComment)
	}

	if m.URL == "" {
		return nil
	}

	u, err := url.Parse(m.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(m.URL) > maxMarkerURL {
		return fmt.Errorf("url must be an http or https URL of at most %d characters", maxMarkerURL)
	}

	return nil
}

// plainText tells whether s is valid UTF-8 without control characters, of
// min to max characters.
func plainText(s string, minLen, maxLen int) bool {
	n := utf8.RuneCountInString(s)

	return utf8.ValidString(s) && n >= minLen && n <= maxLen && strings.IndexFunc(s, unicode.IsControl) < 0
}
