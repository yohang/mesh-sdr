package settings

import (
	"slices"
	"strconv"
)

// Apply tells when a change of a key takes effect (schema `x-apply`).
type Apply string

// Apply modes.
const (
	// ApplyLive keys are read on every use: a change applies at once.
	ApplyLive Apply = "live"
	// ApplyRestart keys are read at start: a change applies after a hub
	// restart.
	ApplyRestart Apply = "restart"
)

// InputKind is the kind of form control a key is edited with. It comes from
// the Go type of the key, the schema `format` and `x-widget`.
type InputKind string

// Input kinds.
const (
	InputText     InputKind = "text"
	InputTextarea InputKind = "textarea"
	InputMarkdown InputKind = "markdown"
	InputEmail    InputKind = "email"
	InputURL      InputKind = "url"
	InputBoolean  InputKind = "boolean"
	InputInteger  InputKind = "integer"
	InputNumber   InputKind = "number"
	InputDuration InputKind = "duration"
	InputRate     InputKind = "rate"
	InputEnum     InputKind = "enum"
	InputGeo      InputKind = "geo"
	InputList     InputKind = "list"
)

var inputKinds = []InputKind{
	InputText, InputTextarea, InputMarkdown, InputEmail, InputURL, InputBoolean, InputInteger,
	InputNumber, InputDuration, InputRate, InputEnum, InputGeo, InputList,
}

// Input describes how a key is edited: the control and the bounds the
// schema declares (shown to the user; the server validator is
// authoritative).
type Input struct {
	Kind    InputKind
	Options []string // InputEnum
	// OptionLabels, when set, are the labels shown for Options (same order).
	OptionLabels []string
	Min, Max     *float64 // InputInteger, InputNumber
	MinText      string   // InputDuration: the smallest duration, e.g. "30d"
	MaxLength    int      // text kinds; 0 means no limit
	Pattern      string
}

// Definition is a key of the settings schema: its default, flags and input.
type Definition struct {
	key         string
	def         Value
	label       string
	description string
	secret      bool
	public      bool
	apply       Apply
	input       Input
}

// DefinitionSpec holds the fields of a definition.
type DefinitionSpec struct {
	Key         string
	Default     Value
	Label       string
	Description string
	Secret      bool
	Public      bool
	Apply       Apply
	Input       Input
}

// NewDefinition validates a definition. A secret key is never public.
func NewDefinition(s DefinitionSpec) (Definition, error) {
	invalid := func(why string) (Definition, error) {
		return Definition{}, ErrInvalidKey.WithDetail("definition of " + strconv.Quote(s.Key) + ": " + why)
	}

	switch {
	case !ValidKey(s.Key):
		return invalid("invalid key")
	case s.Apply != ApplyLive && s.Apply != ApplyRestart:
		return invalid("invalid x-apply " + strconv.Quote(string(s.Apply)))
	case !slices.Contains(inputKinds, s.Input.Kind):
		return invalid("invalid input kind " + strconv.Quote(string(s.Input.Kind)))
	case s.Input.Kind == InputEnum && len(s.Input.Options) == 0:
		return invalid("enum without options")
	case s.Secret && s.Public:
		return invalid("a secret key cannot be public")
	}

	label := s.Label
	if label == "" {
		label = s.Key
	}

	in := s.Input
	in.Options = slices.Clone(in.Options)
	in.OptionLabels = slices.Clone(in.OptionLabels)

	return Definition{
		key: s.Key, def: s.Default, label: label, description: s.Description,
		secret: s.Secret, public: s.Public, apply: s.Apply, input: in,
	}, nil
}

// Key returns the key.
func (d Definition) Key() string { return d.key }

// Default returns the built-in default (JSON null for an optional key).
func (d Definition) Default() Value { return d.def }

// Label returns the human label.
func (d Definition) Label() string { return d.label }

// Description returns the help text.
func (d Definition) Description() string { return d.description }

// Secret reports whether the value is write-only (schema `secret: true`).
func (d Definition) Secret() bool { return d.secret }

// Public reports whether anonymous clients may read the value
// (`GET /settings/public`).
func (d Definition) Public() bool { return d.public }

// Apply tells when a change takes effect.
func (d Definition) Apply() Apply { return d.apply }

// Input returns how the key is edited.
func (d Definition) Input() Input {
	in := d.input
	in.Options = slices.Clone(in.Options)
	in.OptionLabels = slices.Clone(in.OptionLabels)

	return in
}
