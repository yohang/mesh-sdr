package http

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/config"
)

// Every settings key is editable on exactly one admin page section.
func TestEverySettingHasASection(t *testing.T) {
	cat, err := config.NewSettingsCatalog(config.DefaultHub(), config.Origins{})
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]string{}

	for _, p := range formPages {
		for _, s := range p.Forms {
			for _, k := range s.Keys {
				if prev, dup := seen[k]; dup {
					t.Errorf("%s in %s and %s/%s", k, prev, p.Path, s.ID)
				}

				seen[k] = p.Path + "/" + s.ID
			}
		}
	}

	for _, d := range cat.Definitions() {
		if _, ok := seen[d.Key()]; !ok {
			t.Errorf("setting %s is on no admin page", d.Key())
		}

		delete(seen, d.Key())
	}

	for k := range seen {
		t.Errorf("admin page section lists unknown key %s", k)
	}
}
