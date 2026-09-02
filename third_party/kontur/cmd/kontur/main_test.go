package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/kontur/internal/config"
	"github.com/bwsalmon/kontur/internal/guestkey"
	"golang.org/x/crypto/ssh"
)

// authorizedKeyParam returns the authorized_keys line carried by cmdline,
// decoded, or "" if there is none.
func authorizedKeyParam(t *testing.T, cmdline string) string {
	t.Helper()
	for _, field := range strings.Fields(cmdline) {
		if !strings.HasPrefix(field, guestkey.AuthorizedKeyParam+"=") {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(decodeParam(t, field)))
		if err != nil {
			t.Fatalf("cmdline carried an unparseable key %q: %v", field, err)
		}
		return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	}
	return ""
}

func decodeParam(t *testing.T, field string) string {
	t.Helper()
	_, value, _ := strings.Cut(field, "=")
	decoded, err := base64Decode(value)
	if err != nil {
		t.Fatalf("decoding %q: %v", field, err)
	}
	return decoded
}

func publicHalf(t *testing.T, keyPath string) string {
	t.Helper()
	pem, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading %s: %v", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		t.Fatalf("parsing %s: %v", keyPath, err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

// A fresh boot generates a key and tells the guest to authorize it -- and
// the two have to be the same key, or the guest comes up unreachable.
func TestEnsureGuestKey_FreshBoot(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "exec_id_ed25519")
	cfg := config.Config{Cmdline: "console=ttyS0 root=/dev/vda rw"}

	if err := ensureGuestKey(&cfg, keyPath); err != nil {
		t.Fatalf("ensureGuestKey: %v", err)
	}

	if got, want := authorizedKeyParam(t, cfg.Cmdline), publicHalf(t, keyPath); got != want {
		t.Errorf("cmdline authorizes %q, but the key on disk is %q", got, want)
	}
	if !strings.HasPrefix(cfg.Cmdline, "console=ttyS0 root=/dev/vda rw ") {
		t.Errorf("cmdline = %q, want the original preserved", cfg.Cmdline)
	}
}

// With a snapshot configured, the key must also land somewhere that
// outlives this container -- see the restore case below for why.
func TestEnsureGuestKey_SavesKeyBesideSnapshot(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "container", "exec_id_ed25519")
	cfg := config.Config{SnapshotPath: filepath.Join(dir, "snap", "state")}
	if err := os.MkdirAll(filepath.Dir(cfg.SnapshotPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ensureGuestKey(&cfg, keyPath); err != nil {
		t.Fatalf("ensureGuestKey: %v", err)
	}

	saved := filepath.Join(dir, "snap", "exec_id_ed25519")
	if got, want := publicHalf(t, saved), publicHalf(t, keyPath); got != want {
		t.Errorf("saved key = %q, want the generated %q", got, want)
	}
}

// Resuming a snapshot never boots a kernel, so the guest never reads a
// command line and its authorized_keys still holds the key the suspended
// boot installed. Generating a new one here would lock this container out
// of the guest it just resumed -- the failure this test exists to catch.
func TestEnsureGuestKey_RestoreKeepsExistingKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "exec_id_ed25519")
	snapshot := filepath.Join(dir, "state")

	original, err := guestkey.Generate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{SnapshotPath: snapshot, Cmdline: "console=ttyS0 root=/dev/vda rw"}
	if err := ensureGuestKey(&cfg, keyPath); err != nil {
		t.Fatalf("ensureGuestKey: %v", err)
	}

	if got := publicHalf(t, keyPath); got != original {
		t.Errorf("key on disk = %q, want the pre-existing %q -- a restore must not rotate it", got, original)
	}
	if cfg.Cmdline != "console=ttyS0 root=/dev/vda rw" {
		t.Errorf("cmdline = %q, want it untouched: a restore passes no command line at all", cfg.Cmdline)
	}
}

// The case the save exists for: the snapshot is resumed by a container
// recreated since it was taken, so the key is gone from the writable layer
// and has to come back from beside the snapshot.
func TestEnsureGuestKey_RestoreRecoversSavedKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "container", "exec_id_ed25519")
	snapshot := filepath.Join(dir, "snap", "state")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
		t.Fatal(err)
	}

	// First boot: generate and save.
	first := config.Config{SnapshotPath: snapshot}
	if err := ensureGuestKey(&first, keyPath); err != nil {
		t.Fatal(err)
	}
	original := publicHalf(t, keyPath)

	// The container is replaced, taking its writable layer with it, and
	// the VM is resumed from the snapshot the first boot left.
	if err := os.RemoveAll(filepath.Dir(keyPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := config.Config{SnapshotPath: snapshot}
	if err := ensureGuestKey(&second, keyPath); err != nil {
		t.Fatalf("ensureGuestKey on the recreated container: %v", err)
	}
	if got := publicHalf(t, keyPath); got != original {
		t.Errorf("recovered key = %q, want the first boot's %q", got, original)
	}
}

// A restore with no key anywhere cannot reach the guest, and saying so is
// more use than a connection that times out much later.
func TestEnsureGuestKey_RestoreWithoutAnyKeyFails(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "state")
	if err := os.WriteFile(snapshot, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{SnapshotPath: snapshot}
	err := ensureGuestKey(&cfg, filepath.Join(dir, "missing", "exec_id_ed25519"))
	if err == nil {
		t.Fatal("ensureGuestKey = nil, want an error naming the missing key")
	}
	if !strings.Contains(err.Error(), "exec_id_ed25519") {
		t.Errorf("error = %q, want it to name the key it could not find", err)
	}
}

func TestEnsureGuestKey_AuthorizesGuestUser(t *testing.T) {
	t.Setenv("KONTUR_EXEC_USER", "debian")
	cfg := config.Config{}
	if err := ensureGuestKey(&cfg, filepath.Join(t.TempDir(), "key")); err != nil {
		t.Fatalf("ensureGuestKey: %v", err)
	}
	if !strings.Contains(cfg.Cmdline, guestkey.AuthorizedKeyUserParam+"=debian") {
		t.Errorf("cmdline = %q, want it to name the guest account to authorize", cfg.Cmdline)
	}
}

func base64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	return string(b), err
}
