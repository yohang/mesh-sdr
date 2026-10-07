package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// InvitationService runs invitations for the API.
type InvitationService interface {
	Now() time.Time
	Create(ctx context.Context, by app.Actor, in app.CreateInvitationInput) (app.CreatedInvitation, error)
	List(ctx context.Context) ([]*domain.Invitation, error)
	Revoke(ctx context.Context, by app.Actor, id domain.InvitationID) error
	Check(ctx context.Context, token string, meta app.RequestMeta) (*domain.Invitation, error)
	Accept(ctx context.Context, in app.AcceptInput) (app.LoginResult, error)
	TestMail(ctx context.Context, by app.Actor) (domain.Email, error)
}

// SessionOpener turns an opened session into its response parts.
type SessionOpener interface {
	OpenedSession(res app.LoginResult) (domain.Principal, string, []*http.Cookie)
}

// InvitationHandlers serve /invitations and /auth/invitations.
type InvitationHandlers struct {
	sessions    Sessions
	opener      SessionOpener
	invitations InvitationService
}

// NewInvitationHandlers returns the handlers.
func NewInvitationHandlers(s Sessions, o SessionOpener, i InvitationService) InvitationHandlers {
	return InvitationHandlers{sessions: s, opener: o, invitations: i}
}

func invitation(i *domain.Invitation, now time.Time) Invitation {
	return Invitation{
		Id: i.ID().String(), Role: InvitationRole(i.Role().String()), DeviceId: optString(i.Device().String()),
		Email: optString(i.Email().String()), Delivery: InvitationDelivery(i.Delivery()), State: InvitationState(i.StateAt(now)),
		CreatedAt: i.CreatedAt(), ExpiresAt: i.ExpiresAt(), RedeemedAt: optTime(i.RedeemedAt()), RevokedAt: optTime(i.RevokedAt()),
	}
}

// ListInvitations implements StrictServerInterface.
func (h InvitationHandlers) ListInvitations(ctx context.Context, _ ListInvitationsRequestObject) (ListInvitationsResponseObject, error) {
	list, err := h.invitations.List(ctx)
	if err != nil {
		return nil, err
	}

	now := h.invitations.Now()
	out := InvitationList{Invitations: make([]Invitation, 0, len(list))}

	for _, i := range list {
		out.Invitations = append(out.Invitations, invitation(i, now))
	}

	return jsonOK{out}, nil
}

// CreateInvitation implements StrictServerInterface.
func (h InvitationHandlers) CreateInvitation(ctx context.Context, req CreateInvitationRequestObject) (CreateInvitationResponseObject, error) {
	role, err := domain.ParseRole(string(req.Body.Role))
	if err != nil {
		return nil, err
	}

	in := app.CreateInvitationInput{Role: role}
	if req.Body.DeviceId != nil {
		in.Device = *req.Body.DeviceId
	}

	if req.Body.Email != nil {
		in.Email = *req.Body.Email
	}

	if req.Body.ExpiresInHours != nil {
		in.TTL = time.Duration(*req.Body.ExpiresInHours) * time.Hour
	}

	created, err := h.invitations.Create(ctx, h.sessions.Actor(ctx), in)
	if err != nil {
		return nil, err
	}

	return jsonStatus{status: http.StatusCreated, v: InvitationCreated{
		Invitation: invitation(created.Invitation, h.invitations.Now()), Link: created.Link, Mailed: created.Mailed,
	}}, nil
}

// RevokeInvitation implements StrictServerInterface.
func (h InvitationHandlers) RevokeInvitation(ctx context.Context, req RevokeInvitationRequestObject) (RevokeInvitationResponseObject, error) {
	id, err := domain.ParseInvitationID(req.Id)
	if err != nil {
		return nil, domain.ErrInvitationNotFound
	}

	if err := h.invitations.Revoke(ctx, h.sessions.Actor(ctx), id); err != nil {
		return nil, err
	}

	return noContent{}, nil
}

// CheckInvitation implements StrictServerInterface.
func (h InvitationHandlers) CheckInvitation(ctx context.Context, req CheckInvitationRequestObject) (CheckInvitationResponseObject, error) {
	inv, err := h.invitations.Check(ctx, req.Token, h.sessions.Actor(ctx).Meta)

	var rl *domain.RateLimitError
	if errors.As(err, &rl) {
		return rateLimited{err: rl}, nil
	}

	if err != nil {
		return nil, err
	}

	return jsonOK{InvitationCheck{
		Role: InvitationCheckRole(inv.Role().String()), DeviceId: optString(inv.Device().String()),
		Email: optString(inv.Email().String()), ExpiresAt: inv.ExpiresAt(),
	}}, nil
}

// AcceptInvitation implements StrictServerInterface.
func (h InvitationHandlers) AcceptInvitation(ctx context.Context, req AcceptInvitationRequestObject) (AcceptInvitationResponseObject, error) {
	in := app.AcceptInput{Token: req.Token, Username: req.Body.Username, Password: req.Body.Password, Meta: h.sessions.Actor(ctx).Meta}
	if req.Body.DisplayName != nil {
		in.DisplayName = *req.Body.DisplayName
	}

	if req.Body.Email != nil {
		in.Email = *req.Body.Email
	}

	res, err := h.invitations.Accept(ctx, in)

	var rl *domain.RateLimitError
	if errors.As(err, &rl) {
		return rateLimited{err: rl}, nil
	}

	if err != nil {
		return nil, err
	}

	p, token, cookies := h.opener.OpenedSession(res)

	return sessionCreated{sessionResponse{info: sessionInfo(p, token), cookies: cookies}}, nil
}

// jsonStatus writes a JSON body with a status.
type jsonStatus struct {
	status int
	v      any
}

func (j jsonStatus) VisitCreateInvitationResponse(w http.ResponseWriter) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(j.status)

	return json.NewEncoder(w).Encode(j.v)
}

// sessionCreated is a new session (201).
type sessionCreated struct{ s sessionResponse }

func (s sessionCreated) VisitAcceptInvitationResponse(w http.ResponseWriter) error {
	for _, c := range s.s.cookies {
		http.SetCookie(w, c)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	return json.NewEncoder(w).Encode(s.s.info)
}

func (j jsonOK) VisitListInvitationsResponse(w http.ResponseWriter) error { return j.write(w) }
func (j jsonOK) VisitCheckInvitationResponse(w http.ResponseWriter) error { return j.write(w) }

func (n noContent) VisitRevokeInvitationResponse(w http.ResponseWriter) error { return n.write(w) }

// SendTestMail implements StrictServerInterface.
func (h InvitationHandlers) SendTestMail(ctx context.Context, _ SendTestMailRequestObject) (SendTestMailResponseObject, error) {
	to, err := h.invitations.TestMail(ctx, h.sessions.Actor(ctx))
	if err != nil {
		return nil, err
	}

	return jsonOK{MailTest{To: to.String()}}, nil
}

func (j jsonOK) VisitSendTestMailResponse(w http.ResponseWriter) error { return j.write(w) }

func (r rateLimited) VisitCheckInvitationResponse(w http.ResponseWriter) error  { return r.write(w) }
func (r rateLimited) VisitAcceptInvitationResponse(w http.ResponseWriter) error { return r.write(w) }
