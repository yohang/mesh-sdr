package domain

import (
	"maps"
	"net/netip"
	"regexp"
	"time"
)

// ActorKind is who performed an audited action (`audit_log.actor_kind`).
type ActorKind string

// Actor kinds.
const (
	ActorUser      ActorKind = "user"
	ActorAnonymous ActorKind = "anonymous"
	ActorSystem    ActorKind = "system"
	ActorNode      ActorKind = "node"
	ActorCLI       ActorKind = "cli"
)

// Actor is the author of an audited action.
type Actor struct {
	kind   ActorKind
	userID UserID
	ip     netip.Addr
}

// UserActor returns a signed-in user acting from ip (optional).
func UserActor(id UserID, ip netip.Addr) Actor {
	return Actor{kind: ActorUser, userID: id, ip: canonicalIP(ip)}
}

// AnonymousActor returns an anonymous client at ip (optional).
func AnonymousActor(ip netip.Addr) Actor { return Actor{kind: ActorAnonymous, ip: canonicalIP(ip)} }

// CLIActor returns the operator using the command line on the hub host.
func CLIActor() Actor { return Actor{kind: ActorCLI} }

// NodeActor is a node, identified by its address in audit entries.
func NodeActor(ip netip.Addr) Actor { return Actor{kind: ActorNode, ip: canonicalIP(ip)} }

// SystemActor returns the hub itself (jobs).
func SystemActor() Actor { return Actor{kind: ActorSystem} }

// Kind returns the actor kind.
func (a Actor) Kind() ActorKind { return a.kind }

// UserID returns the acting user (zero unless a user acts).
func (a Actor) UserID() UserID { return a.userID }

// IP returns the client address (invalid when unknown).
func (a Actor) IP() netip.Addr { return a.ip }

// AuditResult is the outcome of an audited action.
type AuditResult string

// Results.
const (
	ResultOK     AuditResult = "ok"
	ResultDenied AuditResult = "denied"
	ResultError  AuditResult = "error"
)

// Audited actions of the identity module (dotted verbs, TECHNICAL_SPEC §7.1).
const (
	ActionLoginSuccess      = "auth.login.success"
	ActionLoginFailure      = "auth.login.failure"
	ActionLoginLockout      = "auth.login.lockout"
	ActionLogout            = "auth.logout"
	ActionSessionsRevoke    = "auth.session.revoke"
	ActionPasswordChange    = "auth.password.change"
	ActionUserCreate        = "user.create"
	ActionUserPasswordReset = "user.password.reset"
	ActionUserDisable       = "user.disable"
	ActionUserEnable        = "user.enable"
	ActionUserRoleUpdate    = "user.role.update"
	ActionUserUpdate        = "user.update"
	ActionEmailChange       = "user.email.change"
	ActionInvitationCreate  = "invitation.create"
	ActionInvitationRevoke  = "invitation.revoke"
	ActionInvitationRedeem  = "invitation.redeem"
	ActionResetRequest      = "auth.password_reset.request"
	ActionResetComplete     = "auth.password_reset.complete"
)

var actionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// AuditEntry is one append-only `audit_log` row. Values in Before and After
// must never hold secrets.
type AuditEntry struct {
	at         time.Time
	actor      Actor
	action     string
	targetType string
	targetID   string
	result     AuditResult
	before     map[string]string
	after      map[string]string
	requestID  string
}

// NewAuditEntry builds an entry.
func NewAuditEntry(at time.Time, actor Actor, action string, result AuditResult) (AuditEntry, error) {
	switch {
	case at.IsZero(), actor.kind == "", !actionPattern.MatchString(action), len(action) > 64:
		return AuditEntry{}, ErrInvalidAudit
	case result != ResultOK && result != ResultDenied && result != ResultError:
		return AuditEntry{}, ErrInvalidAudit
	}

	return AuditEntry{at: at.UTC().Truncate(time.Millisecond), actor: actor, action: action, result: result}, nil
}

// WithTarget returns a copy with the target of the action.
func (e AuditEntry) WithTarget(kind, id string) AuditEntry {
	e.targetType, e.targetID = truncateUTF8(kind, 32), truncateUTF8(id, 128)

	return e
}

// WithRequestID returns a copy correlated with a request.
func (e AuditEntry) WithRequestID(id string) AuditEntry {
	e.requestID = truncateUTF8(id, 64)

	return e
}

// WithBefore returns a copy with the state before the change.
func (e AuditEntry) WithBefore(v map[string]string) AuditEntry {
	e.before = maps.Clone(v)

	return e
}

// WithAfter returns a copy with the state after the change (or details).
func (e AuditEntry) WithAfter(v map[string]string) AuditEntry {
	e.after = maps.Clone(v)

	return e
}

// At returns the time of the action.
func (e AuditEntry) At() time.Time { return e.at }

// Actor returns the author.
func (e AuditEntry) Actor() Actor { return e.actor }

// Action returns the dotted verb.
func (e AuditEntry) Action() string { return e.action }

// TargetType returns the kind of target (empty when none).
func (e AuditEntry) TargetType() string { return e.targetType }

// TargetID returns the target id (empty when none).
func (e AuditEntry) TargetID() string { return e.targetID }

// Result returns the outcome.
func (e AuditEntry) Result() AuditResult { return e.result }

// Before returns the state before the change (nil when none).
func (e AuditEntry) Before() map[string]string { return maps.Clone(e.before) }

// After returns the state after the change (nil when none).
func (e AuditEntry) After() map[string]string { return maps.Clone(e.after) }

// RequestID returns the correlated request id.
func (e AuditEntry) RequestID() string { return e.requestID }
