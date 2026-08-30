package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/capability/githubsandbox"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
)

func TestAPIHostRewritesGitHubDotComOnly(t *testing.T) {
	if got := apiHost("github.com"); got != "api.github.com" {
		t.Errorf("apiHost(github.com) = %q, want api.github.com", got)
	}
	if got := apiHost("mock.example"); got != "mock.example" {
		t.Errorf("apiHost(mock.example) = %q, want it left unchanged for a test/mock host", got)
	}
}

func TestManifestSubmissionFormEscapesTheManifest(t *testing.T) {
	form := manifestSubmissionForm("https://github.com/settings/apps/new?state=abc", `{"name":"a\"b"}`)
	if !strings.Contains(form, "https://github.com/settings/apps/new?state=abc") {
		t.Errorf("form does not name the submit URL: %s", form)
	}
	if strings.Contains(form, `"a\"b"`) {
		t.Errorf("form embeds an unescaped quote from the manifest, which would break out of the value attribute: %s", form)
	}
	if !strings.Contains(form, "&quot;") {
		t.Errorf("form does not escape quotes in the manifest: %s", form)
	}
}

// TestWriteAppCredentialsRoundTripsThroughTheSecretsStore is the
// write-then-resolve round trip bwsalmon/agents#495 found missing:
// bootstrapGitHubApp used to write plain files under <secrets-dir>/
// github-app/, which githubsandbox.Provider.Resolve -- backed by
// pkg/secrets.Store, a SQLite database -- never read. This asserts the
// credentials writeAppCredentials stores are the exact ones
// DefaultAppIDCredential and DefaultPrivateKeyCredential resolve back to.
func TestWriteAppCredentialsRoundTripsThroughTheSecretsStore(t *testing.T) {
	dataDir := t.TempDir()
	const wantAppID = "123456"
	const wantPrivateKey = "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n"

	if err := writeAppCredentials(dataDir, wantAppID, wantPrivateKey); err != nil {
		t.Fatalf("writeAppCredentials: %v", err)
	}

	store := secrets.New(filepath.Join(dataDir, "secrets"))
	ctx := context.Background()

	gotAppID, err := store.Resolve(ctx, githubsandbox.DefaultAppIDCredential)
	if err != nil {
		t.Fatalf("resolving %s: %v", githubsandbox.DefaultAppIDCredential, err)
	}
	if gotAppID != wantAppID {
		t.Errorf("resolved app id %q, want %q", gotAppID, wantAppID)
	}

	gotPrivateKey, err := store.Resolve(ctx, githubsandbox.DefaultPrivateKeyCredential)
	if err != nil {
		t.Fatalf("resolving %s: %v", githubsandbox.DefaultPrivateKeyCredential, err)
	}
	if gotPrivateKey != wantPrivateKey {
		t.Errorf("resolved private key %q, want %q", gotPrivateKey, wantPrivateKey)
	}
}

func TestRandomHexIsUnpredictableAndTheRightLength(t *testing.T) {
	a, err := randomHex(16)
	if err != nil {
		t.Fatal(err)
	}
	b, err := randomHex(16)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 {
		t.Errorf("randomHex(16) has length %d, want 32 hex characters", len(a))
	}
	if a == b {
		t.Errorf("two calls to randomHex(16) produced the same value")
	}
}
