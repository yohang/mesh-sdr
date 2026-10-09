package decodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// shell is a stub shell of the renderer.
type shell struct{}

func (shell) Shell(*http.Request) layout.Shell { return layout.Shell{SiteName: "Test"} }

type env struct {
	m         *Module
	db        *db.DB
	published []Message
	visible   []Device
	signedIn  bool
	now       time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()

	e := &env{db: dbtest.NewSQLite(t), now: t0, visible: []Device{{ID: "vhf", Name: "VHF"}}}
	e.m = New(Deps{
		DB:        e.db,
		Visible:   func(context.Context) ([]Device, error) { return e.visible, nil },
		SignedIn:  func(context.Context) bool { return e.signedIn },
		Retention: func() time.Duration { return 30 * 24 * time.Hour },
		MaxRows:   func() int { return 1000 },
		Published: func(_ context.Context, m Message) { e.published = append(e.published, m) },
		DeviceNode: func(_ context.Context, device string) (string, bool, error) {
			node, ok := map[string]string{"vhf": "n1", "hf": "n1", "hidden": "n1", "other": "n2"}[device]

			return node, ok, nil
		},
		Modes: []Mode{{ID: "selcall", Label: "SelCall"}, {ID: "zvei", Label: "ZVEI"}},
		Dedup: func(mode string) (int64, time.Duration) {
			if mode == "selcall" {
				return 1000, 10 * time.Second
			}

			return 0, 0
		},
		Render: render.New(shell{}, nil, slog.New(slog.DiscardHandler)), Now: func() time.Time { return e.now },
	})

	return e
}

func decode(device string, ts time.Time, text string) ctl.Decode {
	return ctl.Decode{
		DeviceID: device, SessionID: "0190c8a4-0000-7000-8000-000000000001", Mode: "selcall", Family: "paging", Freq: 145_500_000,
		TS: ts.UnixMilli(), Source: ctl.SourceListener, CID: "c1", Schema: "selcall.v1", Text: text,
		Payload: json.RawMessage(`{"decoder":"DTMF","digits":"1"}`),
	}
}

// ingest stores a batch in one transaction and flushes it, like the
// control channel ingestion.
func (e *env) ingest(t *testing.T, node string, decodes ...ctl.Decode) {
	t.Helper()

	raw, _ := json.Marshal(ctl.DecodeBatch{Seq: 1, Decodes: decodes})

	err := e.db.WithinTx(context.Background(), func(ctx context.Context) error { return e.m.Ingest(ctx, node, raw, e.now) })
	if err != nil {
		t.Fatal(err)
	}

	e.m.Flush(context.Background(), node)
}

// Two listeners decoding the same burst store it once (ADR 0028); invalid
// messages are skipped; stored ones are published after the commit.
func TestIngestDedup(t *testing.T) {
	e := newEnv(t)

	// Two listeners a few hertz apart, across a second boundary: one row.
	other := decode("vhf", t0.Add(1100*time.Millisecond), "[DTMF] 1")
	other.Freq += 4

	e.ingest(t, "n1", decode("vhf", t0.Add(900*time.Millisecond), "[DTMF] 1"), other)
	e.ingest(t, "n1", decode("vhf", t0.Add(12*time.Second), "[DTMF] 1"))

	// Another node's device, and an unknown one, are skipped.
	e.ingest(t, "n1", decode("other", t0, "[DTMF] 7"), decode("ghost", t0, "[DTMF] 8"))

	bad := decode("vhf", t0, "x")
	bad.Mode = "Bad Mode"
	badPayload := decode("vhf", t0, "y")
	badPayload.Payload = json.RawMessage(`"` + strings.Repeat("x", MaxPayload) + `"`)
	e.ingest(t, "n1", bad, badPayload, decode("hf", t0, "[DTMF] 2\x07\x00"))

	if len(e.published) != 3 || e.published[0].Text != "[DTMF] 1" || e.published[2].Text != "[DTMF] 2" || e.published[0].Origin != "listener" {
		t.Fatalf("published %+v", e.published)
	}

	rows, err := e.m.repo.List(context.Background(), Filter{Devices: []string{"vhf", "hf"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 3 || rows[0].DeviceID != "hf" || rows[0].NodeID != "n1" || rows[2].FreqHz != 145_500_000 {
		t.Errorf("rows %+v", rows)
	}

	// A decode time far ahead of the hub clock is clamped.
	e.ingest(t, "n1", decode("vhf", t0.Add(48*time.Hour), "[DTMF] 3"))

	if last := e.published[len(e.published)-1]; !last.DecodedAt.Equal(t0.Add(MaxAhead)) {
		t.Errorf("clamped time %v", last.DecodedAt)
	}

	// A payload deeper than SQLite takes is skipped, the batch goes on.
	deep := decode("vhf", t0, "[DTMF] 4")
	deep.Payload = json.RawMessage(strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1))
	e.ingest(t, "n1", deep, decode("vhf", t0, "[DTMF] 5"))

	if last := e.published[len(e.published)-1]; last.Text != "[DTMF] 5" || len(e.published) != 5 {
		t.Errorf("published after a deep payload %+v", e.published)
	}

	// A row the database refuses fails alone, inside the transaction.
	err = e.db.WithinTx(context.Background(), func(ctx context.Context) error {
		bad, _ := e.m.validRow("n1", decode("vhf", t0, "bad"), e.now)
		bad.Payload = json.RawMessage(`{`)

		if _, _, err := e.m.repo.Insert(ctx, bad); !errors.Is(err, ErrRejected) {
			t.Errorf("constraint: %v", err)
		}

		good, _ := e.m.validRow("n1", decode("vhf", t0, "good"), e.now)
		_, ok, err := e.m.repo.Insert(ctx, good)
		if err != nil || !ok {
			t.Errorf("after a constraint: %v %v", ok, err)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A rolled-back batch publishes nothing.
	raw, _ := json.Marshal(ctl.DecodeBatch{Decodes: []ctl.Decode{decode("vhf", t0.Add(time.Minute), "[DTMF] 9")}})
	_ = e.db.WithinTx(context.Background(), func(ctx context.Context) error {
		_ = e.m.Ingest(ctx, "n1", raw, e.now)

		return context.Canceled
	})
	e.m.Discard("n1")
	e.m.Flush(context.Background(), "n1")

	if len(e.published) != 5 {
		t.Errorf("published after rollback %+v", e.published)
	}
}

func TestPurge(t *testing.T) {
	e := newEnv(t)

	// Five old messages, received when they were decoded.
	for i := range 5 {
		at := t0.Add(-time.Duration(40-i) * 24 * time.Hour)
		e.now = at
		e.ingest(t, "n1", decode("vhf", at, "old"))
	}

	e.now = t0

	var batch []ctl.Decode
	for i := range 1005 {
		batch = append(batch, decode("vhf", t0.Add(time.Duration(i)*time.Second), fmt.Sprintf("new %d", i)))
	}

	e.ingest(t, "n1", batch...)

	n, err := e.m.PurgeOld(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if n != 10 {
		t.Errorf("deleted %d, want 5 old and 5 beyond the cap", n)
	}

	rows, _ := e.m.repo.List(context.Background(), Filter{Devices: []string{"vhf"}, Limit: 2000})
	if len(rows) != 1000 || rows[len(rows)-1].DecodedAt != t0.Add(5*time.Second) {
		t.Errorf("kept %d, oldest %v", len(rows), rows[len(rows)-1].DecodedAt)
	}
}

func get(t *testing.T, e *env, target string) (int, string) {
	t.Helper()

	rec := httptest.NewRecorder()
	e.m.listPage(rec, httptest.NewRequest(http.MethodGet, target, nil))

	return rec.Code, rec.Body.String()
}

// The page shows the devices the visitor may listen to, filters and pages
// them, and escapes the RF text.
func TestListPage(t *testing.T) {
	e := newEnv(t)

	var batch []ctl.Decode
	for i := range PageSize + 2 {
		batch = append(batch, decode("vhf", t0.Add(time.Duration(i)*time.Second), fmt.Sprintf("[DTMF] %d", i)))
	}

	batch = append(batch, decode("hidden", t0, "[DTMF] secret"), decode("vhf", t0.Add(time.Hour), "<b>x</b>"))
	e.ingest(t, "n1", batch...)

	code, body := get(t, e, Path)
	if code != http.StatusOK || strings.Contains(body, "secret") || !strings.Contains(body, "&lt;b&gt;x&lt;/b&gt;") ||
		!strings.Contains(body, "Older messages") || !strings.Contains(body, "kept for 30 days") {
		t.Fatalf("page %d %s", code, body)
	}

	if strings.Count(body, "align-top") != PageSize {
		t.Errorf("rows %d", strings.Count(body, "align-top"))
	}

	// A message that locates a station links to it on the map (MAP-016).
	e.m.d.MapLink = func(m Message) string {
		if m.Text == "<b>x</b>" {
			return "/map?callsign=F4ABC-9"
		}

		return ""
	}
	if _, body := get(t, e, Path); strings.Count(body, "Show on the map") != 1 || !strings.Contains(body, `<a href="/map?callsign=F4ABC-9"`) {
		t.Errorf("map link: %d", strings.Count(body, "Show on the map"))
	}

	e.m.d.MapLink = nil

	// The next page holds the rest, newest first.
	_, body = get(t, e, Path+"?page=2")
	if strings.Count(body, "align-top") != 3 || !strings.Contains(body, `<a href="/decodes">Newer messages</a>`) || strings.Contains(body, "Older messages") {
		t.Errorf("page 2: %s", body)
	}

	// A page past the end keeps the way back.
	_, body = get(t, e, Path+"?page=3")
	if !strings.Contains(body, "No decoded message matches.") || !strings.Contains(body, `<a href="/decodes?page=2">Newer messages</a>`) {
		t.Errorf("page 3: %s", body)
	}

	_, body = get(t, e, Path+"?to=2026-10-08T12:00&mode=selcall")
	if !strings.Contains(body, "No decoded message matches.") {
		t.Errorf("time filter: %s", body)
	}

	// The mode filter shows the catalogue labels, the ids are the values.
	if !strings.Contains(body, `<option value="selcall" selected>SelCall</option>`) ||
		!strings.Contains(body, `<option value="zvei">ZVEI</option>`) {
		t.Errorf("mode filter: %s", body)
	}

	if code, _ := get(t, e, Path+"?from=yesterday"); code != http.StatusBadRequest {
		t.Errorf("invalid from: %d", code)
	}

	e.visible = nil

	_, body = get(t, e, Path)
	if !strings.Contains(body, "Sign in to see") {
		t.Errorf("anonymous without device: %s", body)
	}
}

// Stored sees each stored message inside the ingestion transaction (the map
// projection): duplicates are not passed again, and its error rolls the
// whole batch back.
func TestIngestStored(t *testing.T) {
	e := newEnv(t)

	var stored []string

	fail := false
	e.m.d.Stored = func(ctx context.Context, m Message) error {
		if fail {
			return errors.New("map down")
		}

		var n int
		if err := e.db.Reader(ctx).QueryRowContext(ctx, `SELECT count(*) FROM decoded_messages WHERE id = ?`, m.ID).Scan(&n); err != nil || n != 1 {
			t.Errorf("message %d not visible in the transaction: %d %v", m.ID, n, err)
		}

		stored = append(stored, m.Text)

		return nil
	}

	e.ingest(t, "n1", decode("vhf", t0, "[DTMF] 1"), decode("vhf", t0, "[DTMF] 1"), decode("hf", t0, "[DTMF] 2"))

	if fmt.Sprint(stored) != "[[DTMF] 1 [DTMF] 2]" {
		t.Errorf("stored %v", stored)
	}

	fail = true
	raw, _ := json.Marshal(ctl.DecodeBatch{Seq: 2, Decodes: []ctl.Decode{decode("vhf", t0.Add(time.Minute), "[DTMF] 3")}})

	err := e.db.WithinTx(context.Background(), func(ctx context.Context) error { return e.m.Ingest(ctx, "n1", raw, e.now) })
	if err == nil {
		t.Fatal("batch committed despite the Stored error")
	}

	e.m.Discard("n1")

	rows, err := e.m.repo.List(context.Background(), Filter{Devices: []string{"vhf"}, Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Errorf("rows after a rollback %+v %v", rows, err)
	}
}
