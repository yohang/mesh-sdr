// Package probe samples the node host for capability reports and
// heartbeats (§4.5, §4.7). Linux values come from /proc and /sys; missing
// files give zero values.
package probe

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

// Prober implements agent.Prober.
type Prober struct {
	version string
	devices func() []ctl.Device
	started time.Time
	root    string // filesystem root, "/" except in tests

	mu       sync.Mutex
	lastIdle uint64
	lastAll  uint64
}

// New returns a prober. devices lists the devices of the node config.
func New(version string, devices func() []ctl.Device, started time.Time) *Prober {
	return &Prober{version: version, devices: devices, started: started, root: "/"}
}

func (p *Prober) read(path string) []byte {
	b, err := os.ReadFile(p.root + strings.TrimPrefix(path, "/"))
	if err != nil {
		return nil
	}

	return b
}

// Capabilities implements agent.Prober.
func (p *Prober) Capabilities(context.Context) ctl.Capabilities {
	host, _ := os.Hostname()
	mem := p.meminfo()

	devices := []ctl.Device{}
	if p.devices != nil {
		devices = p.devices()
	}

	return ctl.Capabilities{
		ProductVersion: p.version,
		Protocols:      []string{rxv1.Subprotocol, rxv1.ControlSubprotocol},
		Platform: ctl.Platform{
			OS: runtime.GOOS, Arch: runtime.GOARCH, CPUModel: p.cpuModel(), CPUCores: runtime.NumCPU(),
			RAMBytes: mem["MemTotal"], Hostname: host,
		},
		SDRDrivers:      []ctl.SDRDriver{},
		Devices:         devices,
		DevicesDetected: []any{},
		Decoders:        []ctl.Decoder{},
		AudioCodecs:     []string{},
		FFTCodecs:       []string{},
	}
}

// Heartbeat implements agent.Prober.
func (p *Prober) Heartbeat(context.Context) ctl.Heartbeat {
	mem := p.meminfo()
	hb := ctl.Heartbeat{
		UptimeS: int64(time.Since(p.started).Seconds()),
		CPU:     p.cpuUsage(),
		Load:    p.loadavg(),
		Mem:     ctl.Mem{TotalBytes: mem["MemTotal"], AvailableBytes: mem["MemAvailable"]},
	}

	if b := p.read("/sys/class/thermal/thermal_zone0/temp"); b != nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			c := v / 1000
			hb.TempC = &c
		}
	}

	return hb
}

// NTPSynced implements agent.Prober.
func (p *Prober) NTPSynced() bool { return ntpSynced() }

func (p *Prober) meminfo() map[string]uint64 {
	out := map[string]uint64{}

	s := bufio.NewScanner(bytes.NewReader(p.read("/proc/meminfo")))
	for s.Scan() {
		k, v, ok := strings.Cut(s.Text(), ":")
		if !ok {
			continue
		}

		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}

		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}

		if len(f) > 1 && f[1] == "kB" {
			n *= 1024
		}

		out[k] = n
	}

	return out
}

func (p *Prober) cpuModel() string {
	s := bufio.NewScanner(bytes.NewReader(p.read("/proc/cpuinfo")))
	for s.Scan() {
		k, v, ok := strings.Cut(s.Text(), ":")
		if ok && (strings.TrimSpace(k) == "model name" || strings.TrimSpace(k) == "Model") {
			return strings.TrimSpace(v)
		}
	}

	return ""
}

func (p *Prober) loadavg() [3]float64 {
	var out [3]float64

	f := strings.Fields(string(p.read("/proc/loadavg")))
	for i := 0; i < 3 && i < len(f); i++ {
		out[i], _ = strconv.ParseFloat(f[i], 64)
	}

	return out
}

// cpuUsage returns the CPU busy ratio (0..1) since the previous call.
func (p *Prober) cpuUsage() float64 {
	line, _, _ := strings.Cut(string(p.read("/proc/stat")), "\n")

	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0
	}

	var all, idle uint64

	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		all += n

		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	dAll, dIdle := all-p.lastAll, idle-p.lastIdle
	p.lastAll, p.lastIdle = all, idle

	if dAll == 0 || dIdle > dAll {
		return 0
	}

	return float64(dAll-dIdle) / float64(dAll)
}
