package decodes

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
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
)

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	modeName   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,23}$`)
	familyName = regexp.MustCompile(`^[a-z0-9_-]{1,16}$`)
	schemaName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)
)

// Ingest stores the messages of a decode.batch of a node, inside the
// ingestion transaction of ctx (DEC-047). Invalid messages are skipped and
// logged; duplicates (same device, mode, second, frequency and text, ADR
// 0028) are dropped. The stored ones are published by Flush after the
// commit.
func (m *Module) Ingest(ctx context.Context, node string, payload json.RawMessage, now time.Time) error {
	var batch ctl.DecodeBatch
	if err := json.Unmarshal(payload, &batch); err != nil {
		m.d.Logger.WarnContext(ctx, "invalid decode.batch skipped", slog.String("node_id", node), slog.Any("error", err))

		return nil
	}

	for _, d := range batch.Decodes {
		r, reason := validRow(node, d, now)
		if reason != "" {
			m.d.Logger.WarnContext(ctx, "invalid decoded message skipped", slog.String("node_id", node), slog.String("reason", reason))

			continue
		}

		id, inserted, err := m.repo.Insert(ctx, r)
		if err != nil {
			return err
		}

		if !inserted {
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
func validRow(node string, d ctl.Decode, now time.Time) (row, string) {
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
	}

	origin := "listener"

	switch d.Source {
	case ctl.SourceListener:
	case ctl.SourceBackground:
		origin = "service"
	default:
		return r, "invalid source"
	}

	r = row{
		Message: Message{
			DecodedAt: time.UnixMilli(d.TS).UTC(), NodeID: node, DeviceID: d.DeviceID, Origin: origin, Mode: d.Mode,
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

	r.DedupKey = DedupKey(r.DeviceID, r.Mode, r.DecodedAt, r.FreqHz, r.Text)

	return r, ""
}

// DedupKey identifies a message by its content (ADR 0028): the first 16
// bytes of SHA-256 of the device, mode, second of decoding (the slot start
// for slot modes), frequency and text.
func DedupKey(device, mode string, at time.Time, freqHz int64, text string) []byte {
	h := sha256.New()

	for _, s := range []string{device, mode} {
		_ = binary.Write(h, binary.BigEndian, uint32(len(s)))
		h.Write([]byte(s))
	}

	_ = binary.Write(h, binary.BigEndian, at.Unix())
	_ = binary.Write(h, binary.BigEndian, freqHz)
	h.Write([]byte(text))

	return h.Sum(nil)[:16]
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
