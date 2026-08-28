package secrets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReadsFlatDirectoryTrimmingTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gcp-key-minter"), "super-secret-key\n")
	writeFile(t, filepath.Join(dir, "gemini-key"), "no-trailing-newline")

	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Resolve(context.Background(), "gcp-key-minter")
	if err != nil {
		t.Fatal(err)
	}
	if got != "super-secret-key" {
		t.Errorf("Resolve(gcp-key-minter) = %q, want trailing newline trimmed", got)
	}

	got, err = store.Resolve(context.Background(), "gemini-key")
	if err != nil {
		t.Fatal(err)
	}
	if got != "no-trailing-newline" {
		t.Errorf("Resolve(gemini-key) = %q", got)
	}
}

func TestResolveUnknownNameIsAnError(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(context.Background(), "never-configured"); err == nil {
		t.Fatal("expected an error resolving a name nothing configured, got nil")
	}
}

func TestOpenSkipsKubeletBookkeepingEntriesAndFollowsItsSymlinks(t *testing.T) {
	// The real shape kubelet's atomic writer produces for a mounted
	// Secret: a timestamped directory holding the actual files, a
	// "..data" symlink to it, and each key at the top level symlinked
	// through "..data" rather than written directly.
	dir := t.TempDir()
	versioned := filepath.Join(dir, "..2024_01_01_00_00_00.000000000")
	if err := os.Mkdir(versioned, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(versioned, "api-token"), "material\n")

	dataLink := filepath.Join(dir, "..data")
	if err := os.Symlink("..2024_01_01_00_00_00.000000000", dataLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "api-token"), filepath.Join(dir, "api-token")); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Resolve(context.Background(), "api-token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "material" {
		t.Errorf("Resolve(api-token) = %q, want material read through the kubelet symlink chain", got)
	}

	for _, name := range store.Names() {
		if name == "..data" || name == "..2024_01_01_00_00_00.000000000" {
			t.Errorf("Names() = %v, should never include kubelet's own bookkeeping entries", store.Names())
		}
	}
}

func TestOpenSkipsOrdinarySubdirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "not-a-secret"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "real-secret"), "value")

	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := store.Names(), []string{"real-secret"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

func TestNamesIsSorted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "zeta"), "z")
	writeFile(t, filepath.Join(dir, "alpha"), "a")
	writeFile(t, filepath.Join(dir, "mu"), "m")

	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "mu", "zeta"}
	got := store.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

func TestMissingReportsUnconfiguredNames(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gcp-key-minter"), "present")

	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	missing := store.Missing([]string{"gcp-key-minter", "gemini-key", "github-bot-token"})
	want := []string{"gemini-key", "github-bot-token"}
	if len(missing) != len(want) {
		t.Fatalf("Missing() = %v, want %v", missing, want)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Fatalf("Missing() = %v, want %v", missing, want)
		}
	}
}

func TestOpenNonexistentDirectoryIsAnError(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected an error opening a directory that does not exist")
	}
}
