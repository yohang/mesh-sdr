package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Violation codes of setting values. Semantic checks keep the code of the
// domain error that rejected the value.
const (
	CodeInvalidType = "invalid_type"
)

// maxLoginBurst bounds auth.login_rate_limit.
const maxLoginBurst = 100

// maxPolicyText is the maximum length of receiver.usage_policy_text, in
// characters (the schema maxLength).
const maxPolicyText = 20000

// errInvalidPolicyText rejects a usage policy the shell cannot show.
var errInvalidPolicyText = shared.NewError(shared.KindInvalid, "invalid_usage_policy",
	"usage policy must be 1 to "+strconv.Itoa(maxPolicyText)+" characters")

// ErrUnknownSetting is a key that the [settings] table does not define.
var ErrUnknownSetting = errors.New("unknown setting")

// settingsPrefix is the hub.toml table of the settings.
const settingsPrefix = "settings."

// settingLeaf is one settings key with its Go type and schema node.
type settingLeaf struct {
	key    string // without "settings."
	typ    reflect.Type
	schema map[string]any
}

type settingsIndex struct {
	order  []string
	byKey  map[string]*settingLeaf
	schema map[string]any // the "settings" node of the hub schema
}

// settingHooks are the semantic checks of keys, beyond the schema: the
// domain value object of a key's consumer. They receive the decoded Go
// value.
var settingHooks = map[string]func(v any) error{
	// Login attempts per client address: at most 100 per window, and a
	// window of at least one minute (at most 100 attempts per minute).
	"auth.login_rate_limit": func(v any) error {
		r, _ := v.(Rate)
		if r.Count() > maxLoginBurst || r.Window() < time.Minute {
			return fmt.Errorf("at most %d attempts per window of at least 1m", maxLoginBurst)
		}

		return nil
	},
	"links.callsign_url": linkTemplate,
	"links.vessel_url":   linkTemplate,
	"links.flight_url":   linkTemplate,
	"links.modes_url":    linkTemplate,
	"links.sonde_url":    linkTemplate,
	"links.geoip_url":    linkTemplate,
	"receiver.usage_policy_text": func(v any) error {
		s, _ := v.(string)

		s = strings.TrimSpace(s)
		if s == "" {
			return nil // empty: the built-in default policy
		}

		if utf8.RuneCountInString(s) > maxPolicyText || !utf8.ValidString(s) {
			return errInvalidPolicyText
		}

		return nil
	},
}

// errLinkTemplate rejects a lookup link template (ADM-032).
var errLinkTemplate = shared.NewError(shared.KindInvalid, "invalid_link_template",
	"must be an http or https URL with exactly one {} placeholder")

// linkTemplate checks a lookup link template: empty (no link), or an http
// or https URL with exactly one {} placeholder.
func linkTemplate(v any) error {
	s, _ := v.(string)
	if s == "" {
		return nil
	}

	if strings.Count(s, "{}") != 1 || !isHTTPURL(strings.Replace(s, "{}", "x", 1)) || strings.ContainsAny(s, " \t\r\n\\") {
		return errLinkTemplate
	}

	return nil
}

// loadSettingsIndex builds the index from the generated hub schema, so that
// validation reads exactly the published schema.
var loadSettingsIndex = sync.OnceValues(func() (*settingsIndex, error) {
	b, err := Schema(RoleHub)
	if err != nil {
		return nil, err
	}

	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("parse hub schema: %w", err)
	}

	settings := child(root, "settings")
	if settings == nil {
		return nil, errors.New("hub schema has no settings table")
	}

	idx := &settingsIndex{byKey: map[string]*settingLeaf{}, schema: settings}

	for _, lf := range leaves(&Hub{}) {
		key, ok := strings.CutPrefix(lf.key, settingsPrefix)
		if !ok {
			continue
		}

		node := settings
		for p := range strings.SplitSeq(key, ".") {
			node = child(node, p)
		}

		if node == nil {
			return nil, fmt.Errorf("hub schema has no property %s", lf.key)
		}

		idx.order = append(idx.order, key)
		idx.byKey[key] = &settingLeaf{key: key, typ: lf.value.Type(), schema: node}
	}

	return idx, nil
})

func child(node map[string]any, name string) map[string]any {
	if node == nil {
		return nil
	}

	props, _ := node["properties"].(map[string]any)
	c, _ := props[name].(map[string]any)

	return c
}

// DecodeSetting checks a setting value given as JSON, whatever its source
// (DB row, API write): decode into the key's Go type, semantic check of the
// key, then the schema keywords. It returns the decoded Go value, or the
// violations (path = the key); the error is ErrUnknownSetting for a key
// outside the schema.
func DecodeSetting(key string, raw []byte) (any, []shared.Violation, error) {
	idx, err := loadSettingsIndex()
	if err != nil {
		return nil, nil, err
	}

	leaf, ok := idx.byKey[key]
	if !ok {
		return nil, nil, fmt.Errorf("%w %s", ErrUnknownSetting, key)
	}

	ptr := reflect.New(leaf.typ)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(ptr.Interface()); err != nil || dec.More() {
		return nil, []shared.Violation{decodeViolation(key, leaf.typ, err)}, nil
	}

	v := ptr.Elem().Interface()
	if vs := checkSetting(leaf, v, raw); len(vs) > 0 {
		return nil, vs, nil
	}

	return v, nil, nil
}

// SettingKey is one key of the [settings] table of a hub config.
type SettingKey struct {
	// Key is the key without "settings.".
	Key string
	// Type is its Go type, Schema its node of the hub JSON Schema.
	Type   reflect.Type
	Schema map[string]any
	// Default is its default value, Value its value in the config, set
	// from Origin.
	Default, Value any
	Origin         Origin
}

// SettingKeys lists the [settings] keys of a loaded hub config, cfg with
// the origins of its keys (Meta.Origins), in schema order, and returns the
// JSON Schema of the [settings] table (its property paths are the keys).
func SettingKeys(cfg Hub, origins Origins) ([]SettingKey, json.RawMessage, error) {
	idx, err := loadSettingsIndex()
	if err != nil {
		return nil, nil, err
	}

	defaults := DefaultHub()
	defLeaves := map[string]leaf{}

	for _, lf := range leaves(&defaults) {
		defLeaves[lf.key] = lf
	}

	var out []SettingKey

	for _, lf := range leaves(&cfg) {
		key, ok := strings.CutPrefix(lf.key, settingsPrefix)
		if !ok {
			continue
		}

		sl := idx.byKey[key]
		out = append(out, SettingKey{
			Key: key, Type: sl.typ, Schema: sl.schema, Default: defLeaves[lf.key].value.Interface(), Value: lf.value.Interface(),
			Origin: origins.Of(lf.key),
		})
	}

	schema, err := json.Marshal(idx.schema)
	if err != nil {
		return nil, nil, fmt.Errorf("encode settings schema: %w", err)
	}

	return out, schema, nil
}

func decodeViolation(key string, typ reflect.Type, err error) shared.Violation {
	var ute *json.UnmarshalTypeError
	if err == nil || errors.As(err, &ute) || errors.Is(err, errGeoShape) || strings.HasPrefix(fmt.Sprint(err), "json: unknown field") {
		return shared.NewViolation(key, CodeInvalidType, "want "+jsonTypeName(typ))
	}

	// A unit type (duration, rate, position) rejected the value.
	return shared.NewViolation(key, CodeInvalidValue, err.Error())
}

func jsonTypeName(t reflect.Type) string {
	switch t {
	case reflect.TypeFor[Duration]():
		return "a duration string such as \"15m\" or \"30d\""
	case reflect.TypeFor[Rate]():
		return "a rate string such as \"5/1m\""
	case reflect.TypeFor[GeoPoint]():
		return `an object {"lat": <deg>, "lon": <deg>}`
	}

	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice:
		return "an array"
	default:
		return "a valid value"
	}
}

// checkSetting runs the semantic check of the key on the Go value, then the
// schema keywords on the JSON form.
func checkSetting(leaf *settingLeaf, v any, raw []byte) []shared.Violation {
	if hook := settingHooks[leaf.key]; hook != nil {
		if err := hook(v); err != nil {
			var de *shared.Error
			if errors.As(err, &de) {
				return []shared.Violation{shared.NewViolation(leaf.key, de.Code(), de.Message())}
			}

			return []shared.Violation{shared.NewViolation(leaf.key, CodeInvalidValue, err.Error())}
		}
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var generic any
	if err := dec.Decode(&generic); err != nil {
		return []shared.Violation{shared.NewViolation(leaf.key, CodeInvalidType, err.Error())}
	}

	var out []shared.Violation

	checkNode(leaf.key, leaf.schema, generic, &out)

	return out
}

var patternCache sync.Map // pattern → *regexp.Regexp

func compiled(p string) *regexp.Regexp {
	if re, ok := patternCache.Load(p); ok {
		return re.(*regexp.Regexp) //nolint:forcetypeassert // only regexps are stored
	}

	re, err := regexp.Compile(p)
	if err != nil {
		re = regexp.MustCompile(`^\b$`) // an invalid schema pattern matches nothing
	}

	patternCache.Store(p, re)

	return re
}

// checkNode checks v against the subset of JSON Schema keywords the
// generated schema uses: type, enum, minimum, maximum, minLength,
// maxLength, pattern, format (email, uri, uri-reference), properties,
// required, additionalProperties: false, items, minItems, maxItems, and the
// x-min-duration and x-max-duration extensions.
func checkNode(path string, s map[string]any, v any, out *[]shared.Violation) {
	fail := func(code, msg string) { *out = append(*out, shared.NewViolation(path, shared.Code(code), msg)) }

	if t, ok := s["type"].(string); ok && !hasType(t, v) {
		fail(CodeInvalidType, "want "+article(t))

		return
	}

	if enum, ok := s["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return sameJSON(e, v) }) {
		fail(CodeInvalidValue, fmt.Sprintf("must be one of %s", enumList(enum)))

		return
	}

	switch x := v.(type) {
	case string:
		checkString(s, x, fail)
	case json.Number:
		f, _ := x.Float64()

		if m, ok := s["minimum"].(float64); ok && f < m {
			fail(CodeInvalidValue, fmt.Sprintf("must be at least %v", m))
		}

		if m, ok := s["maximum"].(float64); ok && f > m {
			fail(CodeInvalidValue, fmt.Sprintf("must be at most %v", m))
		}
	case map[string]any:
		props, _ := s["properties"].(map[string]any)

		for _, k := range sortedKeys(x) {
			ps, ok := props[k].(map[string]any)
			if !ok {
				if s["additionalProperties"] == false {
					*out = append(*out, shared.NewViolation(path+"."+k, CodeInvalidValue, "unknown property"))
				}

				continue
			}

			checkNode(path+"."+k, ps, x[k], out)
		}

		req, _ := s["required"].([]any)
		for _, r := range req {
			if name, _ := r.(string); name != "" {
				if _, ok := x[name]; !ok {
					*out = append(*out, shared.NewViolation(path+"."+name, CodeRequired, "required"))
				}
			}
		}
	case []any:
		if m, ok := s["minItems"].(float64); ok && float64(len(x)) < m {
			fail(CodeInvalidValue, fmt.Sprintf("must have at least %v items", m))
		}

		if m, ok := s["maxItems"].(float64); ok && float64(len(x)) > m {
			fail(CodeInvalidValue, fmt.Sprintf("must have at most %v items", m))
		}

		if items, ok := s["items"].(map[string]any); ok {
			for i, e := range x {
				checkNode(fmt.Sprintf("%s[%d]", path, i), items, e, out)
			}
		}
	}
}

func checkString(s map[string]any, x string, fail func(code, msg string)) {
	n := utf8.RuneCountInString(x)

	if m, ok := s["minLength"].(float64); ok && float64(n) < m {
		if m == 1 {
			fail(CodeRequired, "must not be empty")
		} else {
			fail(CodeInvalidValue, fmt.Sprintf("must be at least %v characters", m))
		}

		return
	}

	if m, ok := s["maxLength"].(float64); ok && float64(n) > m {
		fail(CodeInvalidValue, fmt.Sprintf("must be at most %v characters", m))

		return
	}

	if x == "" {
		return // an empty optional string means "not set"
	}

	if p, ok := s["pattern"].(string); ok && !compiled(p).MatchString(x) {
		fail(CodeInvalidValue, "invalid format")

		return
	}

	if f, ok := s["format"].(string); ok && !validFormat(f, x) {
		fail(CodeInvalidValue, formatMessage(f))

		return
	}

	for _, bound := range []struct {
		key  string
		less bool
	}{{"x-min-duration", true}, {"x-max-duration", false}} {
		limit, ok := s[bound.key].(string)
		if !ok {
			continue
		}

		d, err1 := ParseDuration(x)
		l, err2 := ParseDuration(limit)

		if err1 != nil || err2 != nil {
			continue
		}

		if bound.less && d.Duration() < l.Duration() {
			fail(CodeInvalidValue, "must be at least "+limit)
		}

		if !bound.less && d.Duration() > l.Duration() {
			fail(CodeInvalidValue, "must be at most "+limit)
		}
	}
}

func hasType(t string, v any) bool {
	switch t {
	case "string":
		_, ok := v.(string)

		return ok
	case "boolean":
		_, ok := v.(bool)

		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}

		f, err := n.Float64()

		return err == nil && f == math.Trunc(f)
	case "number":
		_, ok := v.(json.Number)

		return ok
	case "object":
		_, ok := v.(map[string]any)

		return ok
	case "array":
		_, ok := v.([]any)

		return ok
	default:
		return true
	}
}

func article(t string) string {
	switch t {
	case "integer", "object", "array":
		return "an " + t
	default:
		return "a " + t
	}
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)

	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func enumList(enum []any) string {
	parts := make([]string, len(enum))
	for i, e := range enum {
		parts[i] = fmt.Sprint(e)
	}

	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

func validFormat(format, s string) bool {
	switch format {
	case "email":
		a, err := mail.ParseAddress(s)

		return err == nil && a.Name == "" && a.Address == s
	case "uri":
		return isHTTPURL(s)
	case "uri-reference":
		if strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//") && !strings.ContainsAny(s, "\\ \t\r\n") {
			_, err := url.Parse(s)

			return err == nil
		}

		return isHTTPURL(s)
	default:
		return true
	}
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)

	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

func formatMessage(format string) string {
	switch format {
	case "email":
		return "must be an e-mail address"
	case "uri":
		return "must be an http or https URL"
	case "uri-reference":
		return "must be a path on this hub (/…) or an http or https URL"
	default:
		return "invalid format"
	}
}

// settingRules are the checks across keys, run on the effective values
// (config, DB and defaults together).
type settingRule struct {
	keys  []string
	check func(get func(key string) any) []shared.Violation
}

var settingRules = []settingRule{
	{[]string{"waterfall.min_db", "waterfall.max_db"}, func(get func(string) any) []shared.Violation {
		lo, _ := get("waterfall.min_db").(int)
		hi, _ := get("waterfall.max_db").(int)

		if hi <= lo {
			return []shared.Violation{shared.NewViolation("waterfall.max_db", CodeInvalidValue,
				fmt.Sprintf("must be above waterfall.min_db (%d)", lo))}
		}

		return nil
	}},
	{[]string{"grid.heartbeat_interval_s", "grid.offline_after_s"}, func(get func(string) any) []shared.Violation {
		hb, _ := get("grid.heartbeat_interval_s").(int)
		off, _ := get("grid.offline_after_s").(int)

		if off <= 2*hb {
			return []shared.Violation{shared.NewViolation("grid.offline_after_s", CodeInvalidValue,
				fmt.Sprintf("must be more than twice grid.heartbeat_interval_s (%d)", hb))}
		}

		return nil
	}},
	{[]string{"auth.lockout.delay_after", "auth.lockout.lock_after"}, func(get func(string) any) []shared.Violation {
		delay, _ := get("auth.lockout.delay_after").(int)
		lock, _ := get("auth.lockout.lock_after").(int)

		if lock <= delay {
			return []shared.Violation{shared.NewViolation("auth.lockout.lock_after", CodeInvalidValue,
				fmt.Sprintf("must be greater than auth.lockout.delay_after (%d)", delay))}
		}

		return nil
	}},
	{[]string{"map.base_layers", "map.default_base_layer"}, func(get func(string) any) []shared.Violation {
		layers, _ := get("map.base_layers").([]string)
		def, _ := get("map.default_base_layer").(string)

		if !slices.Contains(layers, def) {
			return []shared.Violation{shared.NewViolation("map.default_base_layer", CodeInvalidValue,
				"must be one of the offered base layers (map.base_layers)")}
		}

		return nil
	}},
	{[]string{"auth.lockout.lock_for", "auth.lockout.max_lock"}, func(get func(string) any) []shared.Violation {
		lockFor, _ := get("auth.lockout.lock_for").(Duration)
		maxLock, _ := get("auth.lockout.max_lock").(Duration)

		if maxLock.Duration() < lockFor.Duration() {
			return []shared.Violation{shared.NewViolation("auth.lockout.max_lock", CodeInvalidValue,
				"must not be shorter than auth.lockout.lock_for ("+lockFor.String()+")")}
		}

		return nil
	}},
}

// CheckSettings runs the checks across keys on the effective values, given
// by key (the Go values DecodeSetting returns; ok false when unknown). A
// check runs only when all its keys are known.
func CheckSettings(get func(key string) (any, bool)) []shared.Violation {
	var out []shared.Violation

	for _, rule := range settingRules {
		known := true

		for _, k := range rule.keys {
			_, ok := get(k)
			known = known && ok
		}

		if known {
			out = append(out, rule.check(func(k string) any { v, _ := get(k); return v })...)
		}
	}

	return out
}
