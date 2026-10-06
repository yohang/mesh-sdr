package layout

// AdminSection is a section of the admin area (FEATURE_SPEC §10.13).
type AdminSection struct {
	ID    string
	Label string
	Href  string
}

// Admin sections, in navigation order. Modules add theirs here when they
// build them (users, nodes, presets…).
var AdminSections = []AdminSection{
	{ID: "overview", Label: "Overview", Href: "/admin"},
	{ID: "site", Label: "Site", Href: "/admin/site"},
	{ID: "access", Label: "Access", Href: "/admin/access"},
	{ID: "look-and-feel", Label: "Look & feel", Href: "/admin/look-and-feel"},
	{ID: "devices", Label: "Devices", Href: "/admin/devices"},
	{ID: "retention", Label: "Data & retention", Href: "/admin/retention"},
	{ID: "system", Label: "System", Href: "/admin/system"},
}

// OperatorAdminSections are the admin sections an operator may open
// (FEATURE_SPEC §7.4: device views are readable by operators).
var OperatorAdminSections = []AdminSection{{ID: "devices", Label: "Devices", Href: "/admin/devices"}}
