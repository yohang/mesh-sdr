package decodes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite/sqlc"
)

// Repository stores the decoded messages (decoded_messages).
type Repository struct{ db *db.DB }

// NewRepository returns the repository.
func NewRepository(d *db.DB) *Repository { return &Repository{db: d} }

// row is a message to insert.
type row struct {
	Message
	ReceivedAt time.Time
	PresetID   []byte
	SessionID  []byte
	DedupKey   []byte
}

// Insert stores a message; inserted is false for a duplicate (same
// dedup_key).
func (r *Repository) Insert(ctx context.Context, m row) (id int64, inserted bool, err error) {
	p := sqlc.InsertDecodedMessageParams{
		DecodedAt: m.DecodedAt.UnixMilli(), ReceivedAt: m.ReceivedAt.UnixMilli(),
		NodeID: nullString(m.NodeID), DeviceID: nullString(m.DeviceID), PresetID: m.PresetID, DecoderSessionID: m.SessionID,
		Origin: m.Origin, Mode: m.Mode, Family: m.Family, Text: nullString(m.Text),
		Payload: string(m.Payload), PayloadSchema: m.Schema, DedupKey: m.DedupKey,
	}

	if m.FreqHz > 0 {
		p.Frequency = sql.NullInt64{Int64: m.FreqHz, Valid: true}
	}

	id, err = sqlc.New(r.db.Writer(ctx)).InsertDecodedMessage(ctx, p)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("insert decoded message: %w", err)
	}

	return id, true, nil
}

// Filter selects decoded messages.
type Filter struct {
	// Devices are the devices the visitor may see; none: no message.
	Devices []string
	// Mode and Device narrow the list ("" for any).
	Mode, Device string
	// From and Until bound the decode time [From, Until); zero: open.
	From, Until time.Time
	// Before is the id the page starts below (0: the newest).
	Before int64
	Limit  int
}

// List returns the messages of f, newest first.
func (r *Repository) List(ctx context.Context, f Filter) ([]Message, error) {
	if len(f.Devices) == 0 {
		return []Message{}, nil
	}

	devices, err := json.Marshal(f.Devices)
	if err != nil {
		return nil, err
	}

	p := sqlc.ListDecodedMessagesParams{
		DevicesJson: string(devices),
		Mode:        f.Mode, Device: f.Device, FromMs: math.MinInt64, ToMs: math.MaxInt64, BeforeID: math.MaxInt64, MaxRows: int64(f.Limit),
	}

	if !f.From.IsZero() {
		p.FromMs = f.From.UnixMilli()
	}

	if !f.Until.IsZero() {
		p.ToMs = f.Until.UnixMilli()
	}

	if f.Before > 0 {
		p.BeforeID = f.Before
	}

	rows, err := sqlc.New(r.db.Reader(ctx)).ListDecodedMessages(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("list decoded messages: %w", err)
	}

	out := make([]Message, 0, len(rows))
	for _, x := range rows {
		out = append(out, Message{
			ID: x.ID, DecodedAt: time.UnixMilli(x.DecodedAt).UTC(), NodeID: x.NodeID.String, DeviceID: x.DeviceID.String,
			Origin: x.Origin, Mode: x.Mode, Family: x.Family, FreqHz: x.Frequency.Int64, Text: x.Text.String,
			Schema: x.PayloadSchema, Payload: json.RawMessage(x.Payload),
		})
	}

	return out, nil
}

// Modes returns the modes of the stored messages.
func (r *Repository) Modes(ctx context.Context) ([]string, error) {
	modes, err := sqlc.New(r.db.Reader(ctx)).ListDecodedModes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list decoded modes: %w", err)
	}

	return modes, nil
}

// purgeBatch bounds each delete of the retention job.
const purgeBatch = 10_000

// Purge deletes the messages decoded before before, then the oldest beyond
// the newest maxRows, in batches; it returns the rows deleted.
func (r *Repository) Purge(ctx context.Context, before time.Time, maxRows int) (int64, error) {
	q := sqlc.New(r.db.Writer(ctx))

	var total int64

	for {
		n, err := q.DeleteDecodedBefore(ctx, sqlc.DeleteDecodedBeforeParams{BeforeMs: before.UnixMilli(), MaxRows: purgeBatch})
		if err != nil {
			return total, fmt.Errorf("purge decoded messages: %w", err)
		}

		total += n

		if n < purgeBatch {
			break
		}
	}

	for {
		n, err := q.DeleteDecodedBeyond(ctx, sqlc.DeleteDecodedBeyondParams{Keep: int64(maxRows), MaxRows: purgeBatch})
		if err != nil {
			return total, fmt.Errorf("cap decoded messages: %w", err)
		}

		total += n

		if n < purgeBatch {
			return total, nil
		}
	}
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
