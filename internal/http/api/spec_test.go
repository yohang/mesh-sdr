package api

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Every operation of the API has an access level (SR-15: no unmapped route).
func TestSpecCoversEveryOperation(t *testing.T) {
	s, err := parseSpec(Spec())
	if err != nil {
		t.Fatal(err)
	}

	roles := map[string]domain.Role{}
	for _, op := range s.operations {
		roles[op.name] = op.role
	}

	ops := reflect.TypeFor[StrictServerInterface]()
	for i := range ops.NumMethod() {
		if _, ok := roles[ops.Method(i).Name]; !ok {
			t.Errorf("operation %s has no %s", ops.Method(i).Name, AccessExtension)
		}
	}

	want := map[string]domain.Role{"GetSession": domain.RoleAnonymous, "MintAccessToken": domain.RoleAnonymous, "GetEffectiveConfig": domain.RoleAdmin}
	for op, role := range want {
		if roles[op] != role {
			t.Errorf("%s requires %v, want %v", op, roles[op], role)
		}
	}
}

// A document with an operation without a valid access level fails the
// handler at startup: the API fails closed.
func TestSpecFailsClosed(t *testing.T) {
	for name, doc := range map[string]string{
		"missing":        `{"paths":{"/x":{"get":{"operationId":"getX"}}}}`,
		"unknown":        `{"paths":{"/x":{"get":{"operationId":"getX","x-meshsdr-access":"root"}}}}`,
		"no id":          `{"paths":{"/x":{"post":{"x-meshsdr-access":"admin"}}}}`,
		"not openapi":    `[`,
		"unknown schema": `{"paths":{"/x":{"post":{"operationId":"postX","x-meshsdr-access":"admin","requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Nope"}}}}}}}}`,
	} {
		if _, err := newHandler([]byte(doc), Server{}, authzFunc(nil), 1<<20, slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	s, err := parseSpec([]byte(`{"paths":{"/x":{"parameters":[],"get":{"operationId":"getX","x-meshsdr-access":"operator"}}}}`))
	if err != nil || len(s.operations) != 1 || s.operations[0].name != "GetX" || s.operations[0].role != domain.RoleOperator {
		t.Errorf("spec = %+v, %v", s, err)
	}
}

func TestSpecBodyFields(t *testing.T) {
	s, err := parseSpec(specJSON)
	if err != nil {
		t.Fatal(err)
	}

	token := s.bodies["POST /auth/token"]
	if a := token.allowed; !a["node_id"] || !a["cid"] || len(a) != 2 {
		t.Errorf("token fields = %+v", token)
	}

	if req := token.required; len(req) != 2 || req[0] != "cid" || req[1] != "node_id" {
		t.Errorf("token required fields = %v", req)
	}
}
