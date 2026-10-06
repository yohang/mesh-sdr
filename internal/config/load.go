package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/caarlos0/env/v11"
)

const (
	// EnvPrefix prefixes every config env var.
	EnvPrefix = "MESHSDR_"
	// EnvConfigDir overrides the config directory.
	EnvConfigDir = EnvPrefix + "CONFIG_DIR"
)

// Options tells Load where to read from.
type Options struct {
	// Dir is the config directory. Empty means DefaultDir.
	Dir string
	// Env is the environment. Nil means the process environment.
	Env map[string]string
}

// Meta describes a loaded configuration.
type Meta struct {
	// Files lists the files read, relative to the config dir, in merge order.
	Files []string
	// Origins gives the origin of every key.
	Origins Origins
	// Warnings are non-fatal findings (for example inline secrets).
	Warnings []string
}

// Problem codes.
const (
	CodeConfigMissing            = "config_missing"
	CodeConfigUnreadable         = "config_unreadable"
	CodeParse                    = "config_parse_error"
	CodeSchemaVersionMissing     = "schema_version_missing"
	CodeSchemaVersionUnsupported = "schema_version_unsupported"
	CodeUnknownKey               = "unknown_key"
	CodeUnknownEnv               = "unknown_env_var"
	CodeInvalidValue             = "invalid_value"
	CodeRequired                 = "required"
	CodeInlineSecretForbidden    = "inline_secret_forbidden"
	CodeInsecureSecretFile       = "insecure_secret_file"
	CodeSecretUnresolved         = "secret_unresolved"
	CodeDBEngineUnsupported      = "db_engine_unsupported"
)

// Problem is one configuration error.
type Problem struct {
	Key     string // dotted key or env var, may be empty
	Origin  string // "hub.toml:12", "env:MESHSDR_X", "default" or empty
	Code    string
	Message string
}

func (p Problem) String() string {
	var parts []string

	if p.Origin != "" {
		parts = append(parts, p.Origin)
	}

	if p.Key != "" {
		parts = append(parts, p.Key)
	}

	parts = append(parts, p.Message+" ("+p.Code+")")

	return strings.Join(parts, ": ")
}

// Error is an invalid configuration. The process must exit with code 78
// (EX_CONFIG).
type Error struct {
	Role     Role
	Problems []Problem
}

func (e *Error) Error() string {
	msgs := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		msgs[i] = p.String()
	}

	return fmt.Sprintf("invalid %s configuration: %s", e.Role, strings.Join(msgs, "; "))
}

// LoadHub loads and validates the hub configuration.
func LoadHub(opts Options) (Hub, Meta, error) {
	cfg := DefaultHub()
	meta, err := load(RoleHub, &cfg, opts)

	return cfg, meta, err
}

// LoadNode loads and validates the node configuration.
func LoadNode(opts Options) (Node, Meta, error) {
	cfg := DefaultNode()
	meta, err := load(RoleNode, &cfg, opts)

	return cfg, meta, err
}

type validator interface {
	validate(o Origins) []Problem
}

type loader struct {
	role     Role
	dir      string
	env      map[string]string
	leaves   []leaf
	origins  Origins
	problems []Problem
	warnings []string
}

func (l *loader) fail(p Problem) { l.problems = append(l.problems, p) }

func load[T any, PT interface {
	*T
	validator
}](role Role, cfg PT, opts Options) (Meta, error) {
	l := &loader{role: role, dir: opts.Dir, env: opts.Env, origins: Origins{}}
	if l.dir == "" {
		l.dir = DefaultDir
	}

	if l.env == nil {
		l.env = environ()
	}

	for _, lf := range leaves(cfg) {
		if !isFileMetaKey(lf.key) {
			l.leaves = append(l.leaves, lf)
			l.origins[lf.key] = Origin{kind: OriginDefault}
		}
	}

	files := l.files()
	for _, f := range files {
		l.decodeFile(f, cfg)
	}

	if len(l.problems) == 0 {
		l.applyEnv(cfg)
	}

	if len(l.problems) == 0 {
		l.resolveSecrets()
	}

	if len(l.problems) == 0 {
		l.problems = append(l.problems, cfg.validate(l.origins)...)
	}

	meta := Meta{Origins: l.origins, Warnings: l.warnings}
	for _, f := range files {
		meta.Files = append(meta.Files, l.rel(f))
	}

	if len(l.problems) > 0 {
		return meta, &Error{Role: role, Problems: l.problems}
	}

	return meta, nil
}

func environ() map[string]string {
	m := map[string]string{}

	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}

	return m
}

func (l *loader) rel(path string) string {
	if r, err := filepath.Rel(l.dir, path); err == nil {
		return r
	}

	return path
}

// files returns <role>.toml followed by <role>.d/*.toml in lexical order.
func (l *loader) files() []string {
	base := filepath.Join(l.dir, string(l.role)+".toml")
	if _, err := os.Stat(base); err != nil {
		l.fail(Problem{Origin: base, Code: CodeConfigMissing, Message: fmt.Sprintf("cannot read %s.toml: %v", l.role, err)})

		return nil
	}

	dropins, err := filepath.Glob(filepath.Join(l.dir, string(l.role)+".d", "*.toml"))
	if err != nil {
		l.fail(Problem{Code: CodeConfigUnreadable, Message: err.Error()})
	}

	sort.Strings(dropins)

	files := []string{base}

	for _, f := range dropins {
		if info, err := os.Stat(f); err == nil && info.Mode().IsRegular() {
			files = append(files, f)
		}
	}

	return files
}

// isFileMetaKey reports keys that describe a file rather than configure the
// process: they have no env var and no origin.
func isFileMetaKey(key string) bool {
	return key == "schema_version" || key == "allow_inline_secrets"
}

type fileHeader struct {
	SchemaVersion      *int64 `toml:"schema_version"`
	AllowInlineSecrets bool   `toml:"allow_inline_secrets"`
}

func (l *loader) decodeFile(path string, cfg any) {
	name := l.rel(path)

	data, err := os.ReadFile(path) //nolint:gosec // path is under the operator's config dir
	if err != nil {
		l.fail(Problem{Origin: name, Code: CodeConfigUnreadable, Message: err.Error()})

		return
	}

	var h fileHeader
	if _, err := toml.Decode(string(data), &h); err != nil {
		l.fail(parseProblem(name, err))

		return
	}

	switch {
	case h.SchemaVersion == nil:
		l.fail(Problem{Origin: name, Key: "schema_version", Code: CodeSchemaVersionMissing,
			Message: fmt.Sprintf("schema_version is required (this binary supports %d)", SchemaVersion)})

		return
	case *h.SchemaVersion != SchemaVersion:
		l.fail(Problem{Origin: name, Key: "schema_version", Code: CodeSchemaVersionUnsupported,
			Message: fmt.Sprintf("schema_version %d is not supported (this binary supports %d)", *h.SchemaVersion, SchemaVersion)})

		return
	}

	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		l.fail(parseProblem(name, err))

		return
	}

	lines := keyLines(string(data))
	origin := func(key string) Origin { return Origin{kind: OriginFile, file: name, line: lines[key]} }

	undecoded := map[string]bool{}
	for _, k := range md.Undecoded() {
		undecoded[strings.Join(k, ".")] = true
	}

	for _, k := range md.Undecoded() {
		key := strings.Join(k, ".")
		if parentUndecoded(k, undecoded) {
			continue
		}

		l.fail(Problem{Origin: origin(key).String(), Key: key, Code: CodeUnknownKey,
			Message: "unknown key" + suggest(key, l.keys())})
	}

	for _, lf := range l.leaves {
		if !md.IsDefined(strings.Split(lf.key, ".")...) {
			continue
		}

		l.origins[lf.key] = origin(lf.key)

		if s, ok := lf.value.Addr().Interface().(*Secret); ok && s.source == secretInline {
			if !h.AllowInlineSecrets {
				l.fail(Problem{Origin: origin(lf.key).String(), Key: lf.key, Code: CodeInlineSecretForbidden,
					Message: `inline secret value; use { file = "…" } or { env = "…" }, or set allow_inline_secrets = true in this file`})
			} else {
				l.warnings = append(l.warnings, fmt.Sprintf("%s: %s: inline secret value (allow_inline_secrets = true)", origin(lf.key), lf.key))
			}
		}
	}
}

// parentUndecoded reports whether a parent table of k is itself unknown.
func parentUndecoded(k toml.Key, undecoded map[string]bool) bool {
	for i := 1; i < len(k); i++ {
		if undecoded[strings.Join(k[:i], ".")] {
			return true
		}
	}

	return false
}

func parseProblem(name string, err error) Problem {
	var perr toml.ParseError
	if errors.As(err, &perr) {
		return Problem{
			Origin:  fmt.Sprintf("%s:%d:%d", name, perr.Position.Line, perr.Position.Col),
			Key:     perr.LastKey,
			Code:    CodeParse,
			Message: perr.Message,
		}
	}

	return Problem{Origin: name, Code: CodeParse, Message: err.Error()}
}

func (l *loader) keys() []string {
	keys := make([]string, len(l.leaves))
	for i, lf := range l.leaves {
		keys[i] = lf.key
	}

	return keys
}

// knownEnvVars lists the env var names of every role, so that a variable for
// another role (MESHSDR_NODE__ID in a hub container) is not an error.
func knownEnvVars() []string {
	var names []string

	for _, v := range []any{&Hub{}, &Node{}} {
		for _, lf := range leaves(v) {
			if lf.env != "" {
				names = append(names, EnvPrefix+lf.env)
			}
		}
	}

	return append(names, EnvConfigDir)
}

func (l *loader) applyEnv(cfg any) {
	known := knownEnvVars()

	for name := range l.env {
		if strings.HasPrefix(name, EnvPrefix) && !slices.Contains(known, name) {
			l.fail(Problem{Origin: "env:" + name, Code: CodeUnknownEnv, Message: "unknown config env var" + suggest(name, known)})
		}
	}

	envToKey := map[string]string{}

	for _, lf := range l.leaves {
		if lf.env != "" {
			envToKey[EnvPrefix+lf.env] = lf.key
		}
	}

	err := env.ParseWithOptions(cfg, env.Options{
		Prefix:      EnvPrefix,
		Environment: l.env,
		// OnSet fires for every field, set or not: keep only the variables
		// present and non-empty (caarlos0/env leaves the field untouched for
		// an empty value, so an empty variable is treated as unset).
		OnSet: func(tag string, _ any, isDefault bool) {
			if v := l.env[tag]; v == "" || isDefault {
				return
			}

			if key, ok := envToKey[tag]; ok {
				l.origins[key] = Origin{kind: OriginEnv, env: tag}
			}
		},
	})
	if err != nil {
		l.fail(Problem{Code: CodeInvalidValue, Message: err.Error()})
	}
}

func (l *loader) resolveSecrets() {
	lookup := func(name string) (string, bool) {
		v, ok := l.env[name]

		return v, ok
	}

	for _, lf := range l.leaves {
		s, ok := lf.value.Addr().Interface().(*Secret)
		if !ok || !s.IsSet() {
			continue
		}

		if err := s.resolve(lookup); err != nil {
			code := CodeSecretUnresolved
			if errors.Is(err, errInsecureSecretFile) {
				code = CodeInsecureSecretFile
			}

			l.fail(Problem{Origin: l.origins.Of(lf.key).String(), Key: lf.key, Code: code, Message: err.Error()})
		}
	}
}

// suggest returns a "did you mean" hint for the closest candidate, if close.
func suggest(s string, candidates []string) string {
	best, bestDist := "", -1

	for _, c := range candidates {
		if d := levenshtein(strings.ToLower(s), strings.ToLower(c)); bestDist < 0 || d < bestDist {
			best, bestDist = c, d
		}
	}

	if bestDist < 0 || bestDist > max(2, len(s)/4) {
		return ""
	}

	return fmt.Sprintf(" (did you mean %q?)", best)
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)

	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		cur[0] = i

		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}

			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}

		prev, cur = cur, prev
	}

	return prev[len(b)]
}
