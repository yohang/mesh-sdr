package process

import (
	"fmt"
	"path/filepath"
)

// Tools resolves the external programs of the node (ADR 0017 decision 10).
type Tools struct {
	// Paths are the tools.<name> absolute paths, by program name.
	Paths map[string]string
	// Dirs is tools.dirs: the search path, and the child's PATH.
	Dirs []string
}

// Resolve returns the absolute path of a tool: tools.<name> when set,
// otherwise the first executable of that name in Dirs.
func (t Tools) Resolve(name string) (string, error) {
	if p := t.Paths[name]; p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("tools.%s: %q is not absolute", name, p)
		}

		return filepath.Clean(p), nil
	}

	p, err := ResolveTool(name, t.Dirs)
	if err != nil {
		return "", fmt.Errorf("tool %s not found in tools.dirs: %w", name, err)
	}

	return p, nil
}
