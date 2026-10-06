package domain

import (
	"strconv"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ErrInvalidSection is returned for an unknown top-level section.
var ErrInvalidSection = shared.NewError(shared.KindInvalid, "invalid_section",
	"section must be receiver, map, decodes, files or admin")

// Section is a top-level section of the app shell navigation (UI-006,
// FEATURE_SPEC §10.2): Receiver, Map, Decodes, Files and Admin, in that
// order. The zero value is no section.
type Section struct {
	id string
}

type sectionDef struct {
	label, path string
}

// Section ids.
const (
	sectionReceiver = "receiver"
	sectionMap      = "map"
	sectionDecodes  = "decodes"
	sectionFiles    = "files"
	sectionAdmin    = "admin"
)

var sectionDefs = map[string]sectionDef{
	sectionReceiver: {"Receiver", "/"},
	sectionMap:      {"Map", "/map"},
	sectionDecodes:  {"Decodes", "/decodes"},
	sectionFiles:    {"Files", "/files"},
	sectionAdmin:    {"Admin", "/admin"},
}

// The sections of the navigation.
var (
	SectionReceiver = Section{id: sectionReceiver}
	SectionMap      = Section{id: sectionMap}
	SectionDecodes  = Section{id: sectionDecodes}
	SectionFiles    = Section{id: sectionFiles}
	SectionAdmin    = Section{id: sectionAdmin}
)

// Sections returns every section in navigation order.
func Sections() []Section {
	return []Section{SectionReceiver, SectionMap, SectionDecodes, SectionFiles, SectionAdmin}
}

// NewSection returns the section with the given id.
func NewSection(id string) (Section, error) {
	if _, ok := sectionDefs[id]; !ok {
		return Section{}, ErrInvalidSection.WithDetail("unknown section " + strconv.Quote(id))
	}

	return Section{id: id}, nil
}

// ID returns the section id (receiver, map, decodes, files, admin), or ""
// for the zero value.
func (s Section) ID() string { return s.id }

// Label returns the navigation label.
func (s Section) Label() string { return sectionDefs[s.id].label }

// Path returns the path of the section's entry page.
func (s Section) Path() string { return sectionDefs[s.id].path }

// IsZero reports whether s is no section.
func (s Section) IsZero() bool { return s.id == "" }

// String returns the id.
func (s Section) String() string { return s.id }
