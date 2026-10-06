package templates

// Theme is the admin-chosen theme mode (UI-001 / UI-008). There is no user override.
type Theme string

// Theme modes. Auto follows the OS prefers-color-scheme.
const (
	ThemeLight Theme = "light"
	ThemeDark  Theme = "dark"
	ThemeAuto  Theme = "auto"
)

// ColorScheme returns the value of the color-scheme meta tag for the theme, so that
// native controls and scrollbars match before the stylesheet is applied.
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

// Section identifies a top-level app shell section (UI-006).
type Section string

// App shell sections, in navigation order.
const (
	SectionReceiver Section = "receiver"
	SectionMap      Section = "map"
	SectionDecodes  Section = "decodes"
	SectionFiles    Section = "files"
	SectionAdmin    Section = "admin"
)

// NavItem is an entry of the main navigation.
type NavItem struct {
	Section Section
	Label   string
	Href    string
}

// Shell carries the per-request data the app shell layout needs.
type Shell struct {
	// Title is the page title, without the site name.
	Title string
	// Active is the section the page belongs to.
	Active Section
	// Theme is the admin-chosen theme mode.
	Theme Theme
	// CSRFToken is the session-bound synchronizer token sent back in the X-CSRF-Token header.
	CSRFToken string
	// Nav lists the sections visible to the current user.
	Nav []NavItem
}

// DocumentTitle returns the full <title> value.
func (s Shell) DocumentTitle() string {
	if s.Title == "" {
		return siteName
	}

	return s.Title + " · " + siteName
}

// SPIKE: the site name becomes an admin setting (receiver identity) later.
const siteName = "MeshSDR"
