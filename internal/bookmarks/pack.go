package bookmarks

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // UUIDv5 is defined over SHA-1 (RFC 9562 §5.5), not used for security.
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// data holds the bookmark packs and band plans imported unmodified from
// OpenWebRX+ (see data/NOTICE).
//
//go:embed data/NOTICE data/bookmarks data/bands
var data embed.FS

// Regions are the IARU regions of the band plans and the region packs.
var Regions = []string{"r1", "r2", "r3"}

// ValidRegion reports whether r is a region id.
func ValidRegion(r string) bool { return slices.Contains(Regions, r) }

// owrxBookmark is one entry of an OpenWebRX+ bookmark file
// (bookmarks.d/*.json): only name, frequency and modulation are required.
type owrxBookmark struct {
	Name        string `json:"name"`
	Frequency   int64  `json:"frequency"`
	Modulation  string `json:"modulation"`
	Underlying  string `json:"underlying"`
	Description string `json:"description"`
	Scannable   *bool  `json:"scannable"`
}

// owrxScannable are the modes OpenWebRX+ scans when an entry does not say
// (Bookmark.SCANNABLE_MODES).
var owrxScannable = []string{"lsb", "usb", "cw", "am", "sam", "nfm"}

// owrxModes maps the OpenWebRX+ modulation names to the mode ids of the
// nodes. The analog modes share their names; any other mode (digital,
// decoders) is kept as is: it becomes usable when a node offers it (M2).
var owrxModes = map[string]string{
	"am": "am", "sam": "sam", "nfm": "nfm", "wfm": "wfm", "usb": "usb", "lsb": "lsb", "cw": "cw",
}

// modeOf returns the mode id of an OpenWebRX+ modulation name.
func modeOf(owrx string) string {
	m := strings.ToLower(strings.TrimSpace(owrx))
	if id, ok := owrxModes[m]; ok {
		return id
	}

	return m
}

// PackFile is one bookmark file of the packs, with its entry count.
type PackFile struct {
	// Pack is the file name without extension (aviation, pmr…).
	Pack string
	// Region is r1, r2 or r3 for a region pack, "" for the general pack.
	Region  string
	Entries int
}

// packEntry is a pack bookmark before it is stored: one per name,
// frequency and modulation, with the packs and regions that hold it.
type packEntry struct {
	name, modulation, underlying, description string
	frequency                                 int64
	scannable                                 bool
	packs                                     []string
	regions                                   []string
	general                                   bool
}

// key is the unique key of a bookmark (TECHNICAL_SPEC §7.1).
func (e *packEntry) key() string {
	return e.name + "\x00" + fmt.Sprint(e.frequency) + "\x00" + e.modulation
}

// packNamespace is the UUIDv5 namespace of the pack bookmark ids.
var packNamespace = shared.MustParseUUID("6f0d3c1e-3b6a-5a43-9a51-6d6573687364")

// id is the stable id of a pack bookmark: a UUIDv5 of its unique key, the
// same on every hub and every sync.
func (e *packEntry) id() shared.UUID {
	h := sha1.New() //nolint:gosec // see the import.
	h.Write(packNamespace.Bytes())
	h.Write([]byte(e.key()))

	sum := h.Sum(nil)[:16]
	sum[6] = sum[6]&0x0f | 0x50
	sum[8] = sum[8]&0x3f | 0x80

	id, _ := shared.UUIDFromBytes(sum)

	return id
}

// tags returns the tags of a pack row: its packs, and its regions unless a
// general pack also holds it (it then shows in every region).
func (e *packEntry) tags() []string {
	out := make([]string, 0, len(e.packs)+len(e.regions))
	for _, p := range e.packs {
		out = append(out, "pack:"+p)
	}

	if !e.general {
		for _, r := range e.regions {
			out = append(out, regionTagPrefix+r)
		}
	}

	slices.Sort(out)

	return slices.Compact(out)
}

// snapshot returns the stored form of the entry (its times and version
// are set by the sync).
func (e *packEntry) snapshot() Snapshot {
	return Snapshot{
		ID: e.id(), Name: e.name, Frequency: e.frequency, Modulation: e.modulation, Underlying: e.underlying,
		Description: e.description, Scannable: e.scannable, Tags: e.tags(), Origin: OriginBuiltin, Locked: []string{},
		Scope: AllDevices(), Version: 1,
	}
}

// Packs is the parsed content of the embedded packs: the general pack and
// the three region packs, merged by unique key.
type Packs struct {
	files   []PackFile
	entries []*packEntry
}

// Files returns the pack files, general pack first.
func (p *Packs) Files() []PackFile { return slices.Clone(p.files) }

// Len returns the number of distinct pack bookmarks.
func (p *Packs) Len() int { return len(p.entries) }

// LoadPacks parses the embedded packs.
func LoadPacks() (*Packs, error) { return loadPacks(data, "data/bookmarks") }

// loadPacks parses the packs of dir: its *.json files are the general
// pack, its r1, r2 and r3 directories the region packs.
func loadPacks(fsys fs.FS, dir string) (*Packs, error) {
	p := &Packs{}
	byKey := map[string]*packEntry{}

	for _, region := range append([]string{""}, Regions...) {
		files, err := fs.Glob(fsys, path.Join(dir, region, "*.json"))
		if err != nil {
			return nil, fmt.Errorf("list packs: %w", err)
		}

		if region != "" && len(files) == 0 {
			return nil, fmt.Errorf("region pack %s: no file", region)
		}

		for _, f := range files {
			n, err := p.parse(fsys, f, region, byKey)
			if err != nil {
				return nil, err
			}

			p.files = append(p.files, PackFile{Pack: strings.TrimSuffix(path.Base(f), ".json"), Region: region, Entries: n})
		}
	}

	slices.SortFunc(p.entries, func(a, b *packEntry) int {
		if a.frequency != b.frequency {
			return int(min(max(a.frequency-b.frequency, -1), 1))
		}

		return strings.Compare(a.key(), b.key())
	})

	return p, nil
}

// parse reads one pack file into byKey; it returns its entry count.
func (p *Packs) parse(fsys fs.FS, file, region string, byKey map[string]*packEntry) (int, error) {
	raw, err := fs.ReadFile(fsys, file)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", file, err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	var list []owrxBookmark
	if err := dec.Decode(&list); err != nil {
		return 0, fmt.Errorf("parse %s: %w", file, err)
	}

	pack := strings.TrimSuffix(path.Base(file), ".json")

	for i, b := range list {
		e := &packEntry{
			name: strings.TrimSpace(b.Name), frequency: b.Frequency, modulation: modeOf(b.Modulation),
			underlying: modeOf(b.Underlying), description: strings.TrimSpace(b.Description),
		}

		e.scannable = slices.Contains(owrxScannable, strings.ToLower(b.Modulation))
		if b.Scannable != nil {
			e.scannable = *b.Scannable
		}

		if _, err := Rehydrate(e.snapshot()); err != nil {
			return 0, fmt.Errorf("%s entry %d (%q): %w", file, i, b.Name, err)
		}

		cur, ok := byKey[e.key()]
		if !ok {
			cur = e
			byKey[e.key()] = e
			p.entries = append(p.entries, e)
		}

		cur.packs = append(cur.packs, pack)

		if region == "" {
			cur.general = true
		} else if !slices.Contains(cur.regions, region) {
			cur.regions = append(cur.regions, region)
		}
	}

	return len(list), nil
}
