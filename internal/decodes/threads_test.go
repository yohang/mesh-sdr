package decodes

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestThreads(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	js8 := func(id int64, sec int, df int64, thread int, text, call string) Message {
		p, _ := json.Marshal(map[string]any{"df": df, "submode": "A", "thread_type": thread, "callsign": call})

		return Message{
			ID: id, DecodedAt: t0.Add(time.Duration(sec) * time.Second), NodeID: "n1", DeviceID: "hf", Mode: "js8", Schema: js8Schema,
			Text: text, Payload: p, FreqHz: 7_078_000 + df,
		}
	}

	other := Message{ID: 4, DecodedAt: t0.Add(20 * time.Second), DeviceID: "hf", Mode: "ft8", Schema: "wsjt.v1", Text: "CQ K1ABC FN42", Payload: []byte(`{}`)}

	// Newest first, as the repository lists them.
	rows := []Message{
		js8(7, 60, 900, 0, "AGAIN", ""),
		js8(6, 45, 1503, 2, "TODAY ", ""),
		js8(5, 30, 1499, 0, "ITALY ", ""),
		other,
		js8(3, 15, 1500, 1, "IN ", "G0CQZ"),
		js8(2, 15, 2000, 3, "LZ1CWK: CQ CQ CQ KN32", "LZ1CWK"),
	}

	// Another node on the same frequency is another thread.
	elsewhere := js8(8, 70, 1499, 0, "ELSEWHERE", "")
	elsewhere.NodeID = "n2"
	rows = append([]Message{elsewhere}, rows...)

	got := Threads(rows)

	var lines []string
	for _, e := range got {
		lines = append(lines, e.Text+"|"+strings.Join(e.Calls, ",")+"|"+map[bool]string{true: "open", false: "closed"}[e.Open])
	}

	want := []string{
		"ELSEWHERE||open",
		"AGAIN||open",
		"IN ITALY TODAY|G0CQZ|closed",
		"CQ K1ABC FN42||closed",
		"LZ1CWK: CQ CQ CQ KN32|LZ1CWK|closed",
	}

	if !slices.Equal(lines, want) {
		t.Errorf("got %q\nwant %q", lines, want)
	}

	if got[2].Frames != 3 || got[2].ID != 6 || !got[2].DecodedAt.Equal(t0.Add(15*time.Second)) || got[3].Frames != 0 {
		t.Errorf("thread %+v", got[2])
	}
}
