package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureGitCredentialsOverSSHWritesACredentialLineMatchingGitCredentialStore(t *testing.T) {
	home := t.TempDir()
	runner := localExecRunner{dir: home}
	if err := ConfigureGitCredentialsOverSSH(runner, "http://10.100.0.1:8080/owner/repo.git", "secret-token", GitIdentity{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".git-credentials"))
	if err != nil {
		t.Fatal(err)
	}
	want := "http://sandbox:secret-token@10.100.0.1:8080\n"
	if string(data) != want {
		t.Errorf("got %q, want %q", data, want)
	}
}

func TestConfigureGitCredentialsOverSSHWritesAGitconfigWithTheCredentialHelperAndAnIdentity(t *testing.T) {
	home := t.TempDir()
	runner := localExecRunner{dir: home}
	if err := ConfigureGitCredentialsOverSSH(runner, "http://proxy:8080/owner/repo.git", "tok", GitIdentity{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
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

func TestConfigureGitCredentialsOverSSHFilesAreReadableOnlyByTheOwner(t *testing.T) {
	home := t.TempDir()
	runner := localExecRunner{dir: home}
	if err := ConfigureGitCredentialsOverSSH(runner, "http://proxy:8080/owner/repo.git", "tok", GitIdentity{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git-credentials", ".gitconfig"} {
		info, err := os.Stat(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestConfigureGitCredentialsOverSSHRejectsAMalformedRemoteURL(t *testing.T) {
	runner := localExecRunner{dir: t.TempDir()}
	if err := ConfigureGitCredentialsOverSSH(runner, "not-a-url", "tok", GitIdentity{}); err == nil {
		t.Fatal("expected an error for a remote URL with no scheme or host")
	}
}

func TestConfigureGitCredentialsOverSSHSurfacesARemoteWriteFailure(t *testing.T) {
	// A session directory the remote user has no permission to write
	// into -- exercises the exitCode != 0 path a real SSH session would
	// hit if e.g. the VM's disk were full or its home directory somehow
	// unwritable.
	if os.Geteuid() == 0 {
		// A 0500 directory stops every user but root, which is who a
		// container-based CI job or a `sudo make test` runs as -- there
		// is no permission left to deny, so the write this test wants to
		// see fail would succeed. Skipped rather than left to fail
		// confusingly, the same reason the missing-credential test stopped
		// depending on $PATH.
		t.Skip("root ignores directory permissions; nothing here can make the write fail")
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(home, 0o700) })
	runner := localExecRunner{dir: home}
	if err := ConfigureGitCredentialsOverSSH(runner, "http://proxy:8080/owner/repo.git", "tok", GitIdentity{}); err == nil {
		t.Fatal("expected an error writing into an unwritable session directory")
	}
}
