// Package http serves the device messages of the node media WebSocket
// (TECHNICAL_SPEC §6.4, §6.5): device.attach / detach, stream.configure,
// audio.configure, demod.create / set / remove, device.retune and
// preset.select (shared centre: one tuning for every listener). The
// grid media endpoint authenticates the connection, checks the token scope
// and hands these messages over (media.Streams); this package turns them
// into device manager and DSP engine calls, and the engine output into
// rx.v1 frames on the connection's send queue.
package http

import (
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/radio/domain"
	"github.com/yohang/mesh-sdr/internal/shared/ratelimit"
)

// Device defaults sent in device.config (FEATURE_SPEC defaults of
// dsp.squelch_auto_margin; ADR 0019) and the demodulator limit of a token
// without lim.max_demods.
const (
	tuningStepHz     = 1000
	autoMinRange     = 50
	squelchMargin    = 10
	squelchInitial   = -150
	defaultMaxDemods = 1
)

// Devices is the device manager seen by the handler.
type Devices interface {
	Attach(id string) (*app.Lease, error)
	Retune(id string, hz, rate int64) (domain.Snapshot, error)
	SetActivePreset(id, preset string) error
	Watch(id string, fn func(domain.Snapshot)) (func(), error)
}

// DesiredState is the desired state pushed by the hub
// (grid/infra/agent.DesiredState): the presets of each device and the global
// settings.
type DesiredState interface {
	Device(id string) (ctl.DesiredDevice, bool)
	Preset(id string) (ctl.Preset, bool)
	Policy() ctl.StatePolicy
}

// Streams implements media.Streams.
type Streams struct {
	devices Devices
	state   DesiredState
	dec     Decoding
	log     *slog.Logger

	// switching serialises the shared changes of the devices
	// (preset.select, device.retune) and guards lastSwitch, applied and
	// waterfall.
	switching sync.Mutex
	// lastSwitch is the time of the last preset switch of each device
	// (§5.11 limit).
	lastSwitch map[string]time.Time
	// applied is the active preset of each device as it was switched: an
	// edit or an unassignment by the hub clears it.
	applied map[string]activePreset
	// waterfall are the waterfall defaults the listeners last received;
	// nodeWaterfall are those of the hub settings until the desired state
	// carries them (ADR 0026).
	waterfall     media.Waterfall
	nodeWaterfall ctl.StateWaterfall
	// retunes limits the device.retune of each device (§5.11).
	retunes *ratelimit.Limiter[string]
	// decoderSets limits the decoder.set of each connection.
	decoderSets *ratelimit.Limiter[string]
	now         func() time.Time

	mu       sync.Mutex
	sessions map[*session]struct{}
}

// Per-device limits of the shared changes (§5.11): one preset switch per
// 5 s, four retunes per second.
const (
	PresetSwitchEvery = 5 * time.Second
	RetuneEvery       = 250 * time.Millisecond
	RetuneBurst       = 4
)

// NewStreams returns the handler. state may be nil (no hub state: no
// preset, the node defaults apply); a zero dec offers no digital mode;
// waterfall are the waterfall defaults until the desired state carries
// them.
func NewStreams(d Devices, state DesiredState, dec Decoding, waterfall ctl.StateWaterfall, log *slog.Logger) *Streams {
	s := &Streams{
		devices: d, state: state, dec: dec, nodeWaterfall: waterfall, log: log, sessions: map[*session]struct{}{},
		lastSwitch: map[string]time.Time{}, applied: map[string]activePreset{},
		retunes:     ratelimit.New[string](RetuneEvery, RetuneBurst, ratelimit.DefaultCapacity),
		decoderSets: ratelimit.New[string](DecoderSetEvery, DecoderSetBurst, ratelimit.DefaultCapacity), now: time.Now,
	}
	s.waterfall = s.waterfallDefaults()

	return s
}

// presets returns what device.config shows of the presets of a device: the
// active preset activeID (nil when none or no longer offered) and the
// presets it may switch to.
func (s *Streams) presets(device, activeID string) (*media.PresetRef, *ctl.Preset, []media.PresetRef) {
	avail := []media.PresetRef{}

	if s.state == nil {
		return nil, nil, avail
	}

	want, ok := s.state.Device(device)
	if !ok {
		return nil, nil, avail
	}

	var (
		ref    *media.PresetRef
		active *ctl.Preset
	)

	for _, id := range want.Presets {
		p, ok := s.state.Preset(id)
		if !ok {
			continue
		}

		avail = append(avail, media.PresetRef{ID: id, Name: p.Name})

		if id == activeID {
			ref, active = &media.PresetRef{ID: id, Name: p.Name}, &p
		}
	}

	return ref, active, avail
}

// activePreset is a switched preset: its id and its content then.
type activePreset struct {
	id string
	p  ctl.Preset
}

// StateApplied clears the active preset of the devices whose preset the
// hub edited or no longer assigns to them, and sends new waterfall defaults
// to every listener (device.config.patch). Call it after every applied
// desired state.
func (s *Streams) StateApplied() {
	s.switching.Lock()
	defer s.switching.Unlock()

	if w := s.waterfallDefaults(); w != s.waterfall {
		s.waterfall = w

		s.mu.Lock()
		all := slices.Collect(maps.Keys(s.sessions))
		s.mu.Unlock()

		for _, ss := range all {
			ss.waterfallChanged(w)
		}
	}

	for device, was := range s.applied {
		if p, err := s.resolve(device, was.id); err == nil && reflect.DeepEqual(p, was.p) {
			continue
		}

		s.clearPreset(device)
	}
}

// clearPreset forgets the active preset of a device (switching held).
func (s *Streams) clearPreset(device string) {
	delete(s.applied, device)

	if err := s.devices.SetActivePreset(device, ""); err != nil {
		s.log.Warn("active preset not cleared", slog.String("device_id", device), slog.Any("error", err))
	}
}

// resolve returns a preset of the desired state that the device may run.
func (s *Streams) resolve(device, id string) (ctl.Preset, error) {
	if s.state == nil {
		return ctl.Preset{}, &rxv1.Error{Code: rxv1.CodeNotFound, Reason: "no preset received from the hub"}
	}

	p, ok := s.state.Preset(id)
	if !ok {
		return ctl.Preset{}, &rxv1.Error{Code: rxv1.CodeNotFound, Reason: "unknown preset"}
	}

	if want, ok := s.state.Device(device); !ok || !slices.Contains(want.Presets, id) {
		return ctl.Preset{}, &rxv1.Error{Code: rxv1.CodePresetIncompatible, Reason: "the preset does not fit this device"}
	}

	return p, nil
}

// listeners returns the sessions attached to a device.
func (s *Streams) listeners(device string) []*session {
	s.mu.Lock()
	all := slices.Collect(maps.Keys(s.sessions))
	s.mu.Unlock()

	out := all[:0]

	for _, ss := range all {
		if ss.attachedTo(device) != nil {
			out = append(out, ss)
		}
	}

	return out
}

// waterfallDefaults returns the waterfall defaults of device.config: the
// hub settings of the desired state, else the node's.
func (s *Streams) waterfallDefaults() media.Waterfall {
	p := s.nodeWaterfall

	if s.state != nil {
		if w := s.state.Policy().Waterfall; w != nil {
			p = *w
		}
	}

	return media.Waterfall{Levels: media.Levels{Min: float64(p.MinDB), Max: float64(p.MaxDB)}, AutoMinRange: autoMinRange, Scheme: p.Palette}
}

// Open implements media.Streams.
func (s *Streams) Open(p media.Peer) media.StreamSession {
	ss := &session{
		s: s, peer: p, q: p.Queue(), next: 1,
		devices: map[string]*attached{}, demods: map[string]*demodState{},
		audio: audioConfig{codec: preferredCodec(p.Hello()), rate: dsp.DefaultOutputRate},
	}

	s.mu.Lock()
	s.sessions[ss] = struct{}{}
	s.mu.Unlock()

	return ss
}
