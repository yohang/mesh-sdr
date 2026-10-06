package http

import (
	"encoding/csv"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/app"
)

// AuditPath is Admin › Audit log (ACC-010).
const AuditPath = "/admin/audit"

const pageTitleAudit = "Audit log"

// auditForm is the filter form as typed (dates YYYY-MM-DD, UTC).
type auditForm struct {
	Actor, Action, TargetType, TargetID, From, To string
}

func readAuditForm(v url.Values) auditForm {
	return auditForm{
		Actor: v.Get("actor"), Action: v.Get("action"), TargetType: v.Get("target_type"), TargetID: v.Get("target_id"),
		From: v.Get("from"), To: v.Get("to"),
	}
}

func (f auditForm) values() url.Values {
	v := url.Values{}

	for k, s := range map[string]string{"actor": f.Actor, "action": f.Action, "target_type": f.TargetType, "target_id": f.TargetID, "from": f.From, "to": f.To} {
		if s != "" {
			v.Set(k, s)
		}
	}

	return v
}

// filter converts the form; an invalid date is ignored. "to" includes the
// whole day.
func (f auditForm) filter() app.AuditFilter {
	out := app.AuditFilter{
		Actor: strings.TrimSpace(f.Actor), ActionPrefix: strings.TrimSpace(f.Action),
		TargetType: strings.TrimSpace(f.TargetType), TargetID: strings.TrimSpace(f.TargetID),
	}

	if t, err := time.Parse(time.DateOnly, f.From); err == nil {
		out.From = t
	}

	if t, err := time.Parse(time.DateOnly, f.To); err == nil {
		out.To = t.Add(24 * time.Hour)
	}

	return out
}

// auditView is the audit page.
type auditView struct {
	Form   auditForm
	Rows   []app.AuditRow
	NextQS string
	Export string
}

func (m *Module) auditPage(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	q := r.URL.Query()
	form := readAuditForm(q)
	f := form.filter()
	f.BeforeID, _ = strconv.ParseInt(q.Get("before"), 10, 64)

	rows, next, err := m.audit.Search(r.Context(), f)
	if err != nil {
		m.pageFailed(w, r, err)

		return
	}

	v := auditView{Form: form, Rows: rows, Export: form.values().Encode()}

	if next > 0 {
		nv := form.values()
		nv.Set("before", strconv.FormatInt(next, 10))
		v.NextQS = nv.Encode()
	}

	m.pages.Page(w, r, http.StatusOK, pageTitleAudit, auditPage(v), nil)
}

// details renders before/after maps as "key=value" pairs, sorted.
func details(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}

	return strings.Join(parts, " ")
}

// csvCell neutralises spreadsheet formulas (CSV injection): a cell that
// starts with = + - @ or a control character gets a leading quote.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}

	return s
}

func (m *Module) auditExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := readAuditForm(q).filter()
	format := q.Get("format")

	if format != "csv" && format != "json" {
		m.pages.Error(w, r, http.StatusBadRequest)

		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-log.`+format+`"`)

	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")

		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"id", "at", "actor_kind", "actor", "actor_ip", "action", "target_type", "target_id", "target", "result", "before", "after", "request_id"})

		err := m.audit.Each(r.Context(), f, func(row app.AuditRow) error {
			e := row.Entry
			ip := ""

			if e.Actor().IP().IsValid() {
				ip = e.Actor().IP().String()
			}

			cells := []string{
				strconv.FormatInt(row.ID, 10), e.At().UTC().Format(time.RFC3339Nano), string(e.Actor().Kind()), row.ActorName, ip,
				e.Action(), e.TargetType(), e.TargetID(), row.TargetName, string(e.Result()), details(e.Before()), details(e.After()), e.RequestID(),
			}

			for i := range cells {
				cells[i] = csvCell(cells[i])
			}

			return cw.Write(cells)
		})
		if err != nil {
			m.logger.ErrorContext(r.Context(), "audit export failed", slog.Any("error", err))
		}

		cw.Flush()

		return
	}

	w.Header().Set("Content-Type", "application/json")

	enc := json.NewEncoder(w)
	_, _ = w.Write([]byte("[\n"))
	first := true

	err := m.audit.Each(r.Context(), f, func(row app.AuditRow) error {
		if !first {
			if _, err := w.Write([]byte(",\n")); err != nil {
				return err
			}
		}

		first = false

		return enc.Encode(auditJSON(row))
	})
	if err != nil {
		m.logger.ErrorContext(r.Context(), "audit export failed", slog.Any("error", err))
	}

	_, _ = w.Write([]byte("]\n"))
}

// AuditJSON is the JSON form of an audit entry (export and API).
type AuditJSON struct {
	ID         int64             `json:"id"`
	At         time.Time         `json:"at"`
	ActorKind  string            `json:"actor_kind"`
	ActorID    string            `json:"actor_id,omitempty"`
	Actor      string            `json:"actor"`
	ActorIP    string            `json:"actor_ip,omitempty"`
	Action     string            `json:"action"`
	TargetType string            `json:"target_type,omitempty"`
	TargetID   string            `json:"target_id,omitempty"`
	Target     string            `json:"target,omitempty"`
	Result     string            `json:"result"`
	Before     map[string]string `json:"before,omitempty"`
	After      map[string]string `json:"after,omitempty"`
	RequestID  string            `json:"request_id,omitempty"`
}

func auditJSON(row app.AuditRow) AuditJSON {
	e := row.Entry
	out := AuditJSON{
		ID: row.ID, At: e.At().UTC(), ActorKind: string(e.Actor().Kind()), Actor: row.ActorName, Action: e.Action(),
		TargetType: e.TargetType(), TargetID: e.TargetID(), Target: row.TargetName, Result: string(e.Result()),
		Before: e.Before(), After: e.After(), RequestID: e.RequestID(),
	}

	if id := e.Actor().UserID(); !id.IsZero() {
		out.ActorID = id.String()
	}

	if e.Actor().IP().IsValid() {
		out.ActorIP = e.Actor().IP().String()
	}

	return out
}
