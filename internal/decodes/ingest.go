package decodes

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Bounds of an ingested message.
const (
	MaxText    = 4096
	MaxPayload = 16 << 10
	// MaxDepth is the deepest JSON payload SQLite's JSON functions take.
	MaxDepth = 1000
	// MaxAhead and MaxBehind bound a decode time around the hub's receive
	// time: a node clock far off cannot keep messages from their retention
	// (MaxBehind leaves room for a node that buffered its events while the
	// hub was unreachable).
	MaxAhead  = time.Minute
	MaxBehind = 24 * time.Hour
)

// Default duplicate key rounding of a mode the catalogue does not know.
const (
	defaultDedupStep   = 10
	defaultDedupBucket = 10 * time.Second
)

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	modeName   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,23}$`)
	familyName = regexp.MustCompile(`^[a-z0-9_-]{1,16}$`)
	schemaName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)
)

// Ingest stores the messages of a decode.batch of a node, inside the
// ingestion transaction of ctx (DEC-047). Invalid messages, messages of
// another node's device and messages the database refuses are skipped and
// logged: only a database failure fails the batch. Duplicates (same
// device, mode, text, rounded frequency and time bucket, ADR 0028) are
// dropped. The stored ones are published by Flush after the commit.
func (m *Module) Ingest(ctx context.Context, node string, payload json.RawMessage, now time.Time) error {
	var batch ctl.DecodeBatch
	if err := json.Unmarshal(payload, &batch); err != nil {
		m.d.Logger.WarnContext(ctx, "invalid decode.batch skipped", slog.String("node_id", node), slog.Any("error", err))

		return nil
	}

	owners := map[string]bool{}

	for _, d := range batch.Decodes {
		r, reason := m.validRow(node, d, now)
		if reason != "" {
			m.d.Logger.WarnContext(ctx, "invalid decoded message skipped", slog.String("node_id", node), slog.String("reason", reason))

			continue
		}

		own, seen := owners[r.DeviceID]
		if !seen {
			owner, ok, err := m.d.DeviceNode(ctx, r.DeviceID)
			if err != nil {
				return err
			}

			own = ok && owner == node
			owners[r.DeviceID] = own
		}

		if !own {
			m.d.Logger.WarnContext(ctx, "decoded message of another node's device skipped",
				slog.String("node_id", node), slog.String("device_id", r.DeviceID))

			continue
		}

		id, inserted, err := m.repo.Insert(ctx, r)

		switch {
		case errors.Is(err, ErrRejected):
			m.d.Logger.WarnContext(ctx, "decoded message rejected", slog.String("node_id", node), slog.Any("error", err))

			continue
		case err != nil:
			return err
		case !inserted:
			continue
		}

		r.ID = id

		m.mu.Lock()
		m.pending[node] = append(m.pending[node], r.Message)
		m.mu.Unlock()
	}

	return nil
}

// Flush publishes the messages of node stored by the last committed batch.
func (m *Module) Flush(ctx context.Context, node string) {
	m.mu.Lock()
	msgs := m.pending[node]
	delete(m.pending, node)
	m.mu.Unlock()

	if m.d.Published == nil {
		return
	}

	for _, msg := range msgs {
		m.d.Published(ctx, msg)
	}
}

// Discard forgets the messages of node of a batch that was rolled back.
func (m *Module) Discard(node string) {
	m.mu.Lock()
	delete(m.pending, node)
	m.mu.Unlock()
}

// validRow checks a decoded message of a node and returns its row, or why
// it is refused.
func (m *Module) validRow(node string, d ctl.Decode, now time.Time) (row, string) {
	var r row

	switch {
	case !identifier.MatchString(d.DeviceID):
		return r, "invalid device_id"
	case !modeName.MatchString(d.Mode):
		return r, "invalid mode"
	case !familyName.MatchString(d.Family):
		return r, "invalid family"
	case !schemaName.MatchString(d.Schema):
		return r, "invalid schema"
	case d.TS <= 0:
		return r, "invalid ts"
	case d.Freq < 0:
		return r, "invalid freq"
	case len(d.Payload) > MaxPayload || !json.Valid(d.Payload):
		return r, "invalid payload"
	case jsonDepth(d.Payload) > MaxDepth:
		return r, "payload too deep"
	}

	origin := "listener"

	switch d.Source {
	case ctl.SourceListener:
	case ctl.SourceBackground:
		origin = "service"
	default:
		return r, "invalid source"
	}

	at := time.UnixMilli(d.TS).UTC()

	switch {
	case at.Before(now.Add(-MaxBehind)):
		at = now.Add(-MaxBehind).UTC()
	case at.After(now.Add(MaxAhead)):
		at = now.Add(MaxAhead).UTC()
	}

	r = row{
		Message: Message{
			DecodedAt: at, NodeID: node, DeviceID: d.DeviceID, Origin: origin, Mode: d.Mode,
			Family: d.Family, FreqHz: d.Freq, Text: cleanText(d.Text), Schema: d.Schema, Payload: d.Payload,
		},
		ReceivedAt: now,
	}

	if d.PresetID != "" {
		u, err := shared.ParseUUID(d.PresetID)
		if err != nil {
			return row{}, "invalid preset_id"
		}

		r.PresetID = u.Bytes()
	}

	if d.SessionID != "" {
		u, err := shared.ParseUUID(d.SessionID)
		if err != nil {
			return row{}, "invalid session_id"
		}

		r.SessionID = u.Bytes()
	}

	step, bucket := int64(defaultDedupStep), defaultDedupBucket
	if m.d.Dedup != nil {
		if s, b := m.d.Dedup(r.Mode); s > 0 && b > 0 {
			step, bucket = s, b
		}
	}

	r.DedupKey = DedupKey(r.DeviceID, r.Mode, r.Text, round(r.FreqHz, step), r.DecodedAt.UnixMilli()/bucket.Milliseconds())

	return r, ""
}

// round rounds hz to the nearest multiple of step.
func round(hz, step int64) int64 { return (hz + step/2) / step * step }

// DedupKey identifies a message by its content (ADR 0028): the first 16
// bytes of SHA-256 of the device, mode, text, rounded frequency and time
// bucket.
func DedupKey(device, mode, text string, freqHz, bucket int64) []byte {
	h := sha256.New()

	for _, s := range []string{device, mode, text} {
		_ = binary.Write(h, binary.BigEndian, uint32(len(s)))
		h.Write([]byte(s))
	}

	_ = binary.Write(h, binary.BigEndian, freqHz)
	_ = binary.Write(h, binary.BigEndian, bucket)

	return h.Sum(nil)[:16]
}

// jsonDepth returns the nesting depth of a valid JSON document.
func jsonDepth(b []byte) int {
	depth, deepest, inString, escaped := 0, 0, false, false

	for _, c := range b {
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{' || c == '[':
			depth++
			deepest = max(deepest, depth)
		case c == '}' || c == ']':
			depth--
		}
	}

	return deepest
}

// cleanText sanitises RF text again on the hub: valid UTF-8, no control
// characters but \n and \t, at most MaxText bytes.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}

		return -1
	}, s)

	if len(s) <= MaxText {
		return s
	}

	n := MaxText
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
