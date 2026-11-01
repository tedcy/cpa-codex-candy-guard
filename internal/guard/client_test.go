package guard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientRejectsExternalOrMissingCredentials(t *testing.T) {
	for _, base := range []string{"https://example.com", "http://localhost:8317", "http://127.0.0.1@evil.test", "http://127.0.0.1:8317/path", "http://127.0.0.1?token=x"} {
		if _, err := NewHTTPClient(base, "key"); err == nil {
			t.Fatalf("accepted external base %s", base)
		}
	}
	if _, err := NewHTTPClient("http://127.0.0.1:8317", ""); err == nil {
		t.Fatal("accepted missing key")
	}
}

func TestClientDisableUsesIdentityAndOnlyStatusPatch(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{true: "changed", false: "matching"}[changed], func(t *testing.T) {
			patched := 0
			disabled := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("management authentication missing")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "PATCH" {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if len(body) != 3 || body["disabled"] != true || body["name"] != "one.json" || body["auth_index"] != "index" {
						t.Error("patch changed unrelated fields")
					}
					patched++
					disabled = true
					_, _ = w.Write([]byte(`{"status":"ok"}`))
					return
				}
				email := "one@example.test"
				if changed {
					email = "different@example.test"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{map[string]any{"id": "one", "name": "one.json", "auth_index": "index", "email": email, "provider": "codex", "disabled": disabled}}})
			}))
			defer server.Close()
			client, _ := NewHTTPClient(server.URL, "test-secret")
			err := client.Disable(context.Background(), Credential{ID: "one", Name: "one.json", AuthIndex: "index", Email: "one@example.test"})
			if changed {
				if err == nil || patched != 0 {
					t.Fatal("identity mismatch was not protected")
				}
			} else if err != nil || patched != 1 {
				t.Fatal("expected one verified status patch")
			}
		})
	}
}

func TestClientContractModelsAndSecretRedaction(t *testing.T) {
	modelsReads := 0
	incompatible := false
	brokenSchema := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v8/management/plugins":
			version := "0.4.4"
			if incompatible {
				version = "0.5.0"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"plugins": []any{map[string]any{"id": CandyID, "registered": true, "effective_enabled": true, "metadata": map[string]string{"version": version}}}})
		case "/v8/management/credentials":
			_, _ = w.Write([]byte(`{"files":[{"id":"one","name":"one.json","auth_index":"index","provider":"codex","email":"private@example.test"},{"id":"provider","name":"other.json","runtime_only":true,"provider":"codex"},{"id":"other","name":"two.json","provider":"claude"}]}`))
		case candyBase + "/state":
			if brokenSchema {
				_, _ = w.Write([]byte(`{"auths":[]}`))
			} else {
				_, _ = w.Write([]byte(`{"credentials":[{"id":"one","source":"auth_files","provider":"codex","fingerprints":[]}],"storage_error":""}`))
			}
		case "/v8/management/credentials/models":
			modelsReads++
			if r.URL.Query().Get("auth_index") != "index" {
				t.Error("models not pinned by auth index")
			}
			_, _ = w.Write([]byte(`{"models":[{"id":"gpt-6-astra"}]}`))
		default:
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"error":"test-secret"}`))
		}
	}))
	defer server.Close()
	client, _ := NewHTTPClient(server.URL, "test-secret")
	snapshot, err := client.Snapshot(context.Background())
	if err != nil || len(snapshot.Credentials) != 1 {
		t.Fatal("credential filtering failed", err)
	}
	for range 2 {
		models, err := client.Models(context.Background(), snapshot.Credentials[0])
		if err != nil || len(models) != 1 {
			t.Fatal("models failed")
		}
	}
	if modelsReads != 1 {
		t.Fatal("models were not cached")
	}
	raw, _ := json.Marshal(snapshot.Credentials)
	if strings.Contains(string(raw), "private@example") || strings.Contains(string(raw), "auth_index") {
		t.Fatal("backend identity leaked")
	}
	incompatible = true
	if _, err = client.Snapshot(context.Background()); err == nil {
		t.Fatal("unsupported version accepted")
	}
	incompatible = false
	brokenSchema = true
	if _, err = client.Snapshot(context.Background()); err == nil {
		t.Fatal("broken schema accepted")
	}
	err = client.call(context.Background(), "GET", "/failure", nil, nil)
	if err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatal("upstream error exposed secret")
	}
}

func TestUIAndManagementRoutes(t *testing.T) {
	e, _, _ := makeEngine(t, 0)
	response := serve(e, "", managementRequest{Method: "GET", Path: UIPath})
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), "Codex 自动巡检") || strings.Contains(string(response.Body), "/*ICONS*/") {
		t.Fatal("UI embedding failed")
	}
	if !strings.Contains(response.Headers.Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Fatal("CSP missing")
	}
	bad := serve(e, "", managementRequest{Method: "PUT", Path: APIBase + "/settings", Body: []byte(`{"per_credential_concurrency":4}`)})
	if bad.StatusCode != 400 {
		t.Fatal("invalid settings accepted")
	}
	if result := serve(nil, "Missing key", managementRequest{Method: "GET", Path: APIBase + "/state"}); result.StatusCode != 503 {
		t.Fatal("missing backend was not visible")
	}
}

func TestPluginRegistrationMetadata(t *testing.T) {
	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			Schema       int             `json:"schema_version"`
			Metadata     map[string]any  `json:"metadata"`
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"result"`
	}
	if json.Unmarshal(HandleMethod("plugin.register", nil), &result) != nil || !result.OK {
		t.Fatal("registration envelope invalid")
	}
	for _, name := range []string{"Name", "Version", "Author", "GitHubRepository"} {
		if result.Result.Metadata[name] == "" || result.Result.Metadata[name] == nil {
			t.Fatal("missing metadata field", name)
		}
	}
	if result.Result.Schema != 6 || !result.Result.Capabilities["management_api"] {
		t.Fatal("missing management capability")
	}
}

func TestClientNoRedirects(t *testing.T) {
	externalCalls := 0
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { externalCalls++ }))
	defer external.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, external.URL, 302) }))
	defer local.Close()
	c, _ := NewHTTPClient(local.URL, "secret")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if c.call(ctx, "GET", "/", nil, nil) == nil || externalCalls != 0 {
		t.Fatal("management secret could follow redirects")
	}
}
