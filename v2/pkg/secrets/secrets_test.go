package secrets

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

func TestResolveRejectsPathTraversalInKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(t.TempDir(), "outside"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSecret(t, dir, "secret", map[string]string{"key": "value"})
	store := New(dir)

	if _, err := store.Resolve(context.Background(), "secret/../../etc/passwd"); err == nil {
		t.Fatal("expected an error for a traversal attempt in the key")
	}
}

func TestListReportsNamesAndKeysNotValues(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "db", map[string]string{"username": "app", "password": "hunter2"})
	writeSecret(t, dir, "token", map[string]string{"value": "abc"})
	store := New(dir)

	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []SecretInfo{
		{Name: "db", Keys: []string{"password", "username"}},
		{Name: "token", Keys: []string{"value"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestListOnMissingDirReturnsNoSecrets(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "does-not-exist"))
	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

func TestSetThenResolve(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)

	if err := store.Set("github", "token", []byte("ghp_abc123")); err != nil {
		t.Fatal(err)
	}
	got, err := store.Resolve(context.Background(), "github/token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ghp_abc123" {
		t.Fatalf("got %q", got)
	}
}

func TestSetOverwritesExistingValue(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "db", map[string]string{"password": "old"})
	store := New(dir)

	if err := store.Set("db", "password", []byte("new")); err != nil {
		t.Fatal(err)
	}
	got, err := store.Resolve(context.Background(), "db/password")
	if err != nil {
		t.Fatal(err)
	}
	if got != "new" {
		t.Fatalf("got %q, want overwritten value", got)
	}
}

func TestSetRejectsTraversalNames(t *testing.T) {
	store := New(t.TempDir())
	if err := store.Set("../escape", "key", []byte("x")); err == nil {
		t.Fatal("expected an error for a traversal secret name")
	}
	if err := store.Set("secret", "../escape", []byte("x")); err == nil {
		t.Fatal("expected an error for a traversal key name")
	}
}

func TestDeleteKeyRemovesJustThatKey(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "db", map[string]string{"username": "app", "password": "hunter2"})
	store := New(dir)

	if err := store.DeleteKey("db", "password"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(context.Background(), "db/password"); err == nil {
		t.Fatal("expected the deleted key to be gone")
	}
	got, err := store.Resolve(context.Background(), "db/username")
	if err != nil || got != "app" {
		t.Fatalf("username should still resolve: %q, %v", got, err)
	}
}

func TestDeleteKeyRemovesEmptySecretDirectory(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "token", map[string]string{"value": "abc"})
	store := New(dir)

	if err := store.DeleteKey("token", "value"); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("got %+v, want the now-empty secret gone from List", list)
	}
}

func TestDeleteKeyMissingFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "db", map[string]string{"username": "app"})
	store := New(dir)
	if err := store.DeleteKey("db", "password"); err == nil {
		t.Fatal("expected an error for a missing key")
	}
}

func TestDeleteSecretRemovesEveryKey(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, dir, "db", map[string]string{"username": "app", "password": "hunter2"})
	store := New(dir)

	if err := store.DeleteSecret("db"); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("got %+v, want no secrets left", list)
	}
}

func TestDeleteSecretMissingFailsClosed(t *testing.T) {
	store := New(t.TempDir())
	if err := store.DeleteSecret("nope"); err == nil {
		t.Fatal("expected an error for a missing secret")
	}
}
