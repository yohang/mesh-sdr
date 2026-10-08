package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell"
)

func TestSchema(t *testing.T) {
	tests := []struct {
		role     Role
		required []string
		leaf     []string // path to a leaf property
		def      any
		lockable bool
	}{
		{RoleHub, []string{"schema_version"}, []string{"gateway", "https_listen"}, ":443", false},
		{RoleHub, []string{"schema_version"}, []string{"db", "dsn"}, "sqlite:///var/lib/meshsdr/hub.db", false},
		{RoleHub, []string{"schema_version"}, []string{"settings", "ui", "theme_mode"}, "auto", true},
		{RoleNode, []string{"schema_version"}, []string{"node", "listen"}, "0.0.0.0:8074", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.role)+"/"+tt.leaf[1], func(t *testing.T) {
			b, err := Schema(tt.role)
			if err != nil {
				t.Fatal(err)
			}

			var s map[string]any
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatal(err)
			}

			if s["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
				t.Errorf("$schema = %v", s["$schema"])
			}

			if s["additionalProperties"] != false {
				t.Errorf("additionalProperties = %v", s["additionalProperties"])
			}

			req, _ := s["required"].([]any)
			for _, r := range tt.required {
				found := false
				for _, v := range req {
					found = found || v == r
				}

				if !found {
					t.Errorf("required %v misses %s", req, r)
				}
			}

			node := s
			for _, p := range tt.leaf {
				props, _ := node["properties"].(map[string]any)
				node, _ = props[p].(map[string]any)
				if node == nil {
					t.Fatalf("no property %v", tt.leaf)
				}
			}

			if node["x-scope"] != "global" || node["lockable"] != tt.lockable {
				t.Errorf("annotations = x-scope %v, lockable %v", node["x-scope"], node["lockable"])
			}

			if node["default"] != tt.def {
				t.Errorf("default = %v, want %v", node["default"], tt.def)
			}

			if node["description"] == "" || node["description"] == nil {
				t.Error("missing description")
			}
		})
	}

	if _, err := Schema("gateway"); err == nil {
		t.Error("unknown role accepted")
	}
}

// TestSchemaDescriptionsUnescaped catches a tag escape left in a
// description: commas are escaped (`\,`) in the jsonschema tag only, never
// in jsonschema_description, which is used verbatim.
func TestSchemaDescriptionsUnescaped(t *testing.T) {
	for _, role := range []Role{RoleHub, RoleNode} {
		b, err := Schema(role)
		if err != nil {
			t.Fatal(err)
		}

		var s any
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}

		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch v := v.(type) {
			case map[string]any:
				for k, c := range v {
					if d, ok := c.(string); ok && k == "description" && strings.Contains(d, `\`) {
						t.Errorf("%s %s description = %q", role, path, d)
					}

					walk(path+"/"+k, c)
				}
			case []any:
				for _, c := range v {
					walk(path, c)
				}
			}
		}
		walk("", s)
	}
}

func TestSecretSchemaIsMarked(t *testing.T) {
	b, err := json.Marshal(Secret{}.JSONSchema())
	if err != nil {
		t.Fatal(err)
	}

	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}

	if s["secret"] != true {
		t.Errorf("secret = %v", s["secret"])
	}
}

// The schema limit of the usage policy is the shell's value object limit.
func TestUsagePolicySchemaLimit(t *testing.T) {
	b, err := Schema(RoleHub)
	if err != nil {
		t.Fatal(err)
	}

	var s struct {
		Properties struct {
			Settings struct {
				Properties struct {
					Receiver struct {
						Properties struct {
							Text struct {
								MaxLength int `json:"maxLength"`
							} `json:"usage_policy_text"`
						} `json:"properties"`
					} `json:"receiver"`
				} `json:"properties"`
			} `json:"settings"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}

	if got := s.Properties.Settings.Properties.Receiver.Properties.Text.MaxLength; got != shell.MaxPolicyTextLength {
		t.Errorf("schema maxLength = %d, want %d", got, shell.MaxPolicyTextLength)
	}
}
