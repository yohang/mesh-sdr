// Package decoder runs the decoders of the node (TECHNICAL_SPEC §8.4): it
// probes the external decoder tools behind every decoder capability
// (DEC-001) and runs decoder sessions through the process supervisor in
// private workdirs, argv only, with limits, a watchdog and bounded restarts
// (DEC-048). Each digital mode of the catalogue has an adapter here: its
// tool, argv, stderr rules and output parser.
package decoder

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// probeParallel bounds the concurrent probes (§8.4 "Capability probing"
// rule 2).
const probeParallel = 4

// toolProbe is how a tool is probed: an argv that exits quickly with stdin
// closed (any exit code) and prints a line matching match, on stdout or
// stderr; its first group, if any, is the version. Tools that print no
// version get it from version, another program of the same package
// (WSJT-X: wsjtx_app_version).
type toolProbe struct {
	args    []string
	match   *regexp.Regexp
	version *versionProbe
}

// versionProbe reads the version of a tool from another program.
type versionProbe struct {
	name  string
	args  []string
	match *regexp.Regexp
}

var wsjtxVersion = &versionProbe{name: "wsjtx_app_version", args: []string{"-v"}, match: regexp.MustCompile(`WSJT-X v?(\d+\.\d+(?:\.\d+)?)`)}

// toolProbes are the decoder tools of the node, by program name, with the
// output of the pinned versions (Debian trixie, csdr-skimmer 1.12).
var toolProbes = map[string]toolProbe{
	"jt9":              {args: []string{"-h"}, match: regexp.MustCompile(`Usage: jt9`), version: wsjtxVersion},
	"wsprd":            {args: []string{}, match: regexp.MustCompile(`Usage: wsprd`), version: wsjtxVersion},
	"js8":              {args: []string{"-h"}, match: regexp.MustCompile(`Usage: js8`)},
	"direwolf":         {args: []string{"-h"}, match: regexp.MustCompile(`Dire Wolf version (\d+\.\d+(?:\.\d+)?)`)},
	"multimon-ng":      {args: []string{"-h"}, match: regexp.MustCompile(`multimon-ng (\d+\.\d+(?:\.\d+)?)`)},
	"rtl_433":          {args: []string{"-V"}, match: regexp.MustCompile(`rtl_433 version (\d+\.\d+(?:\.\d+)?)`)},
	"csdr-cwskimmer":   {args: []string{"-h"}, match: regexp.MustCompile(`CW Skimmer`)},
	"csdr-rttyskimmer": {args: []string{"-h"}, match: regexp.MustCompile(`RTTY Skimmer`)},
}

// capProbe is a decoder capability: the tools it needs and the lowest
// version of its first tool ("" for any).
type capProbe struct {
	cap        string
	tools      []string
	minVersion string
}

// capProbes are the decoder capabilities of the node (DEC-001), in report
// order. cap:native-dsp needs no tool: the native decoders are linked in.
var capProbes = []capProbe{
	{cap: domain.CapWSJT, tools: []string{"jt9"}},
	{cap: domain.CapWSJT23, tools: []string{"jt9"}, minVersion: "2.3"},
	{cap: domain.CapWSJT24, tools: []string{"jt9"}, minVersion: "2.4"},
	{cap: domain.CapWSPRD, tools: []string{"wsprd"}},
	{cap: domain.CapJS8, tools: []string{"js8"}},
	{cap: domain.CapDirewolf, tools: []string{"direwolf"}},
	{cap: domain.CapMultimonNG, tools: []string{"multimon-ng"}},
	{cap: domain.CapRTL433, tools: []string{"rtl_433"}},
	{cap: domain.CapSkimmer, tools: []string{"csdr-cwskimmer"}},
	{cap: domain.CapRTTYSkimmer, tools: []string{"csdr-rttyskimmer"}},
	{cap: domain.CapNativeDSP},
}

// ToolboxOptions configure the toolbox.
type ToolboxOptions struct {
	// Supervisor runs the probes; nil: no tool can run (no runtime dir).
	Supervisor *process.Supervisor
	Tools      process.Tools
	Logger     *slog.Logger
}

// Toolbox probes the decoder tools and remembers the result: it implements
// app.DecoderTools.
type Toolbox struct {
	o ToolboxOptions

	// probing serialises the probes (their workdirs have fixed names).
	probing sync.Mutex

	mu     sync.Mutex
	caps   map[string]string // capability → reason ("" when available)
	logged map[string]string
}

// NewToolbox returns a toolbox that has not probed yet: every capability
// is unavailable until the first Probe.
func NewToolbox(o ToolboxOptions) *Toolbox {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}

	return &Toolbox{o: o, caps: map[string]string{}, logged: map[string]string{}}
}

// Available implements app.DecoderTools.
func (t *Toolbox) Available(c string) (bool, string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	reason, ok := t.caps[c]
	if !ok {
		return false, "not probed yet"
	}

	return reason == "", reason
}

// Probe runs every decoder tool (argv only, stdin closed, process.ProbeTimeout,
// at most probeParallel at once, §8.4) and returns the decoder
// capabilities with their tools and digital modes. It is called for every
// capability report: at start, on the hub's request, and after a decoder
// tool went missing (exit 127 or ENOENT).
func (t *Toolbox) Probe(ctx context.Context) []app.DecoderCapability {
	t.probing.Lock()
	defer t.probing.Unlock()

	results := t.probeTools(ctx)

	modes := map[string][]string{}
	for _, m := range domain.DigitalModes() {
		if !m.ServiceOnly {
			modes[m.Cap] = append(modes[m.Cap], m.Name)
		}
	}

	out := make([]app.DecoderCapability, 0, len(capProbes))
	caps := make(map[string]string, len(capProbes))

	for _, cp := range capProbes {
		dc := app.DecoderCapability{Cap: cp.cap, Tools: []app.ToolStatus{}, Modes: slices.Clone(modes[cp.cap])}
		if dc.Modes == nil {
			dc.Modes = []string{}
		}

		var reasons []string

		for i, name := range cp.tools {
			st := results[name]
			switch {
			case i > 0 || !st.OK || cp.minVersion == "":
			case st.Version == "":
				st.OK, st.Reason = false, fmt.Sprintf("the version of %s is unknown (%s or later needed)", name, cp.minVersion)
			case compareVersions(st.Version, cp.minVersion) < 0:
				st.OK, st.Reason = false, fmt.Sprintf("%s %s is older than %s", name, st.Version, cp.minVersion)
			}

			if !st.OK {
				reasons = append(reasons, st.Reason)
			}

			dc.Tools = append(dc.Tools, st)
		}

		caps[cp.cap] = strings.Join(reasons, "; ")
		out = append(out, dc)
	}

	t.mu.Lock()
	t.caps = caps

	for c, reason := range caps {
		if reason != "" && reason != t.logged[c] {
			t.o.Logger.LogAttrs(ctx, slog.LevelWarn, "decoder capability unavailable", slog.String("cap", c), slog.String("reason", reason))
		}
	}

	t.logged = caps
	t.mu.Unlock()

	return out
}

// probeTools probes every tool of capProbes once.
func (t *Toolbox) probeTools(ctx context.Context) map[string]app.ToolStatus {
	var names []string

	for _, cp := range capProbes {
		for _, n := range cp.tools {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make(map[string]app.ToolStatus, len(names))
		sem = make(chan struct{}, probeParallel)
	)

	for _, n := range names {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			st := t.probeTool(ctx, n)

			mu.Lock()
			out[n] = st
			mu.Unlock()
		})
	}

	wg.Wait()

	return out
}

// versionLines collects the first version match of a probe's output.
type versionLines struct {
	re *regexp.Regexp

	mu      sync.Mutex
	version string
	seen    bool
}

func (v *versionLines) add(line string) {
	m := v.re.FindStringSubmatch(line)
	if m == nil {
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.seen {
		return
	}

	v.seen = true
	if len(m) > 1 {
		v.version = m[1]
	}
}

func (v *versionLines) result() (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.version, v.seen
}

func (t *Toolbox) probeTool(ctx context.Context, name string) app.ToolStatus {
	st := app.ToolStatus{Name: name}
	tp := toolProbes[name]

	version, ok, reason := t.run(ctx, "probe-"+name, name, tp.args, tp.match)
	if !ok {
		st.Reason = reason

		return st
	}

	if tp.version != nil {
		// The version is optional: the tool runs without it.
		version, _, _ = t.run(ctx, "probe-"+name+"-version", tp.version.name, tp.version.args, tp.version.match)
	}

	st.Version, st.OK = version, true

	return st
}

// run runs one probe: ok when the program ran and printed a line matching
// match; version is its first group.
func (t *Toolbox) run(ctx context.Context, id, name string, args []string, match *regexp.Regexp) (version string, ok bool, reason string) {
	path, err := t.o.Tools.Resolve(name)
	if err != nil {
		return "", false, name + " not found"
	}

	v := &versionLines{re: match}

	err = process.Probe(ctx, t.o.Supervisor, process.Spec{ID: id, Path: path, Args: args, ToolDirs: t.o.Tools.Dirs}, v.add)

	switch {
	case errors.Is(err, process.ErrNoRuntimeDir):
		return "", false, err.Error()
	case errors.Is(err, process.ErrJobTimeout):
		return "", false, name + " did not answer within " + process.ProbeTimeout.String()
	case errors.Is(err, process.ErrUnavailable) || errors.Is(err, fs.ErrNotExist):
		return "", false, name + " cannot run"
	case err != nil:
		return "", false, name + ": " + err.Error()
	}

	version, seen := v.result()
	if !seen {
		return "", false, name + " did not print its usage"
	}

	return version, true, ""
}

// compareVersions compares dotted numeric versions ("2.6.1" vs "2.4");
// missing parts count as 0 and a non-numeric part stops the comparison.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")

	for i := range max(len(pa), len(pb)) {
		x, y := part(pa, i), part(pb, i)
		if x != y {
			if x < y {
				return -1
			}

			return 1
		}
	}

	return 0
}

func part(p []string, i int) int {
	if i >= len(p) {
		return 0
	}

	n, err := strconv.Atoi(strings.TrimLeftFunc(p[i], func(r rune) bool { return r < '0' || r > '9' }))
	if err != nil {
		return 0
	}

	return n
}
