// Package decodes keeps the decoded messages of the nodes (DEC-047, ADR
// 0028): the hub is the only writer of decoded_messages, from the node
// decode.batch events, and drops duplicates by a content hash. It serves the
// Decodes page (FEATURE_SPEC §10.11: a paginated table with mode, device
// and time filters; rights follow the listen policy) and the retention job.
package decodes

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// Message is one stored decoded message.
type Message struct {
	ID        int64
	DecodedAt time.Time
	NodeID    string
	DeviceID  string
	// Origin is listener, service or mqtt.
	Origin string
	Mode   string
	Family string
	// FreqHz is the absolute RF frequency (0: unknown).
	FreqHz int64
	// Text is the plain-text rendering: untrusted RF text, at most 4 KiB.
	Text    string
	Schema  string
	Payload json.RawMessage
}

// Device is a device whose messages a visitor may see.
type Device struct {
	ID   string
	Name string
}

// Mode is a digital mode of the catalogue: its id (the stored mode, the
// filter value) and its label for people.
type Mode struct {
	ID    string
	Label string
}

// Deps are the dependencies of the module.
type Deps struct {
	DB *db.DB
	// Visible returns the enabled devices the visitor of ctx may listen
	// to (listen policy), by registry order: the messages they see.
	Visible func(ctx context.Context) ([]Device, error)
	// SignedIn reports whether the visitor of ctx is signed in.
	SignedIn func(ctx context.Context) bool
	// Retention returns retention.decoded_messages.max_age.
	Retention func() time.Duration
	// MaxRows returns retention.decoded_messages.max_rows.
	MaxRows func() int
	// Published, when set, is told every stored message after its commit
	// (decode.new).
	Published func(ctx context.Context, m Message)
	// DeviceNode returns the node of a device of the registry (ok false:
	// unknown device). A node stores messages of its own devices only.
	DeviceNode func(ctx context.Context, device string) (node string, ok bool, err error)
	// Modes are the digital modes of the catalogue (the mode filter).
	Modes []Mode
	// Dedup returns the duplicate key rounding of a mode: its frequency
	// step (Hz) and time bucket (ADR 0028).
	Dedup  func(mode string) (step int64, bucket time.Duration)
	Render *render.Renderer
	Now    func() time.Time
	Logger *slog.Logger
}

// Module is the decodes module (an internal/http.Module).
type Module struct {
	d    Deps
	repo *Repository

	mu      sync.Mutex
	pending map[string][]Message
}

// New returns the module.
func New(d Deps) *Module {
	if d.Now == nil {
		d.Now = time.Now
	}

	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}

	return &Module{d: d, repo: NewRepository(d.DB), pending: map[string][]Message{}}
}
