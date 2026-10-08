package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Every operation of the API has an access level (SR-15: no unmapped route).
func TestPolicyCoversEveryOperation(t *testing.T) {
	p, err := LoadPolicy(Spec())
	if err != nil {
		t.Fatal(err)
	}

	ops := reflect.TypeFor[StrictServerInterface]()
	for i := range ops.NumMethod() {
		if _, ok := p[ops.Method(i).Name]; !ok {
			t.Errorf("operation %s has no %s", ops.Method(i).Name, AccessExtension)
		}
	}

	want := map[string]domain.Role{"GetSession": domain.RoleAnonymous, "MintAccessToken": domain.RoleAnonymous, "GetEffectiveConfig": domain.RoleAdmin}
	for op, role := range want {
		if p[op] != role {
			t.Errorf("%s requires %v, want %v", op, p[op], role)
		}
	}
}

func TestLoadPolicyFailsClosed(t *testing.T) {
	for name, doc := range map[string]string{
		"missing":     `{"paths":{"/x":{"get":{"operationId":"getX"}}}}`,
		"unknown":     `{"paths":{"/x":{"get":{"operationId":"getX","x-meshsdr-access":"root"}}}}`,
		"no id":       `{"paths":{"/x":{"post":{"x-meshsdr-access":"admin"}}}}`,
		"not openapi": `[`,
	} {
		if _, err := LoadPolicy([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	p, err := LoadPolicy([]byte(`{"paths":{"/x":{"parameters":[],"get":{"operationId":"getX","x-meshsdr-access":"operator"}}}}`))
	if err != nil || p["GetX"] != domain.RoleOperator {
		t.Errorf("policy = %v, %v", p, err)
	}
}

type authz struct {
	got domain.Role
	err error
}

func (a *authz) Authorize(_ context.Context, r domain.Role) error {
	a.got = r

	return a.err
}

func TestPolicyMiddleware(t *testing.T) {
	p := Policy{"GetX": domain.RoleAdmin}
	called := false
	next := func(context.Context, http.ResponseWriter, *http.Request, any) (any, error) {
		called = true

		return "ok", nil
	}

	run := func(op string, a *authz) (any, error) {
		called = false
		h := p.Middleware(a)(next, op)

		return h(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), nil)
	}

	a := &authz{}
	if res, err := run("GetX", a); err != nil || res != "ok" || !called || a.got != domain.RoleAdmin {
		t.Errorf("allowed: %v %v %v %v", res, err, called, a.got)
	}

	if _, err := run("GetX", &authz{err: domain.ErrForbidden}); !errors.Is(err, domain.ErrForbidden) || called {
		t.Errorf("denied: %v, handler called %v", err, called)
	}

	if _, err := run("Unknown", &authz{}); !errors.Is(err, domain.ErrForbidden) || called {
		t.Errorf("unmapped operation: %v, handler called %v", err, called)
	}
}
