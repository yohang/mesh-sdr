package app

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// AudioBlock is a block of demodulated audio (a demodulator tap, §8.3
// "Secondary decoder"). Samples are valid during the call only.
type AudioBlock struct {
	Samples []float32
	// Rate is the sample rate (the demodulator's output rate).
	Rate int
	// Time is the time of the first sample.
	Time time.Time
	// Discontinuity: input samples were lost before this block.
	Discontinuity bool
}

// Decoder session states (DEC-002: running, unavailable or error, with a
// reason).
const (
	DecoderRunning     = "running"
	DecoderUnavailable = "unavailable"
	DecoderError       = "error"
)

// DecoderStatus is a status change of a decoder session.
type DecoderStatus struct {
	State  string
	Reason string
}

// DecodeRecord is one typed message of a decoder (§8.4 "Typed framed
// output"): its plain-text rendering, already sanitised and capped, and the
// record named by Schema.
type DecodeRecord struct {
	Time    time.Time
	Schema  string
	Text    string
	Payload json.RawMessage
}

// DecoderEvents receive the output of a decoder session. They are called
// from the session's goroutines and must not block.
type DecoderEvents struct {
	Decode func(DecodeRecord)
	Status func(DecoderStatus)
}

// DecoderSpec describes a decoder session to start.
type DecoderSpec struct {
	// Session is the decoder session id (decoder_session_id).
	Session shared.UUID
	Mode    domain.DigitalMode
}

// DecoderRun is a running decoder session.
type DecoderRun interface {
	// Audio feeds the session (an InputAudio mode); it never blocks.
	Audio(b AudioBlock)
	// Close stops the session; it does not wait for the tool to exit.
	Close()
}

// DecoderRunner starts decoder sessions (the decoder adapters).
type DecoderRunner interface {
	Start(spec DecoderSpec, ev DecoderEvents) (DecoderRun, error)
}

// ToolStatus is the probe result of one external program (DEC-001).
type ToolStatus struct {
	Name    string
	Version string
	OK      bool
	// Reason says why the tool is not usable ("multimon-ng not found").
	Reason string
}

// DecoderCapability is one probed decoder capability: the tools behind it
// and the digital modes it enables. It is available when every tool is OK.
type DecoderCapability struct {
	Cap   string
	Tools []ToolStatus
	Modes []string
}

// DecoderTools reports the decoder capabilities of the node, from its last
// probe (DEC-001).
type DecoderTools interface {
	// Available reports whether a capability is usable, and why not.
	Available(cap string) (ok bool, reason string)
}

// Decoded is a decoded message for the hub (decode.batch, DEC-047).
type Decoded struct {
	DeviceID  string
	SessionID string
	PresetID  string
	Mode      string
	Family    string
	FreqHz    int64
	Time      time.Time
	// CID is the media connection of a listener's decoder.
	CID    string
	Schema string
	Text   string
	// Payload is the typed record.
	Payload json.RawMessage
}

// DecodePublisher forwards decoded messages to the hub. It must not block.
type DecodePublisher interface {
	Decoded(d Decoded)
}

// DigitalModeStatus is a digital mode of the catalogue with its
// availability on this node.
type DigitalModeStatus struct {
	Mode      domain.DigitalMode
	Available bool
	// Reason says why it is unavailable (the missing tool).
	Reason string
}

// Decoders checks and starts the decoder sessions of listeners (DEC-002):
// the mode catalogue, the capabilities probed on the node and the
// service-only flag form the allow-list (no extra key).
type Decoders struct {
	tools  DecoderTools
	runner DecoderRunner
	ids    *shared.UUIDv7Generator
	now    func() time.Time
}

// NewDecoders returns the service.
func NewDecoders(tools DecoderTools, runner DecoderRunner, now func() time.Time) *Decoders {
	return &Decoders{tools: tools, runner: runner, ids: shared.NewUUIDv7Generator(), now: now}
}

// Catalogue returns the digital modes a listener may start, with their
// availability: service-only modes are left out.
func (d *Decoders) Catalogue() []DigitalModeStatus {
	var out []DigitalModeStatus

	for _, m := range domain.DigitalModes() {
		if m.ServiceOnly {
			continue
		}

		ok, reason := d.tools.Available(m.Cap)
		out = append(out, DigitalModeStatus{Mode: m, Available: ok, Reason: reason})
	}

	return out
}

// Check returns the digital mode a listener asks for: unknown modes,
// service-only modes and modes whose capability is missing are refused.
func (d *Decoders) Check(name string) (domain.DigitalMode, error) {
	m, err := domain.DigitalModeOf(name)
	if err != nil {
		return domain.DigitalMode{}, err
	}

	if ok, reason := d.tools.Available(m.Cap); !ok {
		return domain.DigitalMode{}, domain.ErrDecoderUnavailable.WithDetail(m.Label + " is not available on this receiver: " + reason)
	}

	return m, nil
}

// Start starts a new session of m; events builds the receivers of its
// output for its session id.
func (d *Decoders) Start(m domain.DigitalMode, events func(id shared.UUID) DecoderEvents) (shared.UUID, DecoderRun, error) {
	id, err := d.ids.New(d.now())
	if err != nil {
		return shared.UUID{}, nil, fmt.Errorf("decoder session id: %w", err)
	}

	run, err := d.runner.Start(DecoderSpec{Session: id, Mode: m}, events(id))
	if err != nil {
		return shared.UUID{}, nil, fmt.Errorf("start %s decoder: %w", m.Name, err)
	}

	return id, run, nil
}
