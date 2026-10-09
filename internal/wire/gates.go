package wire

import (
	"context"

	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
)

// roleGate opens a shell navigation section to the visitors holding a role
// (and, for admin, coming from admin.allowed_networks). The identity module
// is wired after the shell (it renders its pages with the shell renderer),
// so authz is set once identity exists; until then the gate stays closed.
type roleGate struct {
	role  identitydomain.Role
	authz interface {
		Authorize(ctx context.Context, role identitydomain.Role) error
	}
}

// Allows implements shell/app.Gate.
func (g *roleGate) Allows(ctx context.Context) bool {
	return g.authz != nil && g.authz.Authorize(ctx, g.role) == nil
}
