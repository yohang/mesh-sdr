package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/yohang/mesh-sdr/internal/settings"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// SettingsCatalog is the settings schema seen by the settings store (ADR
// 0010): the key definitions, the values the hub config sets (locked) and
// the validator.
type SettingsCatalog struct {
	defs       []settings.Definition
	configured map[string]settings.Configured
	schema     json.RawMessage
}

// NewSettingsCatalog builds the catalog of a loaded hub config: cfg and the
// origins of its keys (Meta.Origins).
func NewSettingsCatalog(cfg Hub, origins Origins) (*SettingsCatalog, error) {
	idx, err := loadSettingsIndex()
	if err != nil {
		return nil, err
	}

	defaults := DefaultHub()
	defLeaves := map[string]leaf{}

	for _, lf := range leaves(&defaults) {
		defLeaves[lf.key] = lf
	}

	c := &SettingsCatalog{configured: map[string]settings.Configured{}}

	for _, lf := range leaves(&cfg) {
		key, ok := strings.CutPrefix(lf.key, settingsPrefix)
		if !ok {
			continue
		}

		sl := idx.byKey[key]

		def, err := definition(sl, defLeaves[lf.key].value.Interface())
		if err != nil {
			return nil, err
		}

		c.defs = append(c.defs, def)

		if o := origins.Of(lf.key); o.Locked() {
			v := settings.Value{} // a secret is never copied out of the config

			if !def.Secret() {
				if v, err = settings.ValueOf(lf.value.Interface()); err != nil {
					return nil, fmt.Errorf("%s: %w", lf.key, err)
				}
			}

			c.configured[key] = settings.Configured{Value: v, Origin: o.String()}
		}
	}

	c.schema, err = json.Marshal(idx.schema)
	if err != nil {
		return nil, fmt.Errorf("encode settings schema: %w", err)
	}

	return c, nil
}

func definition(sl *settingLeaf, def any) (settings.Definition, error) {
	key := sl.key

	dv, err := settings.ValueOf(def)
	if err != nil {
		return settings.Definition{}, fmt.Errorf("default of %s: %w", sl.key, err)
	}

	s := sl.schema
	str := func(k string) string { v, _ := s[k].(string); return v }
	flag := func(k string) bool { v, _ := s[k].(bool); return v }

	apply := settings.Apply(str("x-apply"))
	if apply == "" {
		apply = settings.ApplyLive
	}

	return settings.NewDefinition(settings.DefinitionSpec{
		Key: key, Default: dv, Label: str("x-label"), Description: str("description"),
		Secret: flag("secret"), Public: flag("x-public"), Apply: apply, Input: input(sl.typ, s),
	})
}

func input(t reflect.Type, s map[string]any) settings.Input {
	num := func(k string) *float64 {
		if v, ok := s[k].(float64); ok {
			return &v
		}

		return nil
	}

	in := settings.Input{Min: num("minimum"), Max: num("maximum")}

	if v, ok := s["maxLength"].(float64); ok {
		in.MaxLength = int(v)
	}

	in.Pattern, _ = s["pattern"].(string)

	switch t {
	case reflect.TypeFor[Duration]():
		in.Kind = settings.InputDuration
		in.MinText, _ = s["x-min-duration"].(string)

		return in
	case reflect.TypeFor[Rate]():
		in.Kind = settings.InputRate

		return in
	case reflect.TypeFor[GeoPoint]():
		in.Kind = settings.InputGeo

		return in
	}

	switch t.Kind() {
	case reflect.Bool:
		in.Kind = settings.InputBoolean
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		in.Kind = settings.InputInteger
	case reflect.Float32, reflect.Float64:
		in.Kind = settings.InputNumber
	case reflect.Slice:
		in.Kind = settings.InputList
	default:
		in.Kind = stringInput(s, in.MaxLength)

		if enum, ok := s["enum"].([]any); ok {
			for _, e := range enum {
				in.Options = append(in.Options, fmt.Sprint(e))
			}
		}

		in.OptionLabels = enumLabels(s["x-enum-labels"], len(in.Options))
	}

	return in
}

// enumLabels reads x-enum-labels, the labels shown for the enum options in
// order (one per option, or none).
func enumLabels(v any, n int) []string {
	var out []string

	switch l := v.(type) {
	case []string:
		out = l
	case []any:
		for _, e := range l {
			out = append(out, fmt.Sprint(e))
		}
	}

	if len(out) != n {
		return nil
	}

	return out
}

func stringInput(s map[string]any, maxLength int) settings.InputKind {
	if _, ok := s["enum"]; ok {
		return settings.InputEnum
	}

	if w, _ := s["x-widget"].(string); w == string(settings.InputMarkdown) {
		return settings.InputMarkdown
	}

	switch f, _ := s["format"].(string); f {
	case "email":
		return settings.InputEmail
	case "uri", "uri-reference":
		return settings.InputURL
	}

	if maxLength > 512 {
		return settings.InputTextarea
	}

	return settings.InputText
}

// Definitions returns the key definitions in schema order.
func (c *SettingsCatalog) Definitions() []settings.Definition { return slices.Clone(c.defs) }

// Configured returns the value the hub config sets for key, if any (a
// secret's value stays empty).
func (c *SettingsCatalog) Configured(key string) (settings.Configured, bool) {
	v, ok := c.configured[key]

	return v, ok
}

// Validate checks a value of key (ValidateSetting) and returns its Go value.
func (c *SettingsCatalog) Validate(key string, v settings.Value) (any, error) {
	return ValidateSetting(key, v.JSON())
}

// Check runs the checks across keys on the effective Go values.
func (c *SettingsCatalog) Check(get func(key string) (any, bool)) []shared.Violation {
	return CheckSettings(get)
}

// Schema returns the JSON Schema of the settings namespace (the [settings]
// table of hub.toml, whose property paths are the DB keys).
func (c *SettingsCatalog) Schema() json.RawMessage { return c.schema }
