package gitproxy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// grain/task-4: the *ladder* is written from the UI now too, not just
// the material -- an operator setting a deployment up for the first time
// has no shell on the host to hand-edit credentials.json with.

func TestSetPatternMakesACredentialTheDeploymentDefault(t *testing.T) {
	dir := writeCredentialSet(t, nil, map[string]string{"bot": "bot-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The state a fresh deployment is actually in: material on disk, no
	// pattern naming it, so every clone fails closed.
	if _, ok := set.Select("acme", "widgets"); ok {
		t.Fatal("Select succeeded before any pattern named a credential")
	}
	if got, err := set.SetPattern("*", "bot"); err != nil || got != "*" {
		t.Fatalf("SetPattern(*, bot) = %q, %v", got, err)
	}
	cred, ok := set.Select("acme", "widgets")
	if !ok || cred.Name != "bot" || cred.Token == nil || *cred.Token != "bot-token" {
		t.Fatalf("Select = %+v, %v, want the new default credential", cred, ok)
	}
	if set.DefaultName() != "bot" {
		t.Errorf("DefaultName() = %q, want bot", set.DefaultName())
	}
	if got := set.PatternsFor("bot"); !slices.Equal(got, []string{"*"}) {
		t.Errorf("PatternsFor(bot) = %v, want [*]", got)
	}
	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("credentials.json mode = %v, want 0600", info.Mode().Perm())
	}
}

// The whole reason this file is re-read: the ladder the git proxy is
// serving pushes through was loaded before any of this was written, and
// nothing can restart it from the pane that wrote it.
func TestAnotherLoadedLadderSeesAPatternWrittenAfterIt(t *testing.T) {
	dir := writeCredentialSet(t, nil, map[string]string{"bot": "bot-token"})
	proxy, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := proxy.Select("acme", "widgets"); ok {
		t.Fatal("Select succeeded with an empty ladder")
	}
	pane, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pane.SetPattern("*", "bot"); err != nil {
		t.Fatalf("SetPattern: %v", err)
	}
	cred, ok := proxy.Select("acme", "widgets")
	if !ok || cred.Token == nil || *cred.Token != "bot-token" {
		t.Fatalf("the already-loaded ladder got %+v, %v, want the credential just configured", cred, ok)
	}
}

func TestSetPatternCanonicalizesWhatItWrites(t *testing.T) {
	dir := writeCredentialSet(t, nil, map[string]string{"bot": "bot-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := set.SetPattern("  ACME/Widgets.git ", "bot")
	if err != nil {
		t.Fatalf("SetPattern: %v", err)
	}
	if got != "acme/widgets" {
		t.Fatalf("SetPattern wrote %q, want the canonical form Select looks up", got)
	}
	if cred, ok := set.Select("Acme", "Widgets"); !ok || cred.Name != "bot" {
		t.Errorf("Select = %+v, %v, want the pattern just written to cover it", cred, ok)
	}
}

func TestSetPatternRefusesACredentialThatIsNotConfigured(t *testing.T) {
	dir := writeCredentialSet(t, nil, map[string]string{"bot": "bot-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = func() error { _, err := set.SetPattern("*", "typo"); return err }()
	if err == nil || !strings.Contains(err.Error(), "no credential named") {
		t.Fatalf("SetPattern(*, typo) = %v, want it refused", err)
	}
	if set.DefaultName() != "" {
		t.Errorf("DefaultName() = %q, want the ladder left alone", set.DefaultName())
	}
}

// "anonymous" is not a file, it is load's no-Authorization-header shape
// -- the right answer for a public repo, and so a legitimate thing to
// point a pattern at.
func TestSetPatternAcceptsAnonymous(t *testing.T) {
	set, err := LoadCredentialSet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.SetPattern("acme/*", "anonymous"); err != nil {
		t.Fatalf("SetPattern(acme/*, anonymous): %v", err)
	}
	cred, ok := set.Select("acme", "widgets")
	if !ok || cred.Name != "anonymous" || cred.Token != nil {
		t.Fatalf("Select = %+v, %v, want the anonymous credential", cred, ok)
	}
}

func TestSetPatternRefusesShapesTheLadderNeverConsults(t *testing.T) {
	dir := writeCredentialSet(t, nil, map[string]string{"bot": "bot-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"", "acme", "*/widgets", "acme/wid*", "acme/widgets/extra", "ac me/*", "../*"} {
		if _, err := set.SetPattern(pattern, "bot"); err == nil {
			t.Errorf("SetPattern(%q) reported success", pattern)
		}
	}
	if len(set.Patterns()) != 0 {
		t.Errorf("Patterns() = %v, want nothing written", set.Patterns())
	}
}

func TestSetPatternKeepsTheEntriesItIsNotChanging(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"*": "bot", "acme/*": "bot"},
		map[string]string{"bot": "bot-token", "release": "release-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.SetPattern("acme/widgets", "release"); err != nil {
		t.Fatalf("SetPattern: %v", err)
	}
	want := map[string]string{"*": "bot", "acme/*": "bot", "acme/widgets": "release"}
	got := set.Patterns()
	if len(got) != len(want) {
		t.Fatalf("Patterns() = %v, want %v", got, want)
	}
	for pattern, name := range want {
		if got[pattern] != name {
			t.Errorf("Patterns()[%q] = %q, want %q", pattern, got[pattern], name)
		}
	}
}

func TestRemovePatternDropsOneEntryAndOnlyThat(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"*": "bot", "acme/widgets": "release"},
		map[string]string{"bot": "bot-token", "release": "release-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := set.RemovePattern("acme/widgets"); err != nil {
		t.Fatalf("RemovePattern: %v", err)
	}
	if cred, ok := set.Select("acme", "widgets"); !ok || cred.Name != "bot" {
		t.Errorf("Select = %+v, %v, want it to fall back to the default", cred, ok)
	}
	if err := set.RemovePattern("acme/widgets"); err == nil {
		t.Error("RemovePattern reported success for an entry that is not there")
	}
	// And removing the default is allowed: a deployment naming every
	// repo explicitly is a real shape, even though it fails closed for
	// everything else.
	if err := set.RemovePattern("*"); err != nil {
		t.Fatalf("RemovePattern(*): %v", err)
	}
	if _, ok := set.Select("acme", "widgets"); ok {
		t.Error("Select still succeeded with no pattern covering it")
	}
}

// A credentials.json someone has broken should cost the deployment the
// change, not every push it was already serving.
func TestAnUnparseableLadderKeepsTheOneAlreadyLoaded(t *testing.T) {
	dir := writeCredentialSet(t, map[string]string{"*": "bot"}, map[string]string{"bot": "bot-token"})
	set, err := LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Select("acme", "widgets"); !ok {
		t.Fatal("Select failed before the file was broken")
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cred, ok := set.Select("acme", "widgets"); !ok || cred.Name != "bot" {
		t.Errorf("Select = %+v, %v, want the last good ladder still serving", cred, ok)
	}
}
