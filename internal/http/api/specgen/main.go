// Command specgen validates an OpenAPI YAML document and writes it as JSON.
// It runs at go generate time only (kin-openapi is not linked into meshsdr).
//
// Usage: go run ./specgen <in.yaml> <out.json>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/getkin/kin-openapi/openapi3"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: specgen <in.yaml> <out.json>")
		os.Exit(2)
	}

	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "specgen:", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	loader := openapi3.NewLoader()

	doc, err := loader.LoadFromFile(in)
	if err != nil {
		return fmt.Errorf("load %s: %w", in, err)
	}

	if err := doc.Validate(context.Background()); err != nil {
		return fmt.Errorf("validate %s: %w", in, err)
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil { //nolint:gosec // generated source file
		return fmt.Errorf("write %s: %w", out, err)
	}

	return nil
}
