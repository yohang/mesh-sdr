package api

import (
	"context"
	"net/http"

	"github.com/yohang/mesh-sdr/internal/identity/app"
)

// AuditService reads the audit log for the API.
type AuditService interface {
	Search(ctx context.Context, f app.AuditFilter) ([]app.AuditRow, int64, error)
}

// AuditHandlers serve /audit.
type AuditHandlers struct{ audit AuditService }

// NewAuditHandlers returns the handlers.
func NewAuditHandlers(a AuditService) AuditHandlers { return AuditHandlers{audit: a} }

// SearchAudit implements StrictServerInterface.
func (h AuditHandlers) SearchAudit(ctx context.Context, req SearchAuditRequestObject) (SearchAuditResponseObject, error) {
	p := req.Params
	f := app.AuditFilter{}

	if p.Actor != nil {
		f.Actor = *p.Actor
	}

	if p.Action != nil {
		f.ActionPrefix = *p.Action
	}

	if p.TargetType != nil {
		f.TargetType = *p.TargetType
	}

	if p.TargetId != nil {
		f.TargetID = *p.TargetId
	}

	if p.From != nil {
		f.From = *p.From
	}

	if p.To != nil {
		f.To = *p.To
	}

	if p.Before != nil {
		f.BeforeID = *p.Before
	}

	if p.Limit != nil {
		f.Limit = *p.Limit
	}

	rows, next, err := h.audit.Search(ctx, f)
	if err != nil {
		return nil, err
	}

	out := AuditPage{Entries: make([]AuditEntry, 0, len(rows))}

	for _, row := range rows {
		e := row.Entry
		entry := AuditEntry{
			Id: row.ID, At: e.At().UTC(), ActorKind: AuditEntryActorKind(e.Actor().Kind()), Actor: row.ActorName,
			Action: e.Action(), TargetType: optString(e.TargetType()), TargetId: optString(e.TargetID()),
			Target: optString(row.TargetName), Result: AuditEntryResult(e.Result()), RequestId: optString(e.RequestID()),
		}

		if id := e.Actor().UserID(); !id.IsZero() {
			entry.ActorId = optString(id.String())
		}

		if e.Actor().IP().IsValid() {
			entry.ActorIp = optString(e.Actor().IP().String())
		}

		if b := e.Before(); len(b) > 0 {
			entry.Before = &b
		}

		if a := e.After(); len(a) > 0 {
			entry.After = &a
		}

		out.Entries = append(out.Entries, entry)
	}

	if next > 0 {
		out.NextBefore = &next
	}

	return jsonOK{out}, nil
}

func (j jsonOK) VisitSearchAuditResponse(w http.ResponseWriter) error { return j.write(w) }
