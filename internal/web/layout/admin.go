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
	{ID: "users", Label: "Users", Href: "/admin/users"},
	{ID: "invitations", Label: "Invitations", Href: "/admin/invitations"},
	{ID: "look-and-feel", Label: "Look & feel", Href: "/admin/look-and-feel"},
	{ID: "nodes", Label: "Nodes", Href: "/admin/nodes"},
	{ID: "node-health", Label: "Node health", Href: "/admin/grid"},
	{ID: "decoding", Label: "Decoding", Href: "/admin/decoding"},
	{ID: "map", Label: "Map", Href: "/admin/map"},
	{ID: "devices", Label: "Devices", Href: "/admin/devices"},
	{ID: "presets", Label: "Presets", Href: "/admin/presets"},
	{ID: "schedules", Label: "Schedules", Href: "/admin/schedules"},
	{ID: "bookmarks", Label: "Bookmarks", Href: "/bookmarks/manage"},
	{ID: "connections", Label: "Connections", Href: "/admin/connections"},
	{ID: "retention", Label: "Data & retention", Href: "/admin/retention"},
	{ID: "audit", Label: "Audit log", Href: "/admin/audit"},
	{ID: "system", Label: "System", Href: "/admin/system"},
}

// OperatorAdminSections are the admin sections an operator may open
// (FEATURE_SPEC §7.4 and GRID-009: device and node views are readable by
// operators; BMK-005: operators manage the hub bookmarks).
var OperatorAdminSections = []AdminSection{
	{ID: "nodes", Label: "Nodes", Href: "/admin/nodes"},
	{ID: "devices", Label: "Devices", Href: "/admin/devices"},
	{ID: "bookmarks", Label: "Bookmarks", Href: "/bookmarks/manage"},
}
