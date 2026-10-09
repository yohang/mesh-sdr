package app

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// AuditView reads the audit log for Admin › Audit log (ACC-010). The log is
// append-only: there is no write here.
type AuditView struct {
	log   domain.AuditReader
	users domain.UserRepository
}

// NewAuditView returns the service.
func NewAuditView(log domain.AuditReader, users domain.UserRepository) *AuditView {
	return &AuditView{log: log, users: users}
}

// AuditFilter filters the audit log.
type AuditFilter struct {
	// Actor is a username; an unknown name matches nothing.
	Actor        string
	ActionPrefix string
	TargetType   string
	TargetID     string
	From, To     time.Time
	BeforeID     int64
	// Offset skips the newest matching entries (the previous pages).
	Offset int
	Limit  int
}

// AuditRow is an audit entry with the names of its actor and target user.
type AuditRow struct {
	ID    int64
	Entry domain.AuditEntry
	// ActorName is the actor's username, "deleted user <id>" when the
	// account is gone, or the actor kind (cli, system…).
	ActorName string
	// TargetName is the username of a user target, when it exists.
	TargetName string
}

// AuditPageSize is the page size of the audit view.
const AuditPageSize = 100

// MaxAuditExport bounds an export.
const MaxAuditExport = 100_000

var errNoActor = errors.New("no such actor")

func (s *AuditView) query(ctx context.Context, f AuditFilter) (domain.AuditQuery, error) {
	q := domain.AuditQuery{
		ActionPrefix: f.ActionPrefix, TargetType: f.TargetType, TargetID: f.TargetID, From: f.From, To: f.To,
		BeforeID: f.BeforeID, Offset: f.Offset, Limit: f.Limit,
	}

	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = AuditPageSize
	}

	if f.Actor == "" {
		return q, nil
	}

	name, err := domain.NewUsername(f.Actor)
	if err != nil {
		return q, errNoActor
	}

	u, err := s.users.ByUsername(ctx, name)
	if errors.Is(err, domain.ErrUserNotFound) {
		return q, errNoActor
	}

	if err != nil {
		return q, wrap("load actor", err)
	}

	q.ActorUserID = u.ID()

	return q, nil
}

// Search returns a page of entries, newest first, and the BeforeID of the
// next page (0 when it is the last).
func (s *AuditView) Search(ctx context.Context, f AuditFilter) ([]AuditRow, int64, error) {
	q, err := s.query(ctx, f)
	if errors.Is(err, errNoActor) {
		return nil, 0, nil
	}

	if err != nil {
		return nil, 0, err
	}

	recs, err := s.log.Search(ctx, q)
	if err != nil {
		return nil, 0, wrap("search audit log", err)
	}

	rows, err := s.rows(ctx, recs)
	if err != nil {
		return nil, 0, err
	}

	var next int64
	if len(recs) == q.Limit {
		next = recs[len(recs)-1].ID
	}

	return rows, next, nil
}

// Each calls fn for every entry matching f, newest first, up to
// MaxAuditExport entries (exports).
func (s *AuditView) Each(ctx context.Context, f AuditFilter, fn func(AuditRow) error) error {
	f.Limit = 1000
	n := 0

	for {
		rows, next, err := s.Search(ctx, f)
		if err != nil {
			return err
		}

		for _, r := range rows {
			if n >= MaxAuditExport {
				return nil
			}

			if err := fn(r); err != nil {
				return err
			}

			n++
		}

		if next == 0 {
			return nil
		}

		f.BeforeID = next
	}
}

func (s *AuditView) rows(ctx context.Context, recs []domain.AuditRecord) ([]AuditRow, error) {
	var ids []domain.UserID

	for _, r := range recs {
		if id := r.Entry.Actor().UserID(); !id.IsZero() && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}

		if r.Entry.TargetType() == "user" {
			if id, err := domain.ParseUserID(r.Entry.TargetID()); err == nil && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}

	names, err := s.users.Usernames(ctx, ids)
	if err != nil {
		return nil, wrap("load usernames", err)
	}

	out := make([]AuditRow, 0, len(recs))

	for _, r := range recs {
		row := AuditRow{ID: r.ID, Entry: r.Entry, ActorName: string(r.Entry.Actor().Kind())}

		if id := r.Entry.Actor().UserID(); !id.IsZero() {
			if n, ok := names[id]; ok {
				row.ActorName = n.String()
			} else {
				row.ActorName = "deleted user " + id.String()[:8]
			}
		}

		if r.Entry.TargetType() == "user" {
			if id, err := domain.ParseUserID(r.Entry.TargetID()); err == nil {
				if n, ok := names[id]; ok {
					row.TargetName = n.String()
				}
			}
		}

		out = append(out, row)
	}

	return out, nil
}
