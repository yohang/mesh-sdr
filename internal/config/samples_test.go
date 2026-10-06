package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The committed sample and dev configs must load.
func TestCommittedConfigs(t *testing.T) {
	tests := []struct {
		role Role
		src  string
	}{
		{RoleHub, "../../.infra/config/hub.toml.example"},
		{RoleNode, "../../.infra/config/node.toml.example"},
		{RoleHub, "../../.infra/docker/dev/config/hub.toml"},
		{RoleNode, "../../.infra/docker/dev/config/node.toml"},
	}

	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			b, err := os.ReadFile(tt.src)
			if err != nil {
				t.Fatal(err)
			}

			dir := writeFiles(t, map[string]string{string(tt.role) + ".toml": string(b)})

			if tt.role == RoleHub {
				_, _, err = LoadHub(Options{Dir: dir, Env: map[string]string{}})
			} else {
				_, _, err = LoadNode(Options{Dir: dir, Env: map[string]string{}})
			}

			if err != nil {
				t.Fatal(err)
			}
		})
	}

	// The image's minimal configs load once the required keys come from env.
	prod := "../../.infra/docker/prod/etc/meshsdr"

	if _, _, err := LoadHub(Options{Dir: prod, Env: map[string]string{"MESHSDR_HUB__URL": "https://sdr.example.org"}}); err != nil {
		t.Errorf("prod hub: %v", err)
	}

	if _, _, err := LoadNode(Options{Dir: filepath.Clean(prod), Env: map[string]string{"MESHSDR_NODE__ID": "attic"}}); err != nil {
		t.Errorf("prod node: %v", err)
	}
}
