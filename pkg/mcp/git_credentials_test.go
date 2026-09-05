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
	for _, want := range []string{"helper = store", "name = " + DefaultGitIdentityName, "email = " + DefaultGitIdentityEmail} {
		if !strings.Contains(got, want) {
			t.Errorf(".gitconfig missing %q, got:\n%s", want, got)
		}
	}
}

// The point of the setting: a deployment that has chosen a committer gets
// that committer on every commit the sandbox makes, rather than grain's.
func TestConfigureGitCredentialsWritesTheConfiguredIdentity(t *testing.T) {
	root := t.TempDir()
	identity := GitIdentity{Name: "acme bot", Email: "bot@acme.example"}
	if err := ConfigureGitCredentials(root, "http://proxy:8080/owner/repo.git", "tok", identity); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"name = acme bot", "email = bot@acme.example"} {
		if !strings.Contains(got, want) {
			t.Errorf(".gitconfig missing %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, DefaultGitIdentityName) {
		t.Errorf(".gitconfig still carries grain's own name, got:\n%s", got)
	}
}

// Half an identity is a real answer -- a deployment that renames the
// committer and says nothing about the address -- and the half it left
// out has to come from grain's own, not from nothing: a sandbox with no
// user.email set is one where `git commit` fails outright.
func TestConfigureGitCredentialsFillsTheHalfOfAnIdentityLeftEmpty(t *testing.T) {
	root := t.TempDir()
	if err := ConfigureGitCredentials(root, "http://proxy:8080/owner/repo.git", "tok",
		GitIdentity{Name: "acme bot"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"name = acme bot", "email = " + DefaultGitIdentityEmail} {
		if !strings.Contains(got, want) {
			t.Errorf(".gitconfig missing %q, got:\n%s", want, got)
		}
	}
}

// ui.UpdateSettings refuses a line break on the way in, so this is about
// a value that reached grain_config some other way -- a hand-edited
// state-repo dump. It must come out as a mangled name rather than as a
// .gitconfig git can no longer parse: everything after the newline would
// otherwise be read as a key of its own.
func TestConfigureGitCredentialsFoldsALineBreakOutOfAnIdentity(t *testing.T) {
	root := t.TempDir()
	if err := ConfigureGitCredentials(root, "http://proxy:8080/owner/repo.git", "tok",
		GitIdentity{Name: "acme\nhelper = evil", Email: "bot@acme.example"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "name = acme helper = evil\n") {
		t.Errorf(".gitconfig did not fold the line break, got:\n%s", got)
	}
	if strings.Count(got, "helper = ") != 2 {
		t.Errorf(".gitconfig gained a second helper line, got:\n%s", got)
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
