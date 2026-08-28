package gitproxy

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTokensFile(t *testing.T, dir string, tokens map[string]string) string {
	t.Helper()
	path := filepath.Join(dir, "sandbox-tokens.json")
	data, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuthenticateKnownToken(t *testing.T) {
	path := writeTokensFile(t, t.TempDir(), map[string]string{"sandbox-0": "tok0", "sandbox-1": "tok1"})
	tokens, err := LoadSandboxTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := tokens.Authenticate("tok0"); !ok || name != "sandbox-0" {
		t.Errorf("Authenticate(tok0) = %q, %v", name, ok)
	}
	if name, ok := tokens.Authenticate("tok1"); !ok || name != "sandbox-1" {
		t.Errorf("Authenticate(tok1) = %q, %v", name, ok)
	}
}

func TestAuthenticateUnknownToken(t *testing.T) {
	path := writeTokensFile(t, t.TempDir(), map[string]string{"sandbox-0": "tok0"})
	tokens, err := LoadSandboxTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tokens.Authenticate("not-a-real-token"); ok {
		t.Error("unknown token should not authenticate")
	}
}

func TestMissingFileAuthenticatesNothing(t *testing.T) {
	tokens, err := LoadSandboxTokens(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tokens.Authenticate("anything"); ok {
		t.Error("a missing file should authenticate nothing")
	}
}

func basicAuth(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func TestExtractBasicAuthTokenIgnoresUsername(t *testing.T) {
	if tok, ok := ExtractBasicAuthToken(basicAuth("whatever", "the-token")); !ok || tok != "the-token" {
		t.Errorf("got %q, %v", tok, ok)
	}
	if tok, ok := ExtractBasicAuthToken(basicAuth("", "the-token")); !ok || tok != "the-token" {
		t.Errorf("got %q, %v", tok, ok)
	}
}

func TestExtractBasicAuthTokenRejectsMalformedHeader(t *testing.T) {
	cases := []string{
		"",
		"Bearer sometoken",
		"Basic not-valid-base64!!!",
		"Basic " + base64.StdEncoding.EncodeToString([]byte("no-colon-here")),
	}
	for _, header := range cases {
		if _, ok := ExtractBasicAuthToken(header); ok {
			t.Errorf("ExtractBasicAuthToken(%q) should fail", header)
		}
	}
}

// --- SandboxTokenStore -------------------------------------------------

func TestEnsureTokenMintsANewTokenForAnUnknownSandbox(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox-tokens.json")
	store := NewSandboxTokenStore(path)
	token, err := store.EnsureToken("sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("expected a non-empty token")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["sandbox-0"] != token {
		t.Errorf("file contents = %v", got)
	}
}

func TestEnsureTokenIsIdempotent(t *testing.T) {
	store := NewSandboxTokenStore(filepath.Join(t.TempDir(), "sandbox-tokens.json"))
	first, err := store.EnsureToken("sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnsureToken("sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("first = %q, second = %q, want equal", first, second)
	}
}

func TestEnsureTokenMintsDistinctTokensPerSandbox(t *testing.T) {
	store := NewSandboxTokenStore(filepath.Join(t.TempDir(), "sandbox-tokens.json"))
	t0, err := store.EnsureToken("sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	t1, err := store.EnsureToken("sandbox-1")
	if err != nil {
		t.Fatal(err)
	}
	if t0 == t1 {
		t.Error("expected distinct tokens per sandbox")
	}
}

func TestEnsureTokenPreservesOtherSandboxesAlreadyOnFile(t *testing.T) {
	dir := t.TempDir()
	path := writeTokensFile(t, dir, map[string]string{"sandbox-0": "existing-token"})
	store := NewSandboxTokenStore(path)
	if _, err := store.EnsureToken("sandbox-1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["sandbox-0"] != "existing-token" {
		t.Errorf("sandbox-0 = %q, want unchanged", got["sandbox-0"])
	}
	if _, ok := got["sandbox-1"]; !ok {
		t.Error("sandbox-1 should have been added")
	}
}

func TestATokenMintedByTheStoreAuthenticatesViaSandboxTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-tokens.json")
	token, err := NewSandboxTokenStore(path).EnsureToken("sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := LoadSandboxTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := tokens.Authenticate(token); !ok || name != "sandbox-0" {
		t.Errorf("got %q, %v", name, ok)
	}
}

func TestRotateReplacesAnExistingToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-tokens.json")
	store := NewSandboxTokenStore(path)
	original, err := store.EnsureToken("sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := store.Rotate("sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if rotated == original {
		t.Fatal("rotate should mint a different token")
	}
	tokens, err := LoadSandboxTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tokens.Authenticate(original); ok {
		t.Error("the original token should no longer authenticate")
	}
	if name, ok := tokens.Authenticate(rotated); !ok || name != "sandbox-0" {
		t.Errorf("got %q, %v", name, ok)
	}
}

func TestSaveIsAtomicNoPartialFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox-tokens.json")
	if _, err := NewSandboxTokenStore(path).EnsureToken("sandbox-0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s to exist: %v", path, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected no leftover .tmp file, stat error = %v", err)
	}
}
