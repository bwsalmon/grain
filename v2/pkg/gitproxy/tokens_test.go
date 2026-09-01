package gitproxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	// Globbed rather than checked under one fixed name: save's temp file
	// carries a random suffix (its own doc comment), so a leftover would
	// not be sitting at path+".tmp" to be found.
	leftovers, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("expected no leftover temp file, found %v", leftovers)
	}
}

// TestEnsureTokenIsSafeForConcurrentCallers pins what stopped being true
// when a token stopped being minted per slot at startup and started being
// minted per run, from one goroutine per dispatch (orchestrator's own
// reconcileDispatch). Unguarded, the read-modify-write here loses tokens
// two ways at once: whichever save renames last drops every entry the
// others added, and concurrent writes through one fixed temp-file name
// publish half-written JSON, so a mint can fail outright with a parse
// error or an ENOENT rename. Every token this hands back has to be one
// the proxy can then authenticate -- a run holding a token that never
// reached the file fails every git operation it makes for as long as it
// lives.
func TestEnsureTokenIsSafeForConcurrentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-tokens.json")
	store := NewSandboxTokenStore(path)

	const sandboxes = 16
	names := make([]string, sandboxes)
	minted := make([]string, sandboxes)
	errs := make([]error, sandboxes)
	var wg sync.WaitGroup
	for i := range names {
		names[i] = fmt.Sprintf("t%d-r1", i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			minted[i], errs[i] = store.EnsureToken(names[i])
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureToken(%s): %v", names[i], err)
		}
	}

	tokens, err := LoadSandboxTokens(path)
	if err != nil {
		t.Fatalf("LoadSandboxTokens after concurrent minting: %v", err)
	}
	for i, token := range minted {
		name, ok := tokens.Authenticate(token)
		if !ok {
			t.Errorf("the token minted for %s never reached the file -- that run's git would fail closed", names[i])
			continue
		}
		if name != names[i] {
			t.Errorf("token minted for %s authenticates as %q", names[i], name)
		}
	}
}

// A token minted while the proxy is already serving has to authenticate,
// which is the whole reason SandboxTokens re-reads on a miss -- and it has
// to keep doing so when several sandboxes mint at once, where a reload
// that read the file earlier must not be able to install its map over one
// that read it later.
func TestAuthenticateSeesTokensMintedConcurrentlyAfterLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-tokens.json")
	store := NewSandboxTokenStore(path)

	// Loaded against a file that does not exist yet, so every token below
	// is one minted after this proxy started serving.
	tokens, err := LoadSandboxTokens(path)
	if err != nil {
		t.Fatal(err)
	}

	const sandboxes = 16
	results := make([]bool, sandboxes)
	var wg sync.WaitGroup
	for i := 0; i < sandboxes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("t%d-r1", i)
			token, err := store.EnsureToken(name)
			if err != nil {
				t.Errorf("EnsureToken(%s): %v", name, err)
				return
			}
			got, ok := tokens.Authenticate(token)
			results[i] = ok && got == name
		}(i)
	}
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Errorf("t%d-r1's freshly minted token did not authenticate", i)
		}
	}
}

func TestRevokeDropsOneSandboxAndLeavesTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-tokens.json")
	store := NewSandboxTokenStore(path)

	gone, err := store.EnsureToken("t1-r1")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.EnsureToken("t2-r1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke("t1-r1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	tokens, err := LoadSandboxTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tokens.Authenticate(gone); ok {
		t.Error("a revoked sandbox's token should no longer authenticate")
	}
	if name, ok := tokens.Authenticate(kept); !ok || name != "t2-r1" {
		t.Errorf("revoking one sandbox took another's token with it: got %q, %v", name, ok)
	}
}

// Revoking a sandbox that never minted a token is what a run which failed
// before it got that far leaves behind, and revoking twice is what a
// retried cleanup does. Neither is a fault.
func TestRevokeAnUnknownSandboxIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-tokens.json")
	store := NewSandboxTokenStore(path)
	if err := store.Revoke("never-minted"); err != nil {
		t.Fatalf("Revoke against a missing file: %v", err)
	}
	if _, err := store.EnsureToken("t1-r1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke("t1-r1"); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if err := store.Revoke("t1-r1"); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
}
