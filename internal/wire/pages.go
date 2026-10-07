package wire

import (
	"context"
	"net/http"

	"github.com/a-h/templ"

	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// pages adapts the shell renderer to the identity module's Pages: module
// pages are rendered in the app shell, full page unless htmx asks for a
// fragment (ADR 0007).
type pages struct{ rd *render.Renderer }

// Page implements identityhttp.Pages.
func (p pages) Page(w http.ResponseWriter, r *http.Request, status int, title string, content, fragment templ.Component) {
	p.rd.Page(w, r, status, layout.Page{Title: title}, content, fragment)
}

// AdminPage implements identityhttp.Pages.
func (p pages) AdminPage(w http.ResponseWriter, r *http.Request, status int, title, section string, content, fragment templ.Component) {
	p.rd.Page(w, r, status, layout.Page{Title: title, Section: layout.SectionAdmin}, layout.AdminPage(section, content), fragment)
}

// Error implements identityhttp.Pages.
func (p pages) Error(w http.ResponseWriter, r *http.Request, status int) {
	p.rd.Error(w, r, status)
}

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
