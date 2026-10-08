package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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
	// Live records (the rows of an image) go to the listener only: the hub
	// does not keep them.
	Live bool
}

// File kinds of the decoders (FIL-005).
const (
	FileSSTV = "sstv"
	FileFAX  = "fax"
)

// ProducedFile is a file a decoder session produced for Files (FIL-005):
// an SSTV or FAX image as PNG, with its reception metadata (FIL-008).
type ProducedFile struct {
	Kind string
	Data []byte
	// Start is the reception start; End is zero when the reception was
	// cut short.
	Start, End time.Time
	// Metadata are the per-kind details (SSTV mode, VIS code, FAX LPM…).
	Metadata map[string]any

	// The stream handler adds where the file comes from.
	DeviceID, PresetID, SessionID, Mode string
	// FreqHz is the dial frequency when the reception started.
	FreqHz int64
}

// FilePublisher sends the files of the decoders to the hub: it queues the
// file (disk I/O, no network) and reports whether it could. It is called
// from the decoder goroutines.
type FilePublisher interface {
	Produced(f ProducedFile) error
}

// DecoderEvents receive the output of a decoder session. They are called
// from the session's goroutines and must not block.
type DecoderEvents struct {
	Decode func(DecodeRecord)
	Status func(DecoderStatus)
	// File receives the files of the session, even after Close (the image
	// in progress), and reports whether the file was queued for the hub.
	File func(ProducedFile) error
}

// DecoderSpec describes a decoder session to start.
type DecoderSpec struct {
	// Session is the decoder session id (decoder_session_id).
	Session shared.UUID
	Mode    domain.DigitalMode
	// Variant is the decoder variant of the mode ("" for none).
	Variant string
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
	max    int

	mu      sync.Mutex
	running int
}

// NewDecoders returns the service; maxSessions caps the decoder sessions
// of the node (decoders.max_sessions, 0: no cap).
func NewDecoders(tools DecoderTools, runner DecoderRunner, maxSessions int, now func() time.Time) *Decoders {
	return &Decoders{tools: tools, runner: runner, ids: shared.NewUUIDv7Generator(), now: now, max: maxSessions}
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

// ErrNodeBusy refuses a decoder session beyond decoders.max_sessions.
var ErrNodeBusy = errors.New("node busy")

// Start starts a new session of m with a variant of the mode (already
// checked); events builds the receivers of its output for its session id.
// Beyond the node's session cap it returns ErrNodeBusy.
func (d *Decoders) Start(m domain.DigitalMode, variant string, events func(id shared.UUID) DecoderEvents) (shared.UUID, DecoderRun, error) {
	d.mu.Lock()
	busy := d.max > 0 && d.running >= d.max
	if !busy {
		d.running++
	}
	d.mu.Unlock()

	if busy {
		return shared.UUID{}, nil, ErrNodeBusy
	}

	id, err := d.ids.New(d.now())
	if err != nil {
		d.release()

		return shared.UUID{}, nil, fmt.Errorf("decoder session id: %w", err)
	}

	run, err := d.runner.Start(DecoderSpec{Session: id, Mode: m, Variant: variant}, events(id))
	if err != nil {
		d.release()

		return shared.UUID{}, nil, fmt.Errorf("start %s decoder: %w", m.Name, err)
	}

	return id, &countedRun{DecoderRun: run, release: d.release}, nil
}

func (d *Decoders) release() {
	d.mu.Lock()
	d.running--
	d.mu.Unlock()
}

// countedRun gives its session slot back once, on Close.
type countedRun struct {
	DecoderRun

	once    sync.Once
	release func()
}

func (r *countedRun) Close() {
	r.once.Do(func() {
		r.DecoderRun.Close()
		r.release()
	})
}
