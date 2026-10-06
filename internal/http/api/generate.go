package api

// openapi.yaml is the source of truth: validate it and convert it to the
// embedded openapi.json, then generate the chi strict server (api.gen.go).
// Both outputs are generated, never committed.

//go:generate go run ./specgen openapi.yaml openapi.json
//go:generate go tool oapi-codegen -config oapi-codegen.yaml openapi.yaml
