// Package layout holds the app shell templates shared by every module: the
// document (head, top bar, #main, footer) and the error page. Feature
// modules render their pages through internal/web/render, which wraps their
// content in the shell.
//
// Template rules (ADR 0003 §3, enforced by web.TestTemplateRules): no inline
// <script> except templ.JSONScript, no style="" nor <style>, no hx-on or js:
// expressions, no templ css/script components.
package layout

// Theme is the theme mode the shell renders (UI-001, UI-008).
type Theme string

// Theme modes. Auto follows the OS prefers-color-scheme.
const (
	ThemeLight Theme = "light"
	ThemeDark  Theme = "dark"
	ThemeAuto  Theme = "auto"
)

// Browser UI colors (theme-color, web app manifest): the surface token of
// each set, see static/css/input.css (kept in sync by a test).
const (
	ThemeColorLight = "#ffffff"
	ThemeColorDark  = "#161b22"
)

// ColorScheme returns the color-scheme meta value, so native controls and
// scrollbars match before the stylesheet is applied.
func (t Theme) ColorScheme() string {
	switch t {
	case ThemeLight:
		return "light"
	case ThemeDark:
		return "dark"
	default:
		return "light dark"
	}
}

// ThemeColor returns the browser UI color of a forced theme. In auto mode it
// returns the light color; the document also sends the dark one with a media
// query.
func (t Theme) ThemeColor() string {
	if t == ThemeDark {
		return ThemeColorDark
	}

	return ThemeColorLight
}

// Shell is the per-request data of the app shell, built by the shell module.
type Shell struct {
	// User is the signed-in user of the request; nil for an anonymous
	// visitor, who gets a discreet "Sign in" link (ACC-001).
	User *User
	// SiteName is shown in the top bar and the document title.
	SiteName string
	// Theme is the admin-chosen theme mode. A change applies on the next
	// full page load.
	Theme Theme
	// FooterLinks are the footer links, in order.
	FooterLinks []Link
	// Nav are the top bar sections the visitor may open, in order (UI-006
	// adds the full navigation; for now only Admin, for admins).
	Nav []Link
}

// Link is a navigation link. Section, for a top bar section, matches
// Page.Section to mark the current one.
type Link struct {
	Label   string
	Href    string
	Section string
}

// Admin section of the top bar.
const SectionAdmin = "admin"

// Page describes the page being rendered.
type Page struct {
	// Title is the page title, without the site name. Empty on the home page.
	Title string
	// Section is the top-level section the page belongs to (UI-006); the
	// shell marks its nav entry with aria-current. Empty outside sections.
	Section string
}

// DocumentTitle returns the <title> value.
func (s Shell) DocumentTitle(p Page) string {
	if p.Title == "" {
		return s.SiteName
	}

	return p.Title + " · " + s.SiteName
}

// User is the signed-in user shown in the top bar, with the links of the
// user menu (account, admin pages).
type User struct {
	Name  string
	Links []Link
}
