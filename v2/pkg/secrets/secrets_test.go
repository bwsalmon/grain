package secrets

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

// seedSecret writes every key in data into name via Store.Set -- the
// only way material ever gets into this package's database, now that it
// is not a directory tree a test can seed by writing files directly.
func seedSecret(t *testing.T, store *Store, name string, data map[string]string) {
	t.Helper()
	for key, value := range data {
		if err := store.Set(name, key, []byte(value)); err != nil {
			t.Fatalf("seeding %s/%s: %v", name, key, err)
		}
	}
}

func TestResolveExplicitKeyInMultiKeySecret(t *testing.T) {
	store := New(t.TempDir())
	seedSecret(t, store, "db", map[string]string{"username": "app", "password": "hunter2"})

	got, err := store.Resolve(context.Background(), "db/password")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hunter2" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveBareNameForSingleKeySecret(t *testing.T) {
	store := New(t.TempDir())
	seedSecret(t, store, "gcp-key-minter", map[string]string{"key.json": `{"type":"service_account"}`})

	got, err := store.Resolve(context.Background(), "gcp-key-minter")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"type":"service_account"}` {
		t.Fatalf("got %q", got)
	}
}

func TestResolveBareNameForMultiKeySecretFailsAmbiguous(t *testing.T) {
	store := New(t.TempDir())
	seedSecret(t, store, "db", map[string]string{"username": "app", "password": "hunter2"})

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
	store := New(t.TempDir())
	seedSecret(t, store, "db", map[string]string{"username": "app"})
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
	store := New(t.TempDir())
	seedSecret(t, store, "token", map[string]string{"value": "  padded\n"})

	got, err := store.Resolve(context.Background(), "token/value")
	if err != nil {
		t.Fatal(err)
	}
	if got != "  padded\n" {
		t.Fatalf("got %q, want exact bytes preserved", got)
	}
}

func TestResolveRejectsPathTraversalInKey(t *testing.T) {
	store := New(t.TempDir())
	seedSecret(t, store, "secret", map[string]string{"key": "value"})

	if _, err := store.Resolve(context.Background(), "secret/../../etc/passwd"); err == nil {
		t.Fatal("expected an error for a traversal attempt in the key")
	}
}

func TestListReportsNamesAndKeysNotValues(t *testing.T) {
	store := New(t.TempDir())
	seedSecret(t, store, "db", map[string]string{"username": "app", "password": "hunter2"})
	seedSecret(t, store, "token", map[string]string{"value": "abc"})

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

func TestListOnFreshDatabaseReturnsNoSecrets(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "does-not-exist-yet"))
	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

func TestSetThenResolve(t *testing.T) {
	store := New(t.TempDir())

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
	store := New(t.TempDir())
	seedSecret(t, store, "db", map[string]string{"password": "old"})

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
	store := New(t.TempDir())
	seedSecret(t, store, "db", map[string]string{"username": "app", "password": "hunter2"})

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

func TestDeleteKeyRemovesEmptySecretFromList(t *testing.T) {
	store := New(t.TempDir())
	seedSecret(t, store, "token", map[string]string{"value": "abc"})

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
	store := New(t.TempDir())
	seedSecret(t, store, "db", map[string]string{"username": "app"})
	if err := store.DeleteKey("db", "password"); err == nil {
		t.Fatal("expected an error for a missing key")
	}
}

func TestDeleteSecretRemovesEveryKey(t *testing.T) {
	store := New(t.TempDir())
	seedSecret(t, store, "db", map[string]string{"username": "app", "password": "hunter2"})

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
