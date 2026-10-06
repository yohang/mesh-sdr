package config

import (
	"encoding"
	"reflect"
	"strings"
)

// leaf is one config key: a struct field that is not a nested table.
type leaf struct {
	key   string // dotted TOML key, e.g. "db.dsn"
	env   string // env var name without the MESHSDR_ prefix, "" if none
	value reflect.Value
}

var textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()

// leaves lists the config keys of the struct pointed to by v, in field order.
func leaves(v any) []leaf {
	var out []leaf

	walk(reflect.ValueOf(v).Elem(), "", "", &out)

	return out
}

func walk(v reflect.Value, keyPrefix, envPrefix string, out *[]leaf) {
	t := v.Type()

	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if name == "" || name == "-" {
			continue
		}

		key := keyPrefix + name
		fv := v.Field(i)

		if f.Type.Kind() == reflect.Struct && !reflect.PointerTo(f.Type).Implements(textUnmarshalerType) {
			walk(fv, key+".", envPrefix+f.Tag.Get("envPrefix"), out)

			continue
		}

		env := ""
		if tag, _, _ := strings.Cut(f.Tag.Get("env"), ","); tag != "" && tag != "-" {
			env = envPrefix + tag
		}

		*out = append(*out, leaf{key: key, env: env, value: fv})
	}
}
