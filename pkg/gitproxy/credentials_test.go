package gitproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
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

func TestNamesListsEveryConfiguredCredentialOnce(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"*": "bot"},
		map[string]string{"bot": "bot-token", "release-bot": "release-token"})
	// A credential backed by both file shapes is still one credential --
	// load prefers the App one, so counting it twice would offer a
	// duplicate capability for a single token.
	if err := os.WriteFile(filepath.Join(dir, "release-bot.app.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := set.Names()
	want := []string{"bot", "release-bot"}
	if !slices.Equal(got, want) {
		t.Errorf("Names() = %v, want %v -- credentials.json itself is not a credential", got, want)
	}
}

func TestExtraNamesIsEveryCredentialButTheDefault(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"*": "bot", "acme/*": "acme-bot"},
		map[string]string{"bot": "bot-token", "acme-bot": "acme-token", "release-bot": "release-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.DefaultName(); got != "bot" {
		t.Errorf("DefaultName() = %q, want %q", got, "bot")
	}
	// acme-bot stays in: a narrower ladder entry says which repos reach
	// it without being asked, which is a different question from which
	// tokens a task may ask for by name.
	got, want := set.ExtraNames(), []string{"acme-bot", "release-bot"}
	if !slices.Equal(got, want) {
		t.Errorf("ExtraNames() = %v, want %v", got, want)
	}
}

func TestExtraNamesIsEmptyWithNothingConfigured(t *testing.T) {
	set, err := LoadCredentialSet(filepath.Join(t.TempDir(), "never-created"))
	if err != nil {
		t.Fatal(err)
	}
	if got := set.ExtraNames(); len(got) != 0 {
		t.Errorf("ExtraNames() = %v, want none -- an unreadable secrets directory configures no credentials", got)
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

// load's cache is the one piece of mutable state a CredentialSet carries,
// and Select/Get are called from GitProxy.Handle -- one goroutine per
// inbound HTTP request in a real deployment, so two sandboxes doing git
// operations at the same moment reach it concurrently. Run with -race:
// before mu guarded the cache, this reliably reported a concurrent map
// read/write.
func TestSelectAndGetAreSafeForConcurrentCallers(t *testing.T) {
	dir := writeCredentialSet(t,
		map[string]string{"acme/widgets": "narrow", "acme/*": "wide", "*": "global"},
		map[string]string{"narrow": "narrow-token", "wide": "wide-token", "global": "global-token", "named": "named-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			switch i % 4 {
			case 0:
				if _, ok := set.Select("acme", "widgets"); !ok {
					t.Error("expected acme/widgets to resolve")
				}
			case 1:
				if _, ok := set.Select("acme", "other"); !ok {
					t.Error("expected acme/other to resolve")
				}
			case 2:
				if _, ok := set.Select("someone-else", "repo"); !ok {
					t.Error("expected someone-else/repo to resolve")
				}
			case 3:
				if _, ok := set.Get("named"); !ok {
					t.Error("expected named to resolve")
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
}
