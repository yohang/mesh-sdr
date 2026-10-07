package api

import "testing"

func TestLoadBodyFields(t *testing.T) {
	f, err := loadBodyFields(specJSON)
	if err != nil {
		t.Fatal(err)
	}

	token := f["POST /auth/token"]
	if a := token.allowed; !a["node_id"] || !a["cid"] || len(a) != 2 || token.operation != "MintAccessToken" {
		t.Errorf("token fields = %+v", token)
	}

	if req := token.required; len(req) != 2 || req[0] != "cid" || req[1] != "node_id" {
		t.Errorf("token required fields = %v", req)
	}

	// Every JSON request body of the API is closed (SR-20).
	if _, err := loadBodyFields([]byte(`{"paths":{"/x":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Nope"}}}}}}}}`)); err == nil {
		t.Error("unknown schema accepted")
	}
}
