package render

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// Form is the state of a submitted HTML form: the values as typed, the
// errors by field, and the failure of the whole form.
type Form struct {
	Values  map[string]string
	Errors  map[string]string
	Failure string
	// Conflict and Broken select the status of a failed save.
	Conflict, Broken bool
}

// SetError records the error of a field; the first one of a field wins. A
// dotted path ("tags.2") is the error of its top-level field.
func (f *Form) SetError(field, msg string) {
	field, _, _ = strings.Cut(field, ".")

	if f.Errors == nil {
		f.Errors = map[string]string{}
	}

	if _, ok := f.Errors[field]; !ok {
		f.Errors[field] = msg
	}
}

// AddViolations records the violations of a domain error on their fields;
// without violations its message is the failure of the form. It reports
// whether err is a domain error.
func (f *Form) AddViolations(err error) bool {
	var de *shared.Error
	if !errors.As(err, &de) {
		return false
	}

	for _, v := range de.Violations() {
		f.SetError(v.Path(), Sentence(v.Message()))
	}

	if len(de.Violations()) == 0 {
		f.Failure = Sentence(de.Message())
	}

	return true
}

// Status is the status of the answer to a refused form: 500 when broken,
// 409 on a conflict, 422 otherwise.
func (f *Form) Status() int {
	switch {
	case f.Broken:
		return http.StatusInternalServerError
	case f.Conflict:
		return http.StatusConflict
	default:
		return http.StatusUnprocessableEntity
	}
}

// Field names a field of a form for its error summary.
type Field struct{ Key, Label string }

// Problems lists the field errors in the order of fields, linked to the
// input id idPrefix+key, then the other errors, sorted.
func (f *Form) Problems(idPrefix string, fields []Field) []layout.Problem {
	var out []layout.Problem

	for _, fl := range fields {
		if msg, ok := f.Errors[fl.Key]; ok {
			out = append(out, layout.Problem{ID: idPrefix + fl.Key, Text: fl.Label + ": " + msg})
		}
	}

	var others []string

	for k, msg := range f.Errors {
		if !slices.ContainsFunc(fields, func(fl Field) bool { return fl.Key == k }) {
			others = append(others, msg)
		}
	}

	slices.Sort(others)

	for _, msg := range others {
		out = append(out, layout.Problem{Text: msg})
	}

	return out
}
