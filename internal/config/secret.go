package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/invopop/jsonschema"
)

type secretSource int

const (
	secretUnset secretSource = iota
	secretInline
	secretFile
	secretEnv
)

// Secret is a config value that must not leak (TECHNICAL_SPEC §7.4 "Secrets
// handling"). In a file it is a reference, { file = "/path" } or
// { env = "VAR" }; an inline string is accepted only when the same file sets
// allow_inline_secrets = true. Through a MESHSDR_* env var it is a literal.
//
// The value is resolved at load and never printed: String, GoString,
// LogValue and MarshalText return a redacted placeholder.
type Secret struct {
	source secretSource
	ref    string // file path or env var name
	value  string // resolved value
}

// IsSet reports whether the secret was given.
func (s Secret) IsSet() bool { return s.source != secretUnset }

// Reveal returns the resolved value. Pass it only to the code that needs it.
func (s Secret) Reveal() string { return s.value }

// String returns a redacted placeholder.
func (s Secret) String() string {
	if !s.IsSet() {
		return ""
	}

	return "[redacted]"
}

// GoString implements fmt.GoStringer (%#v) with a redacted placeholder.
func (s Secret) GoString() string { return "config.Secret(" + strconv.Quote(s.String()) + ")" }

// LogValue implements slog.LogValuer with a redacted placeholder.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// MarshalText implements encoding.TextMarshaler with a redacted placeholder.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler: a literal value (env vars).
func (s *Secret) UnmarshalText(text []byte) error {
	*s = Secret{source: secretInline, value: string(text)}

	return nil
}

// UnmarshalTOML implements toml.Unmarshaler: a literal string or a
// { file = "…" } / { env = "…" } reference.
func (s *Secret) UnmarshalTOML(data any) error {
	switch v := data.(type) {
	case string:
		*s = Secret{source: secretInline, value: v}

		return nil
	case map[string]any:
		if len(v) != 1 {
			return errors.New(`secret reference must have exactly one of "file" or "env"`)
		}

		for k, raw := range v {
			ref, ok := raw.(string)
			if !ok || ref == "" {
				return fmt.Errorf("secret reference %q must be a non-empty string", k)
			}

			switch k {
			case "file":
				*s = Secret{source: secretFile, ref: ref}
			case "env":
				*s = Secret{source: secretEnv, ref: ref}
			default:
				return fmt.Errorf(`unknown secret reference %q (want "file" or "env")`, k)
			}
		}

		return nil
	default:
		return errors.New(`secret must be { file = "…" }, { env = "…" } or an inline string`)
	}
}

// resolve reads the referenced value. A relative file path is resolved
// against configDir; lookupEnv reads the process environment.
func (s *Secret) resolve(configDir string, lookupEnv func(string) (string, bool)) error {
	switch s.source {
	case secretFile:
		path := s.ref
		if !filepath.IsAbs(path) {
			path = filepath.Join(configDir, path)
		}

		v, err := readSecretFile(path)
		if err != nil {
			return err
		}

		s.value = v
	case secretEnv:
		v, ok := lookupEnv(s.ref)
		if !ok || v == "" {
			return fmt.Errorf("secret env var %s is unset or empty", s.ref)
		}

		s.value = v
	case secretInline:
		if s.value == "" {
			return errors.New("secret is empty")
		}
	case secretUnset:
	}

	return nil
}

// errInsecureSecretFile is returned for secret files writable by group/others
// or readable by others.
var errInsecureSecretFile = errors.New("insecure_secret_file")

// readSecretFile opens path once, checks the opened file's mode and reads
// from the same handle, so the checked file is the read file.
func readSecretFile(path string) (_ string, err error) {
	f, err := os.Open(path) //nolint:gosec // path comes from the operator's config
	if err != nil {
		return "", fmt.Errorf("secret file: %w", err)
	}

	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("secret file: %w", cerr)
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("secret file: %w", err)
	}

	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("secret file %s is not a regular file", path)
	}

	if perm := info.Mode().Perm(); perm&0o022 != 0 || perm&0o004 != 0 {
		return "", fmt.Errorf("%w: %s has mode %04o, must not be group/world-writable nor world-readable", errInsecureSecretFile, path, perm)
	}

	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("secret file: %w", err)
	}

	v := strings.TrimRight(string(b), "\r\n")
	if v == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}

	return v, nil
}

// JSONSchema describes the type in the generated config schema.
func (Secret) JSONSchema() *jsonschema.Schema {
	ref := func(name string) *jsonschema.Schema {
		return &jsonschema.Schema{
			Type:                 "object",
			PatternProperties:    map[string]*jsonschema.Schema{"^" + name + "$": {Type: "string", MinLength: ptr(uint64(1))}},
			Required:             []string{name},
			AdditionalProperties: jsonschema.FalseSchema,
		}
	}

	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			ref("file"),
			ref("env"),
			{Type: "string", Description: "inline value, only with allow_inline_secrets = true in the same file"},
		},
		Extras: map[string]any{"secret": true},
	}
}

func ptr[T any](v T) *T { return &v }
