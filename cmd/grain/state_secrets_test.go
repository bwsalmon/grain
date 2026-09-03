package main

// Adopting a repository, from the secrets' point of view.
//
// The state repository carries everything grain knows, and its secrets
// file is the one part of it that neither the database nor a clone can
// reconstitute: the ciphertext travels with the repository, and the key
// that opens it deliberately does not. That leaves two ways to lose
// every secret a deployment holds, and this file is both of them --
// adopting an empty repository must carry this installation's own
// secrets across rather than leave them in the tree it moved aside, and
// adopting somebody else's must let their key be installed rather than
// leaving a file nothing here can open.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/secrets"
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

func TestAdoptingAnEmptyRepositoryCarriesTheSecretsAcross(t *testing.T) {
	dataDir := t.TempDir()
	store := secrets.Open(secretsConfig(dataDir))
	if err := store.Set("db", "password", []byte("hunter2")); err != nil {
		t.Fatalf("setting a secret: %v", err)
	}

	if err := stateAdopt(context.Background(), dataDir, []string{"-remote", bareRemote(t)}); err != nil {
		t.Fatalf("adopting: %v", err)
	}

	// The working tree the adopt moved aside is not where this
	// deployment's secrets live now.
	got, err := secrets.Open(secretsConfig(dataDir)).Resolve(context.Background(), "db/password")
	if err != nil {
		t.Fatalf("resolving after the adopt: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("got %q", got)
	}
	// And it is in the repository, so it reaches the remote with
	// everything else rather than living only on this host.
	if _, err := os.Stat(filepath.Join(stateRepoDir(dataDir), secrets.DefaultFileName)); err != nil {
		t.Fatalf("the secrets file is not in the adopted repository: %v", err)
	}
}

// The restore: a repository written by another installation, adopted on
// a host that has never held its key.
func TestAdoptingAnotherInstallationNeedsItsKey(t *testing.T) {
	ctx := context.Background()
	remote := bareRemote(t)

	// The installation that owns the repository. Its key is what its
	// operator has to have kept.
	origin := t.TempDir()
	originStore := secrets.Open(secretsConfig(origin))
	if err := originStore.Set("db", "password", []byte("hunter2")); err != nil {
		t.Fatalf("setting a secret: %v", err)
	}
	if err := stateAdopt(ctx, origin, []string{"-remote", remote}); err != nil {
		t.Fatalf("seeding the remote: %v", err)
	}
	originPublic, err := originStore.PublicKey()
	if err != nil {
		t.Fatalf("reading the origin's public key: %v", err)
	}

	// The new host, which has never seen any of this.
	host := t.TempDir()
	if err := stateAdopt(ctx, host, []string{"-remote", remote}); err != nil {
		t.Fatalf("adopting: %v", err)
	}
	hostStore := secrets.Open(secretsConfig(host))
	if err := hostStore.Check(); err == nil {
		t.Fatal("a host with none of the origin's key material claimed it could read its secrets")
	}
	// No key was minted over the file, which would have made the secrets
	// permanently unreadable while looking like it had worked.
	if hostStore.KeyCreated() {
		t.Fatal("a fresh key was minted over an adopted secrets file")
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
	remote := bareRemote(t)
	origin := t.TempDir()
	if err := secrets.Open(secretsConfig(origin)).Set("db", "password", []byte("hunter2")); err != nil {
		t.Fatalf("setting a secret: %v", err)
	}
	if err := stateAdopt(ctx, origin, []string{"-remote", remote}); err != nil {
		t.Fatalf("seeding the remote: %v", err)
	}

	host := t.TempDir()
	if err := stateAdopt(ctx, host, []string{
		"-remote", remote, "-secrets-key-file", secretsConfig(origin).KeyFile,
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
	dataDir := t.TempDir()
	if err := secrets.Open(secretsConfig(dataDir)).Set("db", "password", []byte("hunter2")); err != nil {
		t.Fatalf("setting a secret: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(stateRepoDir(dataDir), secrets.DefaultFileName))
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
	after, err := os.ReadFile(filepath.Join(stateRepoDir(dataDir), secrets.DefaultFileName))
	if err != nil {
		t.Fatalf("the working tree was moved aside by a failed adopt: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("the secrets file changed under a failed adopt")
	}
}
