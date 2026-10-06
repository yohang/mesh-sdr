package api

import "testing"

func TestLoadBodyFields(t *testing.T) {
	f, err := loadBodyFields(specJSON)
	if err != nil {
		t.Fatal(err)
	}

	login := f["POST /auth/login"]
	if a := login.allowed; !a["login"] || !a["password"] || !a["remember_me"] || len(a) != 3 || login.operation != "Login" {
		t.Errorf("login fields = %+v", login)
	}

	if pw := f["POST /auth/password"].allowed; !pw["current_password"] || !pw["new_password"] || len(pw) != 2 {
		t.Errorf("password fields = %v", pw)
	}

	// Every JSON request body of the API is closed (SR-20).
	if _, err := loadBodyFields([]byte(`{"paths":{"/x":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Nope"}}}}}}}}`)); err == nil {
		t.Error("unknown schema accepted")
	}
}
