package agent_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/agent"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

func newOutbox(t *testing.T) (*agent.Outbox, *agent.Buffer, string) {
	t.Helper()

	return newOutboxOf(t, 10_000)
}

// newOutboxOf returns an outbox over an event buffer of maxEvents.
func newOutboxOf(t *testing.T, maxEvents int) (*agent.Outbox, *agent.Buffer, string) {
	t.Helper()

	buf := agent.NewBuffer(maxEvents, 1<<20)

	a, err := agent.New(agent.Options{NodeID: "attic", Buffer: buf, Now: time.Now, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "outbox")

	return agent.NewOutbox(dir, a, time.Now, slog.New(slog.DiscardHandler)), buf, dir
}

func produced(t *testing.T, content []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

var start = time.Date(2026, 10, 6, 14, 32, 5, 0, time.UTC)

func sstvMeta() agent.FileMeta {
	return agent.FileMeta{
		Kind: agent.FileKindSSTV, DeviceID: "hf", Mode: "sstv", FrequencyHz: 14_230_000, ReceivedStart: start,
		ReceivedEnd: start.Add(110 * time.Second), Metadata: map[string]any{"sstv_mode": "Scottie 1", "vis_code": 60},
	}
}

// encode encodes a buffered event payload as it is sent.
func encode(t *testing.T, e agent.Event) []byte {
	t.Helper()

	b, err := json.Marshal(e.Payload)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// TestOutboxSendsAFile covers FIL-005 on the node: file.begin with the
// FIL-008 metadata, the content in base64 chunks of FileChunkBytes read when
// sent, file.end; the node deletes its copy once the hub acknowledges
// file.end.
func TestOutboxSendsAFile(t *testing.T) {
	o, buf, dir := newOutbox(t)
	content := bytes.Repeat([]byte("0123456789"), 10_000) // 100 000 bytes: 3 chunks
	src := produced(t, content)

	if err := o.SendFile(context.Background(), sstvMeta(), src); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the produced file stays where the decoder wrote it: %v", err)
	}

	events := buf.After(0)
	if len(events) != 5 || events[0].Type != rxv1.TypeFileBegin || events[4].Type != rxv1.TypeFileEnd {
		t.Fatalf("events = %v", events)
	}

	var begin ctl.FileBegin
	if err := json.Unmarshal(encode(t, events[0]), &begin); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(content)
	if begin.Seq != events[0].Seq || begin.Kind != "sstv" || begin.MIME != "image/png" || begin.Size != int64(len(content)) ||
		begin.SHA256 != hex.EncodeToString(sum[:]) || begin.DeviceID != "hf" || begin.FrequencyHz != 14_230_000 ||
		begin.ReceivedStartUTC != "2026-10-06T14:32:05.000Z" || begin.ReceivedEndUTC != "2026-10-06T14:33:55.000Z" ||
		string(begin.Metadata) != `{"sstv_mode":"Scottie 1","vis_code":60}` {
		t.Errorf("file.begin = %+v", begin)
	}

	var got []byte

	for i, e := range events[1:4] {
		var c ctl.FileChunk
		if err := json.Unmarshal(encode(t, e), &c); err != nil {
			t.Fatal(err)
		}

		data, err := base64.StdEncoding.DecodeString(c.DataB64)
		if err != nil || c.FileID != begin.FileID || c.Offset != int64(len(got)) || c.Seq != e.Seq {
			t.Fatalf("chunk %d = %+v, %v", i, c, err)
		}

		if i < 2 && len(data) != ctl.FileChunkBytes {
			t.Errorf("chunk %d has %d bytes", i, len(data))
		}

		got = append(got, data...)
	}

	if !bytes.Equal(got, content) {
		t.Error("the chunks do not carry the content")
	}

	// The encoded chunk stays under the control channel's inbound limit.
	if n := len(encode(t, events[1])); n > rxv1.MaxInboundControlTextBytes-512 {
		t.Errorf("a full chunk encodes to %d bytes", n)
	}

	if files, bytes := o.Pending(); files != 1 || bytes != int64(len(content)) {
		t.Errorf("pending = %d files, %d bytes", files, bytes)
	}

	// An acknowledgement before file.end keeps the copy, then file.end's
	// acknowledgement deletes it.
	buf.Ack(events[3].Seq)

	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("outbox before file.end ack = %v", entries)
	}

	buf.Ack(events[4].Seq)

	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("outbox after the ack = %v", entries)
	}

	if files, bytes := o.Pending(); files != 0 || bytes != 0 {
		t.Errorf("pending after the ack = %d files, %d bytes", files, bytes)
	}
}

// TestOutboxRefuses: a file breaking a rule is refused and deleted.
func TestOutboxRefuses(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		meta func(m *agent.FileMeta)
		size int
		want error
	}{
		{"unknown kind", func(m *agent.FileMeta) { m.Kind = "recording" }, 10, agent.ErrInvalidFile},
		{"no frequency", func(m *agent.FileMeta) { m.FrequencyHz = 0 }, 10, agent.ErrInvalidFile},
		{"no start", func(m *agent.FileMeta) { m.ReceivedStart = time.Time{} }, 10, agent.ErrInvalidFile},
		{"end before start", func(m *agent.FileMeta) { m.ReceivedEnd = start.Add(-time.Second) }, 10, agent.ErrInvalidFile},
		{"invalid device", func(m *agent.FileMeta) { m.DeviceID = "../hf" }, 10, agent.ErrInvalidFile},
		{"invalid preset", func(m *agent.FileMeta) { m.PresetID = "x" }, 10, agent.ErrInvalidFile},
		{"empty", func(*agent.FileMeta) {}, 0, agent.ErrInvalidFile},
		{"over 8 MiB", func(*agent.FileMeta) {}, agent.MaxFileBytes + 1, agent.ErrFileTooBig},
		{"FAX over 16 MiB", func(m *agent.FileMeta) { m.Kind = agent.FileKindFAX; m.Mode = "fax" }, agent.MaxFAXBytes + 1, agent.ErrFileTooBig},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, buf, _ := newOutbox(t)
			m := sstvMeta()
			tt.meta(&m)
			src := produced(t, make([]byte, tt.size))

			if err := o.SendFile(ctx, m, src); !errors.Is(err, tt.want) {
				t.Errorf("SendFile = %v, want %v", err, tt.want)
			}

			if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("refused file kept: %v", err)
			}

			if buf.Len() != 0 {
				t.Errorf("%d events buffered", buf.Len())
			}
		})
	}

	t.Run("not a regular file", func(t *testing.T) {
		o, _, _ := newOutbox(t)
		if err := o.SendFile(ctx, sstvMeta(), t.TempDir()); !errors.Is(err, agent.ErrInvalidFile) {
			t.Errorf("SendFile(dir) = %v", err)
		}
	})

	t.Run("outbox full", func(t *testing.T) {
		o, _, _ := newOutbox(t)
		m := sstvMeta()
		m.Kind, m.Mode = agent.FileKindFAX, "fax"

		for range agent.MaxOutboxBytes / agent.MaxFAXBytes {
			if err := o.SendFile(ctx, m, produced(t, make([]byte, agent.MaxFAXBytes))); err != nil {
				t.Fatal(err)
			}
		}

		if err := o.SendFile(ctx, m, produced(t, []byte("x"))); !errors.Is(err, agent.ErrOutboxFull) {
			t.Errorf("SendFile over the outbox bound = %v", err)
		}
	})

	t.Run("metadata over 4 KiB", func(t *testing.T) {
		o, buf, _ := newOutbox(t)
		m := sstvMeta()
		m.Metadata = map[string]any{"note": strings.Repeat("x", ctl.MaxFileMetadata)}

		if err := o.SendFile(ctx, m, produced(t, []byte("x"))); !errors.Is(err, agent.ErrInvalidFile) || buf.Len() != 0 {
			t.Errorf("SendFile = %v, %d events", err, buf.Len())
		}
	})

	t.Run("event buffer budget", func(t *testing.T) {
		// 100 events: the files may take 50; a 3 MiB file needs 2 + 69.
		o, buf, _ := newOutboxOf(t, 100)

		if err := o.SendFile(ctx, sstvMeta(), produced(t, make([]byte, 3<<20))); !errors.Is(err, agent.ErrOutboxFull) || buf.Len() != 0 {
			t.Errorf("SendFile = %v, %d events", err, buf.Len())
		}

		if err := o.SendFile(ctx, sstvMeta(), produced(t, make([]byte, 1<<20))); err != nil {
			t.Errorf("a file within the budget: %v", err)
		}
	})
}

// TestOutboxEventsFitTheControlChannel: the largest file.begin and a full
// file.chunk stay under the control channel's inbound limit.
func TestOutboxEventsFitTheControlChannel(t *testing.T) {
	o, buf, _ := newOutbox(t)
	m := sstvMeta()
	m.DeviceID, m.Mode = strings.Repeat("d", 63), strings.Repeat("m", 24)
	m.PresetID, m.DecoderSessionID = "0199a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b", "0199a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2c"
	m.Metadata = map[string]any{"note": strings.Repeat("\"", ctl.MaxFileMetadata/2-20)}

	if err := o.SendFile(context.Background(), m, produced(t, make([]byte, ctl.FileChunkBytes+1))); err != nil {
		t.Fatal(err)
	}

	for _, e := range buf.After(0) {
		env, err := rxv1.NewEnvelope(e.Type, rxv1.CorrelationID{}, time.Now().UnixMilli(), e.Payload)
		if err != nil {
			t.Fatal(err)
		}

		raw, err := env.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}

		if len(raw) > rxv1.MaxInboundControlTextBytes {
			t.Errorf("%s encodes to %d bytes", e.Type, len(raw))
		}
	}
}
