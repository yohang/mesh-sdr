package bookmarks

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// Band is a band of a band plan (RX-029).
type Band struct {
	Name  string
	Low   int64
	High  int64
	Tags  []string // hamradio, broadcast, public, service: the colour key
	Dials []Dial
}

// Dial is a dial frequency of a band (RX-030): where a mode is used.
type Dial struct {
	Mode       string
	Frequency  int64
	Underlying string
	Band       string
}

// Bandplan is the band plan of one region, kept in memory (read from the
// embedded files once).
type Bandplan struct {
	region string
	bands  []Band
}

// Region returns the region (r1, r2, r3).
func (p *Bandplan) Region() string { return p.region }

// Bands returns the bands that overlap [from, to], by lower bound.
func (p *Bandplan) Bands(from, to int64) []Band {
	var out []Band

	for _, b := range p.bands {
		if b.High >= from && b.Low <= to {
			b.Tags, b.Dials = slices.Clone(b.Tags), slices.Clone(b.Dials)
			out = append(out, b)
		}
	}

	return out
}

// Dials returns the dial frequencies in [from, to], by frequency then mode.
func (p *Bandplan) Dials(from, to int64) []Dial {
	var out []Dial

	for _, b := range p.bands {
		for _, d := range b.Dials {
			if d.Frequency >= from && d.Frequency <= to {
				out = append(out, d)
			}
		}
	}

	slices.SortStableFunc(out, func(a, b Dial) int {
		return cmp.Or(cmp.Compare(a.Frequency, b.Frequency), strings.Compare(a.Mode, b.Mode))
	})

	return out
}

// owrxBand is one band of an OpenWebRX+ band plan (bands-rN.json).
// frequencies maps a mode to a frequency, a list of frequencies, or
// objects with a frequency and an underlying mode.
type owrxBand struct {
	Name        string                     `json:"name"`
	Lower       int64                      `json:"lower_bound"`
	Upper       int64                      `json:"upper_bound"`
	Frequencies map[string]json.RawMessage `json:"frequencies"`
	Tags        []string                   `json:"tags"`
}

type owrxDial struct {
	Frequency  int64  `json:"frequency"`
	Underlying string `json:"underlying"`
}

// LoadBandplans parses the embedded band plans of every region.
func LoadBandplans() (map[string]*Bandplan, error) { return loadBandplans(data, "data/bands") }

func loadBandplans(fsys fs.FS, dir string) (map[string]*Bandplan, error) {
	out := make(map[string]*Bandplan, len(Regions))

	for _, r := range Regions {
		p, err := parseBandplan(fsys, path.Join(dir, "bands-"+r+".json"), r)
		if err != nil {
			return nil, err
		}

		out[r] = p
	}

	return out, nil
}

func parseBandplan(fsys fs.FS, file, region string) (*Bandplan, error) {
	raw, err := fs.ReadFile(fsys, file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	var list []owrxBand
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}

	p := &Bandplan{region: region}

	for _, ob := range list {
		if strings.TrimSpace(ob.Name) == "" || ob.Lower <= 0 || ob.Upper < ob.Lower {
			return nil, fmt.Errorf("%s: invalid band %q", file, ob.Name)
		}

		b := Band{Name: ob.Name, Low: ob.Lower, High: ob.Upper, Tags: ob.Tags}
		if b.Tags == nil {
			b.Tags = []string{}
		}

		for mode, rawDials := range ob.Frequencies {
			dials, err := parseDials(rawDials)
			if err != nil {
				return nil, fmt.Errorf("%s: band %s, mode %s: %w", file, ob.Name, mode, err)
			}

			for _, d := range dials {
				b.Dials = append(b.Dials, Dial{Mode: modeOf(mode), Frequency: d.Frequency, Underlying: modeOf(d.Underlying), Band: ob.Name})
			}
		}

		slices.SortFunc(b.Dials, func(x, y Dial) int {
			return cmp.Or(cmp.Compare(x.Frequency, y.Frequency), strings.Compare(x.Mode, y.Mode))
		})

		p.bands = append(p.bands, b)
	}

	slices.SortStableFunc(p.bands, func(x, y Band) int { return cmp.Compare(x.Low, y.Low) })

	return p, nil
}

// parseDials reads the dial frequencies of one mode: a number, an object
// or a list of either.
func parseDials(raw json.RawMessage) ([]owrxDial, error) {
	raw = bytes.TrimSpace(raw)

	if len(raw) > 0 && raw[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}

		var out []owrxDial

		for _, it := range items {
			d, err := parseDial(it)
			if err != nil {
				return nil, err
			}

			out = append(out, d)
		}

		return out, nil
	}

	d, err := parseDial(raw)
	if err != nil {
		return nil, err
	}

	return []owrxDial{d}, nil
}

func parseDial(raw json.RawMessage) (owrxDial, error) {
	var d owrxDial

	var hz int64
	if err := json.Unmarshal(raw, &hz); err == nil {
		d.Frequency = hz
	} else if err := json.Unmarshal(raw, &d); err != nil {
		return owrxDial{}, err
	}

	if d.Frequency <= 0 {
		return owrxDial{}, fmt.Errorf("invalid dial frequency %s", raw)
	}

	return d, nil
}
