package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/yohang/mesh-sdr/internal/settings/app"
	"github.com/yohang/mesh-sdr/internal/settings/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// sectionSpec is a form section of an admin page: the keys it edits, saved
// together.
type sectionSpec struct {
	ID          string
	Title       string
	Description string
	Keys        []string
}

// field is the view of one setting in a form.
type field struct {
	Key      string
	ID       string
	Label    string
	Help     string
	Kind     domain.InputKind
	Options  []string
	Min, Max string
	MaxLen   int
	MinText  string
	Value    string // text form of the value
	Checked  bool   // InputBoolean
	Lat, Lon string // InputGeo
	Version  int64
	Locked   bool
	Origin   string
	Source   domain.Source
	Default  string
	Secret   bool
	Set      bool
	Error    string
}

// problem is an error of the form, shown in the summary.
type problem struct {
	FieldID string // empty when not tied to a field
	Text    string
}

// sectionView is a form section with its fields and outcome.
type sectionView struct {
	Spec     sectionSpec
	Action   string
	Fields   []field
	Problems []problem
	Notice   string // success or neutral notice
}

func fieldID(key string) string { return "f-" + strings.ReplaceAll(key, ".", "-") }

// textOf returns the form text of a JSON value of a kind.
func textOf(kind domain.InputKind, v domain.Value) string {
	if v.IsNull() {
		return ""
	}

	var s string
	if err := json.Unmarshal(v.JSON(), &s); err == nil {
		return s
	}

	if kind == domain.InputList {
		var list []string
		if err := json.Unmarshal(v.JSON(), &list); err == nil {
			return strings.Join(list, "\n")
		}
	}

	return v.String()
}

type geo struct {
	Lat *float64 `json:"lat"`
	Lon *float64 `json:"lon"`
}

func fmtFloat(f *float64) string {
	if f == nil {
		return ""
	}

	return strconv.FormatFloat(*f, 'f', -1, 64)
}

// newField builds the view of an effective setting.
func newField(e domain.Effective) field {
	d := e.Definition()
	in := d.Input()

	f := field{
		Key: e.Key().String(), ID: fieldID(e.Key().String()), Label: d.Label(), Help: d.Description(),
		Kind: in.Kind, Options: in.Options, MaxLen: in.MaxLength, MinText: in.MinText,
		Version: e.Version(), Locked: e.Locked(), Origin: e.Origin(), Source: e.Source(),
		Secret: d.Secret(), Set: e.IsSet(),
	}

	if in.Min != nil {
		f.Min = strconv.FormatFloat(*in.Min, 'f', -1, 64)
	}

	if in.Max != nil {
		f.Max = strconv.FormatFloat(*in.Max, 'f', -1, 64)
	}

	if d.Secret() {
		return f
	}

	f.Default = displayValue(in.Kind, d.Default())
	f.setValue(e.Value())

	return f
}

func (f *field) setValue(v domain.Value) {
	switch f.Kind {
	case domain.InputBoolean:
		f.Checked = v.String() == "true"
	case domain.InputGeo:
		var g geo
		if err := json.Unmarshal(v.JSON(), &g); err == nil {
			f.Lat, f.Lon = fmtFloat(g.Lat), fmtFloat(g.Lon)
		}
	default:
		f.Value = textOf(f.Kind, v)
	}
}

// displayValue is a value as shown to people (read-only fields, tables).
func displayValue(kind domain.InputKind, v domain.Value) string {
	switch {
	case v.IsNull():
		return "not set"
	case kind == domain.InputBoolean:
		if v.String() == "true" {
			return "yes"
		}

		return "no"
	case kind == domain.InputGeo:
		var g geo
		if err := json.Unmarshal(v.JSON(), &g); err == nil {
			return fmtFloat(g.Lat) + ", " + fmtFloat(g.Lon)
		}
	}

	t := textOf(kind, v)
	if t == "" {
		return "empty"
	}

	return t
}

// buildSection returns the view of a section from a snapshot.
func buildSection(spec sectionSpec, action string, snap *app.Snapshot) sectionView {
	v := sectionView{Spec: spec, Action: action}

	for _, k := range spec.Keys {
		if e, ok := snap.Get(k); ok {
			v.Fields = append(v.Fields, newField(e))
		}
	}

	return v
}

// errInvalidNumber is a form value that is not a number.
var errInvalidNumber = errors.New("enter a number")

// formValue turns the submitted form text of a field into a change: nil for
// a reset (empty field), or the new JSON value.
func formValue(f field, form url.Values) (*domain.Value, error) {
	text := func(name string) string { return strings.TrimSpace(form.Get(name)) }

	var raw any

	switch f.Kind {
	case domain.InputBoolean:
		vals := form[f.Key]
		raw = len(vals) > 0 && vals[len(vals)-1] == "true"
	case domain.InputGeo:
		lat, lon := text(f.Key+".lat"), text(f.Key+".lon")
		if lat == "" && lon == "" {
			return nil, nil //nolint:nilnil // reset
		}

		la, err1 := strconv.ParseFloat(lat, 64)
		lo, err2 := strconv.ParseFloat(lon, 64)

		if err1 != nil || err2 != nil {
			return nil, errors.New("enter a latitude and a longitude in decimal degrees")
		}

		raw = geo{Lat: &la, Lon: &lo}
	case domain.InputInteger, domain.InputNumber:
		s := text(f.Key)
		if s == "" {
			return nil, nil //nolint:nilnil // reset
		}

		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return nil, errInvalidNumber
		}

		v, err := domain.NewValue([]byte(s))
		if err != nil {
			return nil, errInvalidNumber
		}

		return &v, nil
	case domain.InputList:
		var items []string

		for line := range strings.SplitSeq(form.Get(f.Key), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				items = append(items, line)
			}
		}

		if len(items) == 0 {
			return nil, nil //nolint:nilnil // reset
		}

		raw = items
	case domain.InputTextarea, domain.InputMarkdown:
		s := strings.ReplaceAll(form.Get(f.Key), "\r\n", "\n")
		if strings.TrimSpace(s) == "" {
			return nil, nil //nolint:nilnil // reset
		}

		raw = s
	default:
		s := text(f.Key)
		if s == "" {
			return nil, nil //nolint:nilnil // reset
		}

		raw = s
	}

	v, err := domain.ValueOf(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid value: %w", err)
	}

	return &v, nil
}

// parseSection reads the submitted form of a section: the changes of the
// fields whose value differs from the effective one (untouched defaults
// never become DB rows), and the field errors of values that cannot be
// read. The returned view holds the submitted values.
func parseSection(view sectionView, snap *app.Snapshot, form url.Values) (sectionView, []domain.Change) {
	var changes []domain.Change

	for i := range view.Fields {
		f := &view.Fields[i]
		if f.Locked || f.Secret {
			continue
		}

		e, _ := snap.Get(f.Key)

		key, err := domain.NewKey(f.Key)
		if err != nil {
			continue
		}

		if v, err := strconv.ParseInt(form.Get("version."+f.Key), 10, 64); err == nil && v >= 0 {
			f.Version = v
		}

		v, err := formValue(*f, form)

		// Keep what was typed, for a re-render with errors.
		f.Value, f.Lat, f.Lon = form.Get(f.Key), form.Get(f.Key+".lat"), form.Get(f.Key+".lon")
		if f.Kind == domain.InputBoolean {
			vals := form[f.Key]
			f.Checked = len(vals) > 0 && vals[len(vals)-1] == "true"
		}

		if err != nil {
			f.Error = err.Error()

			continue
		}

		switch {
		case v == nil && (e.Source() == domain.SourceDB || e.Version() > 0):
			// A DB value, or an ignored (invalid) DB row: reset it.
			changes = append(changes, domain.ResetTo(key, f.Version))
		case v == nil:
			// Empty and not set in the DB: nothing to reset.
		case v.Equal(e.Value()):
			// Unchanged.
		default:
			changes = append(changes, domain.SetTo(key, *v, f.Version))
		}
	}

	return view, changes
}

// applyErrors puts the violations of a rejected write on their fields.
func applyErrors(view *sectionView, err error) {
	var de *shared.Error
	if !errors.As(err, &de) {
		view.Problems = append(view.Problems, problem{Text: "The settings could not be saved. Try again later."})

		return
	}

	switch {
	case errors.Is(err, domain.ErrVersionConflict):
		view.Problems = append(view.Problems, problem{Text: "These settings were changed meanwhile. Reload the page to see the current values, then save again."})
	case errors.Is(err, domain.ErrSettingLocked):
		view.Problems = append(view.Problems, problem{Text: "Some settings are now locked by the hub configuration: " + de.Message() + "."})
	}

	for _, vi := range de.Violations() {
		placed := false

		for i := range view.Fields {
			f := &view.Fields[i]
			if vi.Path() == f.Key || strings.HasPrefix(vi.Path(), f.Key+".") {
				if f.Error == "" {
					f.Error = sentence(vi.Message())
				}

				placed = true
			}
		}

		if !placed && !errors.Is(err, domain.ErrVersionConflict) {
			view.Problems = append(view.Problems, problem{Text: vi.Path() + ": " + sentence(vi.Message())})
		}
	}
}

// fieldProblems adds a summary entry per field error.
func fieldProblems(view *sectionView) {
	for _, f := range view.Fields {
		if f.Error != "" {
			view.Problems = append(view.Problems, problem{FieldID: f.ID, Text: f.Label + ": " + f.Error})
		}
	}
}

func sentence(s string) string {
	if s == "" {
		return s
	}

	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}

	return s
}
