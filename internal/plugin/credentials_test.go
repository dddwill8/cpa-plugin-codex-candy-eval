package plugin

import (
	"encoding/json"
	"testing"
)

func TestCredentialInventoryAndSync(t *testing.T) {
	setupTest(t)
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			t.Fatalf("unexpected call %s", method)
		}
		return json.RawMessage(`{"files":[
			{"id":"old.json","provider":"codex","source":"file","priority":99},
			{"id":"cooling.json","provider":"codex","source":"file","status":"error","status_message":"quota exhausted","unavailable":true,"next_retry_after":"2999-01-01T00:00:00Z"},
			{"id":"recovered.json","provider":"codex","source":"file","unavailable":true,"next_retry_after":"2000-01-01T00:00:00Z"},
			{"id":"claude.json","type":"claude","source":"file","disabled":true},
			{"id":"runtime","provider":"gemini","source":"memory","label":"secret-key","email":"secret-key","account_type":"api_key","account":"synthetic-secret-key","priority":-3},
			{"id":"configured","provider":"codex","runtime_only":true,"disabled":true,"unavailable":true},
			{"provider":"codex","name":"missing-id"}]}`), nil
	}
	response := syncCredentialsResponse([]byte(`{"credentials":[{"id":"configured","provider":"codex","name":"Configured Codex","base_url":" https://example.test/v1 ","provider_name":" Codex Group ","disabled":true,"email":"must-not-sync","priority":7},{"id":"compat","provider":"openai-compatible-demo","name":"Configured Demo","priority":9}]}`))
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	all, err := credentials()
	if err != nil || len(all) != 7 {
		t.Fatalf("credentials = %+v, %v", all, err)
	}
	for _, auth := range all {
		if want := map[string]int{"old.json": 99, "runtime": -3, "configured": 7, "compat": 9}[auth.ID]; auth.Priority != want {
			t.Fatalf("priority for %s = %d, want %d", auth.ID, auth.Priority, want)
		}
		if auth.ID == "configured" && (!auth.Disabled || auth.Unavailable || auth.Source != "ai_providers" || auth.Email != "" || auth.BaseURL != "https://example.test/v1" || auth.ProviderName != "Codex Group") {
			t.Fatalf("config merge: %+v", auth)
		}
		if auth.ID == "runtime" && (auth.Name != "synthe…-key" || auth.Email != "") {
			t.Fatalf("unsafe runtime display: %+v", auth)
		}
		if auth.ID == "cooling.json" && (!auth.Unavailable || auth.StatusMessage != "quota exhausted" || auth.NextRetryAfter.IsZero()) || auth.ID == "recovered.json" && auth.Unavailable {
			t.Fatalf("unavailable state: %+v", auth)
		}
	}
	selected, err := selectedCredentials(nil, true)
	if err != nil || len(selected) != 4 {
		t.Fatalf("selected = %+v, %v", selected, err)
	}
	selected, _ = selectedCredentials([]string{"compat", "configured", "cooling.json"}, false)
	if len(selected) != 1 || selected[0].Provider != "openai-compatible-demo" {
		t.Fatalf("selection = %+v", selected)
	}
	for _, body := range []string{`{}`, `{"credentials":[{"id":"a","provider":"codex"},{"id":"a","provider":"codex"}]}`} {
		if res := syncCredentialsResponse([]byte(body)); res.StatusCode != 400 {
			t.Fatal("invalid sync accepted")
		}
	}
	if len(configuredCredentials) != 2 {
		t.Fatal("invalid sync replaced inventory")
	}
	syncCredentialsResponse([]byte(`{"credentials":[{"id":"configured","provider":"codex"},{"id":"old.json","provider":"codex","disabled":true}]}`))
	all, _ = credentials()
	for _, auth := range all {
		if auth.ID == "configured" && !auth.Disabled {
			t.Fatal("sync re-enabled a disabled host credential")
		}
		if auth.ID == "old.json" && (auth.Source != credentialSourceFile || auth.Disabled || auth.Priority != 99) {
			t.Fatal("sync overwrote an authentication file")
		}
	}
	syncCredentialsResponse([]byte(`{"credentials":[]}`))
	if len(configuredCredentials) != 0 {
		t.Fatal("removed configured credentials remained selectable")
	}
}

func TestCredentialListResponse(t *testing.T) {
	setupTest(t)
	for _, body := range []string{`null`, `{}`, `{"files":null}`, `{"files":{}}`, `{"files":[]}`} {
		hostCall = func(string, any) (json.RawMessage, error) { return json.RawMessage(body), nil }
		response := stateResponse()
		want := 502
		if body == `{"files":[]}` {
			want = 200
		}
		if response.StatusCode != want {
			t.Fatalf("body=%s status=%d, want %d", body, response.StatusCode, want)
		}
	}
}

func TestCredentialModelCatalog(t *testing.T) {
	for _, tc := range []struct {
		model            string
		catalog          []string
		known, supported bool
	}{
		{"alias", nil, false, false},
		{"alias", []string{}, true, false},
		{"alias", []string{"upstream"}, true, false},
		{"prefix/alias", []string{"alias"}, true, false},
		{"prefix/alias", []string{"prefix/alias"}, true, true},
		{"alias(high)", []string{"alias"}, true, true},
		{"alias(high)", []string{"alias(high)"}, true, true},
	} {
		known, supported := credentialSupportsModel(map[string][]string{"a": tc.catalog}, "a", tc.model)
		if known != tc.known || supported != tc.supported {
			t.Errorf("%+v: %v %v", tc, known, supported)
		}
	}
}
