package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/secrypto"
	"github.com/tinyship/tinyship/internal/store"
)

// sealedTestServer points the API at a fresh sealing key pair and
// returns the private key path so tests can open what was sealed.
func sealedTestServer(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	keyDir := t.TempDir()
	keyPath, pubPath, err := secrypto.WriteKeyPair(keyDir)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	t.Setenv("TINYSHIP_SECRETS_PUB", pubPath)
	srv, st := testServer(t, t.TempDir())
	return srv, st, keyPath
}

func doReq(t *testing.T, method, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp, decodeBody(t, resp)
}

func TestEnvSetListUnset(t *testing.T) {
	srv, _, _ := sealedTestServer(t)
	createApp(t, srv, testConfig).Body.Close()

	resp, body := doReq(t, http.MethodPut, srv.URL+"/apps/web/env/GREETING",
		`{"value":"hello","kind":"plain"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set plain status = %d body=%v", resp.StatusCode, body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "next deploy") {
		t.Errorf("message = %q, want next-deploy notice", msg)
	}

	resp, body = doReq(t, http.MethodPut, srv.URL+"/apps/web/env/TOKEN",
		`{"value":"sup3r-secret","kind":"secret"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set secret status = %d body=%v", resp.StatusCode, body)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "sup3r-secret") {
		t.Errorf("secret value leaked in set response: %s", raw)
	}

	resp, body = doReq(t, http.MethodGet, srv.URL+"/apps/web/env", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "sup3r-secret") {
		t.Errorf("secret value leaked in list response: %s", raw)
	}
	var list struct {
		Env []envDTO `json:"env"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Env) != 2 {
		t.Fatalf("env count = %d, want 2: %s", len(list.Env), raw)
	}
	for _, e := range list.Env {
		switch e.Key {
		case "GREETING":
			if e.Value == nil || *e.Value != "hello" {
				t.Errorf("GREETING value = %v, want hello", e.Value)
			}
		case "TOKEN":
			if e.Kind != "secret" {
				t.Errorf("TOKEN kind = %q, want secret", e.Kind)
			}
			if e.Value != nil {
				t.Errorf("TOKEN value present: %q, want omitted", *e.Value)
			}
		default:
			t.Errorf("unexpected key %q", e.Key)
		}
	}

	resp, body = doReq(t, http.MethodDelete, srv.URL+"/apps/web/env/TOKEN", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unset status = %d body=%v", resp.StatusCode, body)
	}
	resp, _ = doReq(t, http.MethodDelete, srv.URL+"/apps/web/env/TOKEN", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second unset status = %d, want 404", resp.StatusCode)
	}
}

func TestEnvSecretSealedInTheStore(t *testing.T) {
	srv, st, keyPath := sealedTestServer(t)
	createApp(t, srv, testConfig).Body.Close()

	const secret = "ghcrp_deadbeef"
	resp, _ := doReq(t, http.MethodPut, srv.URL+"/apps/web/env/TOKEN",
		`{"value":"`+secret+`","kind":"secret"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set secret status = %d", resp.StatusCode)
	}

	// Read the row exactly as the worker will: sealed bytes straight
	// from the store, then opened with the private key on disk.
	app, err := st.GetAppByName(t.Context(), "web")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	vars, err := st.ListAppEnv(t.Context(), app.ID)
	if err != nil {
		t.Fatalf("list env: %v", err)
	}
	var sealed []byte
	for _, v := range vars {
		if v.Key == "TOKEN" {
			sealed = v.ValueSealed
			if v.ValuePlain != "" {
				t.Errorf("secret row carries a clear value: %q", v.ValuePlain)
			}
		}
	}
	if len(sealed) == 0 {
		t.Fatal("no sealed payload stored")
	}
	if strings.Contains(string(sealed), secret) {
		t.Fatal("stored payload contains the clear secret")
	}
	priv, err := secrypto.LoadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("load priv: %v", err)
	}
	got, err := secrypto.Open(priv, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got) != secret {
		t.Errorf("opened %q, want %q", got, secret)
	}
}

func TestEnvSetValidation(t *testing.T) {
	srv, _, _ := sealedTestServer(t)
	createApp(t, srv, testConfig).Body.Close()

	cases := []struct {
		name       string
		url        string
		body       string
		wantStatus int
	}{
		{"reserved prefix", "/apps/web/env/TINYSHIP_PORT", `{"value":"1"}`, http.StatusBadRequest},
		{"bad key", "/apps/web/env/not-a-key!", `{"value":"1"}`, http.StatusBadRequest},
		{"unknown kind", "/apps/web/env/OK", `{"value":"1","kind":"encrypted"}`, http.StatusBadRequest},
		{"missing app", "/apps/ghost/env/OK", `{"value":"1"}`, http.StatusNotFound},
		{"bad json", "/apps/web/env/OK", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := doReq(t, http.MethodPut, srv.URL+tc.url, tc.body)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestEnvSetRejectsDuplicateCountOnlyForNewKeys(t *testing.T) {
	srv, _, _ := sealedTestServer(t)
	createApp(t, srv, testConfig).Body.Close()

	// Overwrite must not be blocked by the cap; only new keys are.
	if resp, _ := doReq(t, http.MethodPut, srv.URL+"/apps/web/env/A", `{"value":"1"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("first set status = %d", resp.StatusCode)
	}
	for i := 0; i < appconfig.MaxEnvVars-1; i++ {
		key := fmt.Sprintf("K%03d", i)
		resp, body := doReq(t, http.MethodPut, srv.URL+"/apps/web/env/"+key, `{"value":"1"}`)
		if resp.StatusCode == http.StatusConflict {
			t.Fatalf("hit the cap early at variable %d: %v", i, body)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("set %s status = %d body=%v", key, resp.StatusCode, body)
		}
	}
	// One more new key must now be refused, while overwriting A still works.
	resp, body := doReq(t, http.MethodPut, srv.URL+"/apps/web/env/EXTRA", `{"value":"1"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cap exceeded status = %d body=%v, want 409", resp.StatusCode, body)
	}
	resp, _ = doReq(t, http.MethodPut, srv.URL+"/apps/web/env/A", `{"value":"2"}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("overwrite at cap status = %d, want 200", resp.StatusCode)
	}
}

func TestEnvSecretRequiresKeyPair(t *testing.T) {
	// No key override: the default path does not exist in tests, so
	// sealing must fail with a key-shaped error and no value echo.
	srv, _ := testServer(t, t.TempDir())
	createApp(t, srv, testConfig).Body.Close()

	resp, body := doReq(t, http.MethodPut, srv.URL+"/apps/web/env/TOKEN",
		`{"value":"hunter2","kind":"secret"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%v, want 500", resp.StatusCode, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "E_SECRETS_KEY" {
		t.Errorf("code = %q, want E_SECRETS_KEY", code)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "hunter2") {
		t.Errorf("value leaked in error: %s", raw)
	}
}

func TestCredentialCreateListDelete(t *testing.T) {
	srv, _, _ := sealedTestServer(t)

	resp, body := doReq(t, http.MethodPost, srv.URL+"/credentials",
		`{"name":"ghcr-linh","registry":"ghcr.io","username":"linh","token":"tok_abc123"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d body=%v", resp.StatusCode, body)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "tok_abc123") {
		t.Errorf("token leaked in create response: %s", raw)
	}

	resp, body = doReq(t, http.MethodGet, srv.URL+"/credentials", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "tok_abc123") {
		t.Errorf("token leaked in list response: %s", raw)
	}
	if !strings.Contains(string(raw), "ghcr-linh") {
		t.Errorf("list missing credential: %s", raw)
	}

	resp, _ = doReq(t, http.MethodDelete, srv.URL+"/credentials/ghcr-linh", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("delete status = %d, want 200", resp.StatusCode)
	}
	resp, _ = doReq(t, http.MethodDelete, srv.URL+"/credentials/ghcr-linh", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", resp.StatusCode)
	}
}

func TestCredentialValidation(t *testing.T) {
	srv, _, _ := sealedTestServer(t)

	cases := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"bad name", `{"name":"Bad_Name","registry":"r","username":"u","token":"t"}`, http.StatusBadRequest},
		{"short name", `{"name":"ab","registry":"r","username":"u","token":"t"}`, http.StatusBadRequest},
		{"missing registry", `{"name":"ok-name","registry":"","username":"u","token":"t"}`, http.StatusBadRequest},
		{"missing username", `{"name":"ok-name","registry":"r","username":"","token":"t"}`, http.StatusBadRequest},
		{"missing token", `{"name":"ok-name","registry":"r","username":"u","token":""}`, http.StatusBadRequest},
		{"bad json", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := doReq(t, http.MethodPost, srv.URL+"/credentials", tc.body)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestCredentialDeleteInUse(t *testing.T) {
	srv, st, _ := sealedTestServer(t)
	createApp(t, srv, testConfig).Body.Close()

	resp, body := doReq(t, http.MethodPost, srv.URL+"/credentials",
		`{"name":"ghcr-linh","registry":"ghcr.io","username":"linh","token":"t"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d body=%v", resp.StatusCode, body)
	}
	cred, err := st.GetCredential(t.Context(), "ghcr-linh")
	if err != nil {
		t.Fatalf("get credential: %v", err)
	}
	if _, err := st.DB().Exec(
		`UPDATE apps SET registry_credential_id = ? WHERE name = 'web'`, cred.ID); err != nil {
		t.Fatalf("link app: %v", err)
	}
	resp, _ = doReq(t, http.MethodDelete, srv.URL+"/credentials/ghcr-linh", "")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete in use status = %d, want 409", resp.StatusCode)
	}
}
