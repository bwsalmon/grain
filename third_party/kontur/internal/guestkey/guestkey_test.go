package guestkey

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The property the whole scheme rests on: the private key left on disk
// for "kontur exec" and the authorized_keys line handed to the guest are
// two halves of one keypair. Nothing else in this package matters if a
// guest authorizes a key the container cannot present.
func TestGenerate_HalvesMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "exec_id_ed25519")

	authorized, err := Generate(path)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	pem, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the private key back: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		t.Fatalf("parsing the private key: %v", err)
	}

	got := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if got != authorized {
		t.Errorf("private key's public half = %q, want the returned line %q", got, authorized)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized)); err != nil {
		t.Errorf("returned line does not parse as an authorized_keys entry: %v", err)
	}
}

// sshd refuses a private key any other account can read, so the mode is
// part of the contract rather than a detail.
func TestGenerate_PrivateKeyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exec_id_ed25519")
	if _, err := Generate(path); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

// A second boot must not append to the first boot's file: an OpenSSH PEM
// with a second PEM stuck to the end of it does not parse.
func TestGenerate_ReplacesExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exec_id_ed25519")
	if _, err := Generate(path); err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	second, err := Generate(path)
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the private key back: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		t.Fatalf("parsing the private key after a second Generate: %v", err)
	}
	if got := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))); got != second {
		t.Errorf("on-disk key = %q, want the second Generate's %q", got, second)
	}
}

func TestWithParams(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3Nz kontur-exec"

	got := WithParams("console=ttyS0 root=/dev/vda rw", key, "debian")
	wantKey := AuthorizedKeyParam + "=" + base64.StdEncoding.EncodeToString([]byte(key))
	if !strings.Contains(got, wantKey) {
		t.Errorf("cmdline = %q, want it to contain %q", got, wantKey)
	}
	if !strings.Contains(got, AuthorizedKeyUserParam+"=debian") {
		t.Errorf("cmdline = %q, want it to carry the user parameter", got)
	}
	if !strings.HasPrefix(got, "console=ttyS0 root=/dev/vda rw ") {
		t.Errorf("cmdline = %q, want the original preserved as a prefix", got)
	}
	// The key is base64 precisely so it survives as one space-separated
	// field; a regression here would hand the guest a truncated key.
	for _, field := range strings.Fields(got) {
		if strings.HasPrefix(field, AuthorizedKeyParam+"=") {
			if field != wantKey {
				t.Errorf("key field = %q, want %q", field, wantKey)
			}
		}
	}
}

func TestWithParams_NoUser(t *testing.T) {
	got := WithParams("", "ssh-ed25519 AAAAC3Nz", "")
	if strings.Contains(got, AuthorizedKeyUserParam) {
		t.Errorf("cmdline = %q, want no user parameter when none was given", got)
	}
}

func TestWithParams_KeepsExplicitValue(t *testing.T) {
	existing := AuthorizedKeyParam + "=already"
	if got := WithParams(existing, "ssh-ed25519 AAAAC3Nz", ""); got != existing {
		t.Errorf("cmdline = %q, want %q left alone", got, existing)
	}
}
