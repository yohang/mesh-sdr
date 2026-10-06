package config

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/invopop/jsonschema"
)

// Schema returns the JSON Schema (draft 2020-12) of a role's config files,
// generated from the config structs (TECHNICAL_SPEC §7.4 "Format" rule 2).
//
// Every key carries the annotations x-scope ("global" for now) and lockable:
// true for keys under [settings], which map to DB settings that a config
// value locks; false for bootstrap keys, which are config-only. Secret keys
// carry secret: true.
func Schema(role Role) ([]byte, error) {
	var (
		target   any
		defaults any
	)

	switch role {
	case RoleHub:
		d := DefaultHub()
		target, defaults = &Hub{}, &d
	case RoleNode:
		d := DefaultNode()
		target, defaults = &Node{}, &d
	default:
		return nil, fmt.Errorf("unknown role %q", role)
	}

	r := &jsonschema.Reflector{
		FieldNameTag:               "toml",
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
		ExpandedStruct:             true,
	}

	s := r.Reflect(target)
	s.ID = jsonschema.ID("urn:meshsdr:config:" + string(role))
	s.Title = fmt.Sprintf("MeshSDR %s.toml", role)
	s.Description = fmt.Sprintf("Configuration of the %s role: %s.toml and %s.d/*.toml drop-ins. Every key can be overridden by a %s* env var (%s<TABLE>__<KEY>).",
		role, role, role, EnvPrefix, EnvPrefix)

	defaultValues := map[string]any{}

	for _, lf := range leaves(defaults) {
		if lf.value.IsZero() {
			continue
		}

		if m, ok := lf.value.Interface().(encoding.TextMarshaler); ok {
			b, err := m.MarshalText()
			if err != nil {
				return nil, fmt.Errorf("default %s: %w", lf.key, err)
			}

			defaultValues[lf.key] = string(b)

			continue
		}

		defaultValues[lf.key] = lf.value.Interface()
	}

	annotate(s, "", defaultValues)

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	if err := enc.Encode(s); err != nil {
		return nil, fmt.Errorf("encode schema: %w", err)
	}

	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func annotate(s *jsonschema.Schema, prefix string, defaults map[string]any) {
	if s.Properties == nil {
		return
	}

	for p := s.Properties.Oldest(); p != nil; p = p.Next() {
		key := prefix + p.Key
		child := p.Value

		if child.Properties != nil {
			annotate(child, key+".", defaults)

			continue
		}

		if isFileMetaKey(key) {
			continue
		}

		if child.Extras == nil {
			child.Extras = map[string]any{}
		}

		child.Extras["x-scope"] = "global"
		child.Extras["lockable"] = strings.HasPrefix(key, "settings.")

		if d, ok := defaults[key]; ok {
			child.Default = d
		}
	}
}
