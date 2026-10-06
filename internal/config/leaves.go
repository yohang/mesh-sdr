package config

import (
	"encoding"
	"reflect"
	"slices"
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

// expandMaps returns the leaves of the entries of every map-of-tables leaf
// (nodes.<id>.*, devices.<id>.*), keyed "<map>.<entry>.<field>", as
// addressable copies, and a store function writing the copies back into the
// maps. Map entries have no env override (ADR 0008).
func expandMaps(ls []leaf) ([]leaf, func()) {
	var (
		out    []leaf
		stores []func()
	)

	for _, lf := range ls {
		v := lf.value
		if v.Kind() != reflect.Map || v.Type().Key().Kind() != reflect.String || v.Type().Elem().Kind() != reflect.Struct {
			continue
		}

		keys := make([]string, 0, v.Len())
		for _, k := range v.MapKeys() {
			keys = append(keys, k.String())
		}

		slices.Sort(keys)

		for _, k := range keys {
			kv := reflect.ValueOf(k).Convert(v.Type().Key())
			cp := reflect.New(v.Type().Elem()).Elem()
			cp.Set(v.MapIndex(kv))

			var sub []leaf

			walk(cp, lf.key+"."+k+".", "", &sub)

			for i := range sub {
				sub[i].env = ""
			}

			out = append(out, sub...)
			stores = append(stores, func() { v.SetMapIndex(kv, cp) })
		}
	}

	return out, func() {
		for _, s := range stores {
			s()
		}
	}
}
