package ui_test

// The agent-credential half of Settings: setting the key each framework
// runs as, clearing it, and reporting which are set -- without any
// response ever carrying a value back, the same rule the secrets pane
// these are built on already holds to.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/secrets"
	"github.com/bwsalmon/grain/pkg/ui"
)

func TestAgentKeysDisabledWithoutASecretsStore(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/agent-keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := decode[map[string]any](t, rec); got["enabled"] != false {
		t.Fatalf("enabled = %v, want false", got["enabled"])
	}

	for _, call := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/agent-keys/claude", `{"value":"sk-ant-oat01-fake"}`},
		{http.MethodDelete, "/api/agent-keys/claude", ""},
	} {
		rec := do(t, srv, call.method, call.path, call.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404: %s", call.method, call.path, rec.Code, rec.Body)
		}
	}
}

func TestSetClaudeOAuthTokenStoresItWhereTheDaemonReadsIt(t *testing.T) {
	srv, store := testServerWithSecrets(t)

	rec := do(t, srv, http.MethodPut, "/api/agent-keys/claude", `{"value":"  sk-ant-oat01-fake\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "sk-ant-oat01-fake") {
		t.Fatalf("response leaked the token: %s", rec.Body)
	}
	got := decode[map[string]any](t, rec)
	if got["claudeOAuthTokenSet"] != true {
		t.Fatalf("claudeOAuthTokenSet = %v, want true", got["claudeOAuthTokenSet"])
	}
	if got["geminiApiKeySet"] != false {
		t.Fatalf("geminiApiKeySet = %v, want false -- only the claude key was set", got["geminiApiKeySet"])
	}

	// The bare secret name, not "<name>/<key>": that sole-key form is
	// exactly how cmd/grain's own agentCredential resolves it, so this is
	// the assertion that a token pasted into the UI is a token the daemon
	// finds. Whitespace around a pasted value is gone, since the same
	// call would otherwise store something that reads as set and
	// authenticates as nothing.
	value, err := store.Resolve(context.Background(), secrets.ClaudeOAuthTokenSecret)
	if err != nil {
		t.Fatalf("Resolve(%q) = %v", secrets.ClaudeOAuthTokenSecret, err)
	}
	if value != "sk-ant-oat01-fake" {
		t.Fatalf("stored token = %q, want it trimmed", value)
	}
}

func TestSetGeminiAPIKeyThenClearIt(t *testing.T) {
	srv, store := testServerWithSecrets(t)

	rec := do(t, srv, http.MethodPut, "/api/agent-keys/gemini", `{"value":"AIza-fake"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := decode[map[string]any](t, rec); got["geminiApiKeySet"] != true {
		t.Fatalf("geminiApiKeySet = %v, want true", got["geminiApiKeySet"])
	}

	rec = do(t, srv, http.MethodDelete, "/api/agent-keys/gemini", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := decode[map[string]any](t, rec); got["geminiApiKeySet"] != false {
		t.Fatalf("geminiApiKeySet = %v after clearing, want false", got["geminiApiKeySet"])
	}
	if _, err := store.Resolve(context.Background(), secrets.GeminiAPIKeySecret); err == nil {
		t.Fatal("the cleared key still resolves")
	}
}

func TestSetAgentKeyRejectsBlanksAndUnknownFrameworks(t *testing.T) {
	srv, _ := testServerWithSecrets(t)

	rec := do(t, srv, http.MethodPut, "/api/agent-keys/gemini", `{"value":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("whitespace-only value status = %d, want 400: %s", rec.Code, rec.Body)
	}
	rec = do(t, srv, http.MethodPut, "/api/agent-keys/gpt", `{"value":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown framework status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestSettingsReportsWhichAgentKeysAreSet(t *testing.T) {
	srv, _ := testServerWithSecrets(t)

	// Before anything is stored, and before this deployment's settings
	// have ever been saved at all -- the state an operator setting one up
	// is actually in.
	settings := decode[ui.Settings](t, do(t, srv, http.MethodGet, "/api/settings", ""))
	if !settings.AgentKeysEnabled {
		t.Fatal("agentKeysEnabled = false on a server with a secrets store")
	}
	if settings.GeminiAPIKeySet || settings.ClaudeOAuthTokenSet {
		t.Fatalf("keys report as set before anything was stored: %+v", settings)
	}

	if rec := do(t, srv, http.MethodPut, "/api/agent-keys/claude", `{"value":"sk-ant-oat01-fake"}`); rec.Code != http.StatusOK {
		t.Fatalf("setting the claude token: %d %s", rec.Code, rec.Body)
	}
	settings = decode[ui.Settings](t, do(t, srv, http.MethodGet, "/api/settings", ""))
	if !settings.ClaudeOAuthTokenSet || settings.GeminiAPIKeySet {
		t.Fatalf("settings = %+v, want only the claude token set", settings)
	}
}
