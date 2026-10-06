package wire

import (
	"context"
	"errors"
	"sync"
	"time"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/infra/keyring"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// errKeyringNotReady is returned before identity attached its keyring.
var errKeyringNotReady = errors.New("access-token keyring not ready")

// hubKeys gives the grid the identity keyring (ACC-007): the keys pushed to
// the nodes with ctl.keys.update (grid app.KeySource) and the signer of the
// tokens minted by the gateway authz (app.TokenIssuer). Identity is wired
// after the grid, so the keyring is attached afterwards.
type hubKeys struct {
	now     func() time.Time
	changed chan struct{}

	mu sync.Mutex
	k  *keyring.Keyring
}

var (
	_ gridapp.KeySource   = (*hubKeys)(nil)
	_ gridapp.TokenIssuer = (*hubKeys)(nil)
)

func newHubKeys(now func() time.Time) *hubKeys {
	return &hubKeys{now: now, changed: make(chan struct{}, 1)}
}

// attach plugs the keyring in; key changes are signalled to the grid.
func (h *hubKeys) attach(k *keyring.Keyring) {
	h.mu.Lock()
	h.k = k
	h.mu.Unlock()

	k.OnChange(func() {
		select {
		case h.changed <- struct{}{}:
		default:
		}
	})
}

func (h *hubKeys) keyring() (*keyring.Keyring, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.k == nil {
		return nil, errKeyringNotReady
	}

	return h.k, nil
}

// VerificationKeys implements gridapp.KeySource.
func (h *hubKeys) VerificationKeys(context.Context) (gridapp.VerificationKeys, error) {
	k, err := h.keyring()
	if err != nil {
		return gridapp.VerificationKeys{}, err
	}

	jwks, revoked := k.Published(h.now())

	return gridapp.VerificationKeys{Keys: jwks.Keys, RevokedKids: revoked}, nil
}

// Changed implements gridapp.KeySource.
func (h *hubKeys) Changed() <-chan struct{} { return h.changed }

// Issue implements gridapp.TokenIssuer with the current signing key.
func (h *hubKeys) Issue(_ context.Context, c token.Claims) (string, error) {
	k, err := h.keyring()
	if err != nil {
		return "", err
	}

	priv, _, err := k.Signer(h.now())
	if err != nil {
		return "", err
	}

	return token.Sign(c, priv)
}

// nodeRevocations sends identity revocations (sessions by token.SessionRef,
// users by id) to the nodes, dated with the hub clock.
type nodeRevocations struct {
	b   gridapp.RevocationBroadcaster
	now func() time.Time
}

// PublishRevocation implements identityapp.RevocationPublisher.
func (n nodeRevocations) PublishRevocation(ctx context.Context, r identityapp.Revocation) {
	users := make([]string, 0, len(r.Users))
	for _, u := range r.Users {
		users = append(users, u.String())
	}

	n.b.BroadcastRevocations(ctx, n.now(), r.Sessions, users)
}

// connectionBinder lets POST /api/v1/auth/token refresh only connections
// the gateway authz opened for the same caller: same node, still open, and
// the same user and session (anonymous rows for anonymous callers only).
type connectionBinder struct {
	repo griddomain.ConnectionRepository
}

// Bound implements identityapp.ConnectionBinder.
func (b connectionBinder) Bound(ctx context.Context, cid, nodeID string, by identityapp.Actor) (bool, error) {
	id, err := shared.ParseUUID(cid)
	if err != nil {
		return false, nil //nolint:nilerr // not a connection id issued by the hub
	}

	c, err := b.repo.Get(ctx, id)
	if errors.Is(err, griddomain.ErrConnectionNotFound) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	info := c.Info()
	if info.NodeID != nodeID || info.Kind != griddomain.ConnectionMedia {
		return false, nil
	}

	if _, _, closed := c.Closed(); closed {
		return false, nil
	}

	p := by.Principal
	if p.IsAnonymous() {
		return info.UserID.IsZero(), nil
	}

	return info.UserID.String() == p.UserID().String() && info.SessionID.String() == p.SessionID().String(), nil
}
