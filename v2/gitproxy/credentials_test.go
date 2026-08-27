package gitproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeCredentialSet(t *testing.T, patterns map[string]string, tokens map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if patterns != nil {
		data, err := json.Marshal(patterns)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, token := range tokens {
		if err := os.WriteFile(filepath.Join(dir, name+".token"), []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSelectPrefersTheExactRepoPattern(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{
		"acme/widgets": "narrow", "acme/*": "wide", "*": "global",
	}, map[string]string{"narrow": "narrow-token", "wide": "wide-token", "global": "global-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	cred, ok := set.Select("acme", "widgets")
	if !ok || cred.Name != "narrow" || cred.Token == nil || *cred.Token != "narrow-token" {
		t.Fatalf("got %+v, %v", cred, ok)
	}
}

func TestSelectFallsBackToOwnerWildcardThenGlobal(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{
		"acme/*": "wide", "*": "global",
	}, map[string]string{"wide": "wide-token", "global": "global-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cred, ok := set.Select("acme", "other"); !ok || cred.Name != "wide" {
		t.Fatalf("got %+v, %v", cred, ok)
	}
	if cred, ok := set.Select("someone-else", "repo"); !ok || cred.Name != "global" {
		t.Fatalf("got %+v, %v", cred, ok)
	}
}

func TestSelectIsCaseAndDotGitInsensitive(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"acme/widgets": "narrow"},
		map[string]string{"narrow": "tok"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Select("ACME", "Widgets.git"); !ok {
		t.Error("expected a case/suffix-insensitive match")
	}
}

func TestSelectFailsClosedWithNoCoveringPattern(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"acme/widgets": "narrow"},
		map[string]string{"narrow": "tok"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Select("someone-else", "repo"); ok {
		t.Error("expected no credential to cover this repo")
	}
}

func TestSelectAnonymousCredentialHasNoToken(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"*": "anonymous"}, nil)
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	cred, ok := set.Select("acme", "widgets")
	if !ok || cred.Name != "anonymous" || cred.Token != nil {
		t.Fatalf("got %+v, %v", cred, ok)
	}
}

func TestMissingCredentialsFileYieldsAnEmptyLadder(t *testing.T) {
	set, err := LoadCredentialSet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Select("acme", "widgets"); ok {
		t.Error("expected no credential without a credentials.json")
	}
}

func TestGetReturnsANamedCredentialDirectly(t *testing.T) {
	dir := writeCredentialSet(t, nil, map[string]string{"workflow": "wf-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	cred, ok := set.Get("workflow")
	if !ok || cred.Name != "workflow" || cred.Token == nil || *cred.Token != "wf-token" {
		t.Fatalf("got %+v, %v", cred, ok)
	}
}

func TestGetFailsClosedForAnUnconfiguredName(t *testing.T) {
	set, err := LoadCredentialSet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Get("nonexistent"); ok {
		t.Error("expected Get to fail closed for a missing token file")
	}
}

func TestGetAnonymousNeedsNoTokenFile(t *testing.T) {
	set, err := LoadCredentialSet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cred, ok := set.Get("anonymous")
	if !ok || cred.Token != nil {
		t.Fatalf("got %+v, %v", cred, ok)
	}
}
