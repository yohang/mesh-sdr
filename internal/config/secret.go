package config

import (
	"errors"
	"fmt"
	"os"
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
// The value is resolved at load and never printed: String and LogValue
// return a redacted placeholder.
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

// resolve reads the referenced value. lookupEnv reads the process environment.
func (s *Secret) resolve(lookupEnv func(string) (string, bool)) error {
	switch s.source {
	case secretFile:
		v, err := readSecretFile(s.ref)
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

func readSecretFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("secret file: %w", err)
	}

	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("secret file %s is not a regular file", path)
	}

	if perm := info.Mode().Perm(); perm&0o022 != 0 || perm&0o004 != 0 {
		return "", fmt.Errorf("%w: %s has mode %04o, must not be group/world-writable nor world-readable", errInsecureSecretFile, path, perm)
	}

	b, err := os.ReadFile(path) //nolint:gosec // path comes from the operator's config
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
