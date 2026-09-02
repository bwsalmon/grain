package gitproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
)

// writeAppCredential writes name.app.json under dir, the *.app.json
// counterpart writeCredentialSet's tokens map writes for *.token
// credentials (credentials_test.go).
func writeAppCredential(t *testing.T, dir, name string) {
	t.Helper()
	key := testAppKey(t)
	data, err := json.Marshal(map[string]string{
		"app_id":          "12345",
		"installation_id": "67890",
		"private_key":     string(pkcs1PEM(key)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".app.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mintedTokenResponse(token, expiresAt string) github.ApiResponse {
	body, _ := json.Marshal(map[string]string{"token": token, "expires_at": expiresAt})
	return github.ApiResponse{Status: 201, Body: body}
}

func TestSelectResolvesAnAppBackedCredential(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"acme/widgets": "ci-app"}, nil)
	writeAppCredential(t, dir, "ci-app")
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	set.AppTransport = github.NewFakeTransport(mintedTokenResponse("ghs_minted", "2026-08-30T13:00:00Z"))

	cred, ok := set.Select("acme", "widgets")
	if !ok || cred.Name != "ci-app" || cred.Token == nil || *cred.Token != "ghs_minted" {
		t.Fatalf("got %+v, %v", cred, ok)
	}
}

func TestGetRecognizesAnAppJSONCredentialWithNoTokenFile(t *testing.T) {
	dir := writeCredentialSet(t, nil, nil)
	writeAppCredential(t, dir, "ci-app")
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	set.AppTransport = github.NewFakeTransport(mintedTokenResponse("ghs_minted", "2026-08-30T13:00:00Z"))

	cred, ok := set.Get("ci-app")
	if !ok || cred.Token == nil || *cred.Token != "ghs_minted" {
		t.Fatalf("got %+v, %v", cred, ok)
	}
}

func TestLoadCachesAnAppTokenUntilItNearsExpiry(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"*": "ci-app"}, nil)
	writeAppCredential(t, dir, "ci-app")
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	transport := github.NewFakeTransport(
		mintedTokenResponse("ghs_first", "2026-08-30T13:00:00Z"),
		mintedTokenResponse("ghs_second", "2026-08-30T14:00:00Z"),
	)
	set.AppTransport = transport

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	set.Now = func() time.Time { return now }

	cred, _ := set.Select("acme", "widgets")
	if *cred.Token != "ghs_first" {
		t.Fatalf("first call: got token %q", *cred.Token)
	}

	// Well before expiry (minus the refresh skew): still cached, no
	// second mint.
	now = now.Add(30 * time.Minute)
	cred, _ = set.Select("acme", "widgets")
	if *cred.Token != "ghs_first" {
		t.Fatalf("within lifetime: got token %q, want the cached one", *cred.Token)
	}
	if len(transport.Calls) != 1 {
		t.Fatalf("expected exactly one mint call so far, got %d", len(transport.Calls))
	}

	// Inside appTokenRefreshSkew of the first token's own expiry: load
	// re-mints rather than serving a token GitHub is about to invalidate.
	now = now.Add(29 * time.Minute)
	cred, _ = set.Select("acme", "widgets")
	if *cred.Token != "ghs_second" {
		t.Fatalf("near expiry: got token %q, want a freshly minted one", *cred.Token)
	}
	if len(transport.Calls) != 2 {
		t.Fatalf("expected a second mint call once the first token neared expiry, got %d", len(transport.Calls))
	}
}

func TestLoadFallsBackToAnonymousWhenMintingFailsAndRetriesAfterADelay(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"*": "ci-app"}, nil)
	writeAppCredential(t, dir, "ci-app")
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	transport := github.NewFakeTransport(
		github.ApiResponse{Status: 403, Body: []byte(`{"message":"bad key"}`)},
		mintedTokenResponse("ghs_recovered", "2026-08-30T14:00:00Z"),
	)
	set.AppTransport = transport

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	set.Now = func() time.Time { return now }

	cred, ok := set.Select("acme", "widgets")
	if !ok || cred.Token != nil {
		t.Fatalf("expected a failed mint to serve as anonymous, got %+v, %v", cred, ok)
	}
	if len(transport.Calls) != 1 {
		t.Fatalf("expected one mint attempt, got %d", len(transport.Calls))
	}

	// Immediately retrying does not hammer GitHub's token endpoint again.
	cred, _ = set.Select("acme", "widgets")
	if cred.Token != nil {
		t.Fatal("expected the cached failure to still read as anonymous")
	}
	if len(transport.Calls) != 1 {
		t.Fatalf("expected no retry before appMintFailureRetryDelay elapsed, got %d calls", len(transport.Calls))
	}

	now = now.Add(appMintFailureRetryDelay + time.Second)
	cred, ok = set.Select("acme", "widgets")
	if !ok || cred.Token == nil || *cred.Token != "ghs_recovered" {
		t.Fatalf("expected a retry to succeed once the key was fixed, got %+v, %v", cred, ok)
	}
	if len(transport.Calls) != 2 {
		t.Fatalf("expected a second mint attempt after the retry delay, got %d", len(transport.Calls))
	}
}
