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
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	settingsdomain "github.com/yohang/mesh-sdr/internal/settings/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	shelldomain "github.com/yohang/mesh-sdr/internal/shell/domain"
)

// Violation codes of setting values. Semantic checks keep the code of the
// domain error that rejected the value.
const (
	CodeInvalidType = "invalid_type"
)

// maxLoginBurst bounds auth.login_rate_limit.
const maxLoginBurst = 100

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
	"receiver.usage_policy_text": func(v any) error {
		s, _ := v.(string)
		if strings.TrimSpace(s) == "" {
			return nil // empty: the built-in default policy
		}

		_, err := shelldomain.NewPolicyText(s)

		return err
	},
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

// ValidateSetting checks a setting value given as JSON, whatever its source
// (DB row, API write): decode into the key's Go type, semantic check of the
// key, then the schema keywords. It returns the decoded Go value, or an
// error: settingsdomain.ErrUnknownSetting, or settingsdomain.ErrInvalidSetting
// with one violation per problem (path = the key).
func ValidateSetting(key string, raw []byte) (any, error) {
	idx, err := loadSettingsIndex()
	if err != nil {
		return nil, err
	}

	leaf, ok := idx.byKey[key]
	if !ok {
		return nil, settingsdomain.ErrUnknownSetting.WithDetail("unknown setting " + key)
	}

	ptr := reflect.New(leaf.typ)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(ptr.Interface()); err != nil || dec.More() {
		return nil, settingsdomain.ErrInvalidSetting.WithViolations(decodeViolation(key, leaf.typ, err))
	}

	v := ptr.Elem().Interface()
	if vs := checkSetting(leaf, v, raw); len(vs) > 0 {
		return nil, settingsdomain.ErrInvalidSetting.WithViolations(vs...)
	}

	return v, nil
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
	{[]string{"auth.lockout.delay_after", "auth.lockout.lock_after"}, func(get func(string) any) []shared.Violation {
		delay, _ := get("auth.lockout.delay_after").(int)
		lock, _ := get("auth.lockout.lock_after").(int)

		if lock <= delay {
			return []shared.Violation{shared.NewViolation("auth.lockout.lock_after", CodeInvalidValue,
				fmt.Sprintf("must be greater than auth.lockout.delay_after (%d)", delay))}
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
// by key (the Go values ValidateSetting returns; ok false when unknown). A
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
