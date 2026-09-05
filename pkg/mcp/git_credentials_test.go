package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureGitCredentialsWritesACredentialLineMatchingGitCredentialStore(t *testing.T) {
	root := t.TempDir()
	if err := ConfigureGitCredentials(root, "http://10.100.0.1:8080/owner/repo.git", "secret-token", GitIdentity{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".git-credentials"))
	if err != nil {
		t.Fatal(err)
	}
	want := "http://sandbox:secret-token@10.100.0.1:8080\n"
	if string(data) != want {
		t.Errorf("got %q, want %q", data, want)
	}
}

func TestConfigureGitCredentialsWritesAGitconfigWithTheCredentialHelperAndAnIdentity(t *testing.T) {
	root := t.TempDir()
	if err := ConfigureGitCredentials(root, "http://proxy:8080/owner/repo.git", "tok", GitIdentity{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"helper = store", "name = grain agent", "email = grain-agent@localhost"} {
		if !strings.Contains(got, want) {
			t.Errorf(".gitconfig missing %q, got:\n%s", want, got)
		}
	}
}

func TestConfigureGitCredentialsFilesAreReadableOnlyByTheOwner(t *testing.T) {
	root := t.TempDir()
	if err := ConfigureGitCredentials(root, "http://proxy:8080/owner/repo.git", "tok", GitIdentity{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git-credentials", ".gitconfig"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestConfigureGitCredentialsRejectsAMalformedRemoteURL(t *testing.T) {
	if err := ConfigureGitCredentials(t.TempDir(), "not-a-url", "tok", GitIdentity{}); err == nil {
		t.Fatal("expected an error for a remote URL with no scheme or host")
	}
}
