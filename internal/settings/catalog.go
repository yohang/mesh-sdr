package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/yohang/mesh-sdr/internal/config"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ConfigCatalog is the settings schema of a hub config (ADR 0010): the key
// definitions, the values the hub config sets (locked) and the validator
// of the config ([settings] table of hub.toml).
type ConfigCatalog struct {
	defs       []Definition
	configured map[string]Configured
	schema     json.RawMessage
}

// NewConfigCatalog builds the catalog of a loaded hub config: cfg and the
// origins of its keys (config.Meta.Origins).
func NewConfigCatalog(cfg config.Hub, origins config.Origins) (*ConfigCatalog, error) {
	keys, schema, err := config.SettingKeys(cfg, origins)
	if err != nil {
		return nil, err
	}

	c := &ConfigCatalog{configured: map[string]Configured{}, schema: schema}

	for _, k := range keys {
		def, err := definition(k)
		if err != nil {
			return nil, err
		}

		c.defs = append(c.defs, def)

		if k.Origin.Locked() {
			v := Value{} // a secret is never copied out of the config

			if !def.Secret() {
				if v, err = ValueOf(k.Value); err != nil {
					return nil, fmt.Errorf("settings.%s: %w", k.Key, err)
				}
			}

			c.configured[k.Key] = Configured{Value: v, Origin: k.Origin.String()}
		}
	}

	return c, nil
}

func definition(k config.SettingKey) (Definition, error) {
	dv, err := ValueOf(k.Default)
	if err != nil {
		return Definition{}, fmt.Errorf("default of %s: %w", k.Key, err)
	}

	s := k.Schema
	str := func(key string) string { v, _ := s[key].(string); return v }
	flag := func(key string) bool { v, _ := s[key].(bool); return v }

	apply := Apply(str("x-apply"))
	if apply == "" {
		apply = ApplyLive
	}

	return NewDefinition(DefinitionSpec{
		Key: k.Key, Default: dv, Label: str("x-label"), Description: str("description"),
		Secret: flag("secret"), Public: flag("x-public"), Apply: apply, Input: input(k.Type, s),
	})
}

func input(t reflect.Type, s map[string]any) Input {
	num := func(k string) *float64 {
		if v, ok := s[k].(float64); ok {
			return &v
		}

		return nil
	}

	in := Input{Min: num("minimum"), Max: num("maximum")}

	if v, ok := s["maxLength"].(float64); ok {
		in.MaxLength = int(v)
	}

	in.Pattern, _ = s["pattern"].(string)

	switch t {
	case reflect.TypeFor[config.Duration]():
		in.Kind = InputDuration
		in.MinText, _ = s["x-min-duration"].(string)

		return in
	case reflect.TypeFor[config.Rate]():
		in.Kind = InputRate

		return in
	case reflect.TypeFor[config.GeoPoint]():
		in.Kind = InputGeo

		return in
	}

	switch t.Kind() {
	case reflect.Bool:
		in.Kind = InputBoolean
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		in.Kind = InputInteger
	case reflect.Float32, reflect.Float64:
		in.Kind = InputNumber
	case reflect.Slice:
		in.Kind = InputList
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

func stringInput(s map[string]any, maxLength int) InputKind {
	if _, ok := s["enum"]; ok {
		return InputEnum
	}

	if w, _ := s["x-widget"].(string); w == string(InputMarkdown) {
		return InputMarkdown
	}

	switch f, _ := s["format"].(string); f {
	case "email":
		return InputEmail
	case "uri", "uri-reference":
		return InputURL
	}

	if maxLength > 512 {
		return InputTextarea
	}

	return InputText
}

// Definitions returns the key definitions in schema order.
func (c *ConfigCatalog) Definitions() []Definition { return slices.Clone(c.defs) }

// Configured returns the value the hub config sets for key, if any (a
// secret's value stays empty).
func (c *ConfigCatalog) Configured(key string) (Configured, bool) {
	v, ok := c.configured[key]

	return v, ok
}

// Validate checks a value of key (config.DecodeSetting) and returns its Go
// value, or ErrUnknownSetting, or ErrInvalidSetting with one violation per
// problem.
func (c *ConfigCatalog) Validate(key string, v Value) (any, error) {
	out, violations, err := config.DecodeSetting(key, v.JSON())

	switch {
	case errors.Is(err, config.ErrUnknownSetting):
		return nil, ErrUnknownSetting.WithDetail("unknown setting " + key)
	case err != nil:
		return nil, err
	case len(violations) > 0:
		return nil, ErrInvalidSetting.WithViolations(violations...)
	}

	return out, nil
}

// Check runs the checks across keys on the effective Go values.
func (c *ConfigCatalog) Check(get func(key string) (any, bool)) []shared.Violation {
	return config.CheckSettings(get)
}

// Schema returns the JSON Schema of the settings namespace (the [settings]
// table of hub.toml, whose property paths are the DB keys).
func (c *ConfigCatalog) Schema() json.RawMessage { return c.schema }
