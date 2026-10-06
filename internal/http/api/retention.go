package api

import (
	"context"
	"strconv"
	"time"

	jobsapp "github.com/yohang/mesh-sdr/internal/jobs/app"
	jobsdomain "github.com/yohang/mesh-sdr/internal/jobs/domain"
)

// RetentionService is the retention view (ADM-011).
type RetentionService interface {
	List(ctx context.Context) ([]jobsapp.StoreView, error)
	Purge(ctx context.Context, actor jobsapp.Actor, store string) (int64, error)
}

// RetentionHandlers serve /retention.
type RetentionHandlers struct {
	retention RetentionService
	actor     ActorFunc
}

// NewRetentionHandlers returns the handlers.
func NewRetentionHandlers(r RetentionService, actor ActorFunc) RetentionHandlers {
	return RetentionHandlers{retention: r, actor: actor}
}

// GetRetention implements StrictServerInterface.
func (h RetentionHandlers) GetRetention(ctx context.Context, _ GetRetentionRequestObject) (GetRetentionResponseObject, error) {
	views, err := h.retention.List(ctx)
	if err != nil {
		return nil, err
	}

	out := GetRetention200JSONResponse{Stores: make([]RetentionStore, 0, len(views))}

	for _, v := range views {
		s := RetentionStore{
			Store: v.Store.Name, Label: v.Store.Label, SettingKey: v.Store.SettingKey,
			Retention: FormatDuration(v.Retention), Rows: v.Rows, Job: JobRunOf(v.LastRun),
		}

		if v.Sized {
			b := v.Bytes
			s.Bytes = &b
		}

		out.Stores = append(out.Stores, s)
	}

	return out, nil
}

// PurgeStore implements StrictServerInterface.
func (h RetentionHandlers) PurgeStore(ctx context.Context, req PurgeStoreRequestObject) (PurgeStoreResponseObject, error) {
	a := h.actor(ctx)

	n, err := h.retention.Purge(ctx, jobsapp.Actor{User: a.User, IP: a.IP, RequestID: a.RequestID}, req.Store)
	if err != nil {
		return nil, err
	}

	return PurgeStore200JSONResponse{Store: req.Store, RowsDeleted: n}, nil
}

// JobRunOf converts the bookkeeping of a job.
func JobRunOf(r *jobsdomain.Run) JobRun {
	j := JobRun{Name: r.Name().String(), Running: r.Running()}

	if t := r.LastStarted(); !t.IsZero() {
		j.LastStartedAt = &t
	}

	if t := r.LastFinished(); !t.IsZero() {
		j.LastFinishedAt = &t
		rows := r.Rows()
		j.RowsAffected = &rows
	}

	if st := r.Status(); st != jobsdomain.StatusNone {
		s := JobRunLastStatus(st)
		j.LastStatus = &s
	}

	if e := r.LastError(); e != "" {
		j.LastError = &e
	}

	return j
}

// FormatDuration writes a duration with the units of the config ("30d",
// "1h30m").
func FormatDuration(d time.Duration) string {
	const day = 24 * time.Hour

	if d > 0 && d%day == 0 {
		return strconv.FormatInt(int64(d/day), 10) + "d"
	}

	return d.String()
}
