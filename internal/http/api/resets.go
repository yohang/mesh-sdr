package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// ResetService runs password resets for the API.
type ResetService interface {
	Request(ctx context.Context, login string, meta app.RequestMeta) error
	Confirm(ctx context.Context, token, password string, meta app.RequestMeta) error
	IssueByAdmin(ctx context.Context, by app.Actor, id domain.UserID) (app.AdminResult, error)
}

// ResetHandlers serve the password reset endpoints.
type ResetHandlers struct {
	sessions Sessions
	resets   ResetService
}

// NewResetHandlers returns the handlers.
func NewResetHandlers(s Sessions, r ResetService) ResetHandlers {
	return ResetHandlers{sessions: s, resets: r}
}

// RequestPasswordReset implements StrictServerInterface.
func (h ResetHandlers) RequestPasswordReset(ctx context.Context, req RequestPasswordResetRequestObject) (RequestPasswordResetResponseObject, error) {
	err := h.resets.Request(ctx, req.Body.Login, h.sessions.Actor(ctx).Meta)

	var rl *domain.RateLimitError
	if errors.As(err, &rl) {
		return rateLimited{err: rl}, nil
	}

	if err != nil {
		return nil, err
	}

	return accepted{}, nil
}

// ConfirmPasswordReset implements StrictServerInterface.
func (h ResetHandlers) ConfirmPasswordReset(ctx context.Context, req ConfirmPasswordResetRequestObject) (ConfirmPasswordResetResponseObject, error) {
	err := h.resets.Confirm(ctx, req.Body.Token, req.Body.NewPassword, h.sessions.Actor(ctx).Meta)

	var rl *domain.RateLimitError
	if errors.As(err, &rl) {
		return rateLimited{err: rl}, nil
	}

	if err != nil {
		return nil, err
	}

	return noContent{}, nil
}

// IssuePasswordReset implements StrictServerInterface.
func (h ResetHandlers) IssuePasswordReset(ctx context.Context, req IssuePasswordResetRequestObject) (IssuePasswordResetResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	res, err := h.resets.IssueByAdmin(ctx, h.sessions.Actor(ctx), id)
	if err != nil {
		return nil, err
	}

	return jsonOK{AdminReset{Mailed: res.Mailed, Link: optString(res.Link)}}, nil
}

// accepted is an empty 202.
type accepted struct{}

func (accepted) VisitRequestPasswordResetResponse(w http.ResponseWriter) error {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)

	return nil
}

func (n noContent) VisitConfirmPasswordResetResponse(w http.ResponseWriter) error { return n.write(w) }

func (j jsonOK) VisitIssuePasswordResetResponse(w http.ResponseWriter) error { return j.write(w) }

func (r rateLimited) VisitRequestPasswordResetResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r rateLimited) VisitConfirmPasswordResetResponse(w http.ResponseWriter) error {
	return r.write(w)
}
