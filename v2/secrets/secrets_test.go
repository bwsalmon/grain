package secrets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeSecret writes one Kubernetes-style secret directory: dir/name/key
// for each key in data, holding its raw bytes -- the same shape kubelet
// writes for a mounted Secret volume.
func writeSecret(t *testing.T, root, name string, data map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for key, value := range data {
		if err := os.WriteFile(filepath.Join(dir, key), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveExplicitKeyInMultiKeySecret(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "db", map[string]string{"username": "app", "password": "hunter2"})
	store := New(dir)

	got, err := store.Resolve(context.Background(), "db/password")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hunter2" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveBareNameForSingleKeySecret(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "gcp-key-minter", map[string]string{"key.json": `{"type":"service_account"}`})
	store := New(dir)

	got, err := store.Resolve(context.Background(), "gcp-key-minter")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"type":"service_account"}` {
		t.Fatalf("got %q", got)
	}
}

func TestResolveBareNameForMultiKeySecretFailsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "db", map[string]string{"username": "app", "password": "hunter2"})
	store := New(dir)

	if _, err := store.Resolve(context.Background(), "db"); err == nil {
		t.Fatal("expected an error for an ambiguous bare name")
	}
}

func TestResolveMissingSecretFailsClosed(t *testing.T) {
	store := New(t.TempDir())
	if _, err := store.Resolve(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error for a missing secret")
	}
}

func TestResolveMissingKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "db", map[string]string{"username": "app"})
	store := New(dir)
	if _, err := store.Resolve(context.Background(), "db/password"); err == nil {
		t.Fatal("expected an error for a missing key")
	}
}

func TestResolveEmptyNameFailsClosed(t *testing.T) {
	store := New(t.TempDir())
	if _, err := store.Resolve(context.Background(), ""); err == nil {
		t.Fatal("expected an error for an empty name")
	}
}

func TestResolveDoesNotTrimValue(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "token", map[string]string{"value": "  padded\n"})
	store := New(dir)

	got, err := store.Resolve(context.Background(), "token/value")
	if err != nil {
		t.Fatal(err)
	}
	if got != "  padded\n" {
		t.Fatalf("got %q, want exact bytes preserved", got)
	}
}
