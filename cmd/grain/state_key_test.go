package main

// Restoring an installation, from the secrets' point of view.
//
// The encrypted file lives beside the key under <data-dir>/secrets and
// not in the state repository (secretsConfig), so moving a deployment to
// a new host is two separate acts: adopt the repository, which brings
// every table, and restore <data-dir>/secrets, which brings the
// ciphertext. The key is deliberately the part that travels by hand, and
// this file is what happens when it has not arrived yet -- a store this
// host cannot read, which has to say so and name the key that would open
// it rather than mint a new one over the file.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/secrets"
	"github.com/bwsalmon/grain/pkg/ui"
)

// bareRemote creates an empty repository, which is what "create one on
// GitHub and point grain at it" produces.
func bareRemote(t *testing.T) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "state.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", remote).CombinedOutput(); err != nil {
		t.Fatalf("creating the remote: %v: %s", err, out)
	}
	return remote
}

// installationWithASecret is a deployment that holds one secret,
// returning its data directory. Its key stays in it: what a restore
// carries is the ciphertext, and the point of these tests is the host
// that has only that.
func installationWithASecret(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	if err := secrets.Open(secretsConfig(dataDir)).Set("db", "password", []byte("hunter2")); err != nil {
		t.Fatalf("setting a secret: %v", err)
	}
	return dataDir
}

// restoreCiphertext copies one installation's encrypted file to another
// host and nothing else -- the backup an operator restores, without the
// key they kept somewhere a backup does not reach.
func restoreCiphertext(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(secretsConfig(from).File)
	if err != nil {
		t.Fatalf("reading the encrypted file: %v", err)
	}
	dst := secretsConfig(to).File
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The restore: another installation's secrets on a host that has never
// held its key.
func TestRestoringAnInstallationNeedsItsKey(t *testing.T) {
	ctx := context.Background()
	origin := installationWithASecret(t)
	originPublic, err := secrets.Open(secretsConfig(origin)).PublicKey()
	if err != nil {
		t.Fatalf("reading the origin's public key: %v", err)
	}

	host := t.TempDir()
	restoreCiphertext(t, origin, host)
	if err := stateAdopt(ctx, host, []string{"-remote", bareRemote(t)}); err != nil {
		t.Fatalf("adopting: %v", err)
	}

	hostStore := secrets.Open(secretsConfig(host))
	if err := hostStore.Check(); err == nil {
		t.Fatal("a host with none of the origin's key material claimed it could read its secrets")
	}
	// No key was minted over the file, which would have made the secrets
	// permanently unreadable while looking like it had worked.
	if hostStore.KeyCreated() {
		t.Fatal("a fresh key was minted over a restored secrets file")
	}
	// What it needs is named, since the operator may hold several keys.
	recipient, err := hostStore.FileRecipient()
	if err != nil {
		t.Fatalf("reading the recipient: %v", err)
	}
	if recipient != originPublic {
		t.Fatalf("recipient %q, want %q", recipient, originPublic)
	}

	// Somebody else's key does not do.
	wrong, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	wrongFile := filepath.Join(t.TempDir(), "wrong.key")
	if err := secrets.WriteKeyFile(wrongFile, wrong); err != nil {
		t.Fatal(err)
	}
	if err := stateKeyImport(hostStore, []string{"-key-file", wrongFile}); err == nil {
		t.Fatal("a key that cannot open the file was installed")
	}

	// The operator's own key does, and that is the whole restore.
	if err := stateKeyImport(hostStore, []string{"-key-file", secretsConfig(origin).KeyFile}); err != nil {
		t.Fatalf("importing the origin's key: %v", err)
	}
	got, err := secrets.Open(secretsConfig(host)).Resolve(ctx, "db/password")
	if err != nil {
		t.Fatalf("resolving after the import: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("got %q", got)
	}
}

// The same restore in one step, which is what the bootstrap's "point
// grain at an existing repository" form does: URL, credential and key
// together.
func TestAdoptCanTakeTheSecretsKeyWithIt(t *testing.T) {
	ctx := context.Background()
	origin := installationWithASecret(t)

	host := t.TempDir()
	restoreCiphertext(t, origin, host)
	if err := stateAdopt(ctx, host, []string{
		"-remote", bareRemote(t), "-secrets-key-file", secretsConfig(origin).KeyFile,
	}); err != nil {
		t.Fatalf("adopting with a key: %v", err)
	}
	got, err := secrets.Open(secretsConfig(host)).Resolve(ctx, "db/password")
	if err != nil {
		t.Fatalf("resolving after the adopt: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("got %q", got)
	}
}

// A key that is not a key stops the adopt before anything has moved, so
// a typo does not leave the installation halfway between repositories.
func TestAdoptRejectsARubbishKeyBeforeTouchingAnything(t *testing.T) {
	dataDir := installationWithASecret(t)
	before, err := os.ReadFile(secretsConfig(dataDir).File)
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(bad, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = stateAdopt(context.Background(), dataDir, []string{
		"-remote", bareRemote(t), "-secrets-key-file", bad,
	})
	if err == nil {
		t.Fatal("an unparseable key was accepted")
	}
	if !strings.Contains(err.Error(), "must start with") {
		t.Fatalf("the error does not say what is wrong with the key: %v", err)
	}
	after, err := os.ReadFile(secretsConfig(dataDir).File)
	if err != nil {
		t.Fatalf("the secrets file went missing under a failed adopt: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("the secrets file changed under a failed adopt")
	}
}

// The same restore through the manager the UI's bootstrap pane talks to,
// rather than through the CLI: the pane is where an operator who has
// never opened a terminal on this host does this, so the path it uses is
// worth exercising as itself.
func TestTheBootstrapPaneCanRestoreAnInstallation(t *testing.T) {
	ctx := context.Background()
	origin := installationWithASecret(t)
	originKey, err := os.ReadFile(secretsConfig(origin).KeyFile)
	if err != nil {
		t.Fatal(err)
	}

	// A running grain, local-only, holding a restored file it cannot
	// read, being pointed at the repository that goes with it.
	host := t.TempDir()
	restoreCiphertext(t, origin, host)
	_, db, err := openStore(host)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer db.Close()
	repo, err := openStateRepo(ctx, host)
	if err != nil {
		t.Fatalf("opening the state repository: %v", err)
	}
	manager := newStateManager(host, db, repo, openSecrets(host), nil)

	status, err := manager.Adopt(ctx, ui.AdoptRequest{Remote: bareRemote(t)})
	if err != nil {
		t.Fatalf("adopting: %v", err)
	}
	// Adopted without the key, the pane has to say the secrets are
	// unreadable and which key would read them -- not report success and
	// leave it to a run to discover.
	if status.SecretsError == "" {
		t.Fatalf("the pane reports nothing wrong about secrets it cannot read: %+v", status)
	}
	if status.SecretsFileRecipient == "" {
		t.Fatalf("the pane does not say which key the file wants: %+v", status)
	}

	if _, err := manager.ImportSecretsKey(ctx, "not a key"); err == nil {
		t.Fatal("rubbish was accepted as a key")
	}
	status, err = manager.ImportSecretsKey(ctx, string(originKey))
	if err != nil {
		t.Fatalf("importing the key: %v", err)
	}
	if status.SecretsError != "" {
		t.Fatalf("the store is still unreadable after its key arrived: %+v", status)
	}
	got, err := secrets.Open(secretsConfig(host)).Resolve(ctx, "db/password")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("got %q", got)
	}
}
