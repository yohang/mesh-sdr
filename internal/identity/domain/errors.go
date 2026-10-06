// Package domain is the identity model (TECHNICAL_SPEC §5, §7.1): users with
// their login identities and role grants, sessions, the principal of a
// request, login throttling and audit entries. It imports only the standard
// library and the shared kernel.
package domain

import (
	"fmt"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Domain errors. Codes are part of the public API.
var (
	ErrInvalidID          = shared.NewError(shared.KindInvalid, "invalid_id", "invalid identifier")
	ErrInvalidUsername    = shared.NewError(shared.KindInvalid, "invalid_username", "a username has 2 to 64 characters among A-Z, a-z, 0-9, '_', '.' and '-'")
	ErrInvalidEmail       = shared.NewError(shared.KindInvalid, "invalid_email", "invalid e-mail address")
	ErrInvalidDisplayName = shared.NewError(shared.KindInvalid, "invalid_display_name", "a display name has 1 to 64 printable characters")
	ErrInvalidPassword    = shared.NewError(shared.KindInvalid, "invalid_password", "the password does not follow the password policy")
	ErrInvalidLogin       = shared.NewError(shared.KindInvalid, "invalid_login", "invalid username or e-mail")
	ErrInvalidRole        = shared.NewError(shared.KindInvalid, "invalid_role", "unknown role")
	ErrInvalidDevice      = shared.NewError(shared.KindInvalid, "invalid_device_id", "invalid device id")
	ErrInvalidIdentity    = shared.NewError(shared.KindInvalid, "invalid_identity", "invalid login identity")
	ErrInvalidHash        = shared.NewError(shared.KindInvalid, "invalid_password_hash", "invalid password hash")
	ErrInvalidToken       = shared.NewError(shared.KindInvalid, "invalid_token", "invalid token")
	ErrInvalidSession     = shared.NewError(shared.KindInvalid, "invalid_session", "invalid session")
	ErrInvalidUser        = shared.NewError(shared.KindInvalid, "invalid_user", "invalid user")
	ErrInvalidInvitation  = shared.NewError(shared.KindInvalid, "invalid_invitation", "invalid invitation")
	ErrInvalidAudit       = shared.NewError(shared.KindInvalid, "invalid_audit_entry", "invalid audit entry")

	// ErrInvalidCredentials is the one generic login error (SR-06): unknown
	// identifier, wrong password, disabled account or no local identity.
	ErrInvalidCredentials = shared.NewError(shared.KindUnauthenticated, "invalid_credentials", "incorrect username, e-mail or password")
	// ErrInvalidCurrentPassword means the current password given to change
	// it is wrong.
	ErrInvalidCurrentPassword = shared.NewError(shared.KindInvalid, "invalid_current_password", "the current password is incorrect")
	// ErrUnauthenticated means the request needs a valid session.
	ErrUnauthenticated = shared.NewError(shared.KindUnauthenticated, "unauthenticated", "sign in required")
	// ErrForbidden means the principal lacks the required role.
	ErrForbidden = shared.NewError(shared.KindForbidden, "forbidden", "not allowed")
	// ErrAdminNetworkDenied means an admin request came from a client address
	// outside admin.allowed_networks (AUTH-016).
	ErrAdminNetworkDenied = shared.NewError(shared.KindForbidden, "admin_network_denied", "admin access is not allowed from this network")
	// ErrPasswordChangeRequired means the user must set a new password
	// before doing anything else (AUTH-006).
	ErrPasswordChangeRequired = shared.NewError(shared.KindForbidden, "password_change_required", "set a new password first")
	// ErrCSRF means a state-changing request failed the CSRF check (AUTH-019).
	ErrCSRF = shared.NewError(shared.KindForbidden, "csrf_failed", "missing or invalid CSRF token")
	// ErrRateLimited means too many attempts; see RateLimitError.
	ErrRateLimited = shared.NewError(shared.KindRateLimited, "rate_limited", "too many attempts")

	// ErrSetupTokenInvalid means the first-admin setup link is unknown,
	// expired, used, or useless because an admin exists (AUTH-018).
	ErrSetupTokenInvalid = shared.NewError(shared.KindNotFound, "setup_token_invalid", "the setup link is not valid")

	// ErrInvitationInvalid means an invitation link is unknown, expired,
	// revoked or used (one answer for all, ACC-002).
	ErrInvitationInvalid = shared.NewError(shared.KindNotFound, "invitation_invalid", "this invitation is no longer valid")
	// ErrInvitationNotFound means no invitation has this id.
	ErrInvitationNotFound = shared.NewError(shared.KindNotFound, "invitation_not_found", "invitation not found")
	// ErrInvitationNotPending means the invitation can no longer be revoked.
	ErrInvitationNotPending = shared.NewError(shared.KindConflict, "invitation_not_pending", "the invitation was already used, revoked or has expired")
	// ErrLastAdmin means the change would leave no enabled admin (ACC-006).
	ErrLastAdmin = shared.NewError(shared.KindConflict, "last_admin", "the last enabled admin cannot lose the admin role, be disabled or be deleted")

	// ErrNoListenableDevice means no device of the node is open to the
	// caller: no access token (§5.9).
	ErrNoListenableDevice = shared.NewError(shared.KindForbidden, "no_listenable_device", "no device of this node is open to you")
	// ErrAnonymousTokens means anonymous access tokens need a connection
	// bound by the gateway (GRID-011), not available yet.
	ErrAnonymousTokens = shared.NewError(shared.KindUnauthenticated, "unauthenticated", "sign in to listen")
	// ErrInvalidConnection means the connection id is malformed or not the
	// caller's.
	ErrInvalidConnection = shared.NewError(shared.KindForbidden, "invalid_connection", "this connection is not yours")

	ErrUserNotFound    = shared.NewError(shared.KindNotFound, "user_not_found", "user not found")
	ErrSessionNotFound = shared.NewError(shared.KindNotFound, "session_not_found", "session not found")
	ErrUsernameTaken   = shared.NewError(shared.KindConflict, "username_taken", "the username is already used")
	ErrEmailTaken      = shared.NewError(shared.KindConflict, "email_taken", "the e-mail address is already used")
	// ErrEmailUnusable is the one answer for an address that cannot be
	// used, whether it is malformed or another account's: account flows
	// never tell that an address has an account (SR-06).
	ErrEmailUnusable = ErrInvalidEmail.WithDetail("this e-mail address cannot be used")

	ErrVersionConflict = shared.NewError(shared.KindConflict, "version_conflict", "the record was changed concurrently")
)

// RateLimitError is ErrRateLimited with the time to wait before retrying.
type RateLimitError struct {
	retryAfter time.Duration
}

// NewRateLimitError returns a rate-limit error. The wait is rounded up to a
// whole second, at least one.
func NewRateLimitError(retryAfter time.Duration) *RateLimitError {
	s := (retryAfter + time.Second - 1) / time.Second
	if s < 1 {
		s = 1
	}

	return &RateLimitError{retryAfter: s * time.Second}
}

// RetryAfter returns the wait before the next attempt.
func (e *RateLimitError) RetryAfter() time.Duration { return e.retryAfter }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%v (retry after %s)", ErrRateLimited, e.retryAfter)
}

// Unwrap returns a domain error matching ErrRateLimited.
func (e *RateLimitError) Unwrap() error {
	return ErrRateLimited.WithDetail(fmt.Sprintf("too many attempts, try again in %d s", int(e.retryAfter/time.Second)))
}
