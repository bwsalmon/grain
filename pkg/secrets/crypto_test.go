package secrets

// What the encryption is actually for: a file that can sit in a git
// repository anyone with the repository can read, and still be readable
// only by whoever holds the private key.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheFileOnDiskIsCiphertext(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := store.Set("db", "password", []byte("hunter2")); err != nil {
		t.Fatalf("setting: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, DefaultFileName))
	if err != nil {
		t.Fatalf("reading the secrets file: %v", err)
	}
	// The value must not appear anywhere in the file -- this is the whole
	// claim that lets the file be committed and pushed.
	if strings.Contains(string(data), "hunter2") {
		t.Fatalf("the value is readable in the file on disk:\n%s", data)
	}
	// Nor may the secret's own name leak: which credentials a deployment
	// holds is itself worth not publishing.
	if strings.Contains(string(data), "password") {
		t.Fatalf("a key name is readable in the file on disk:\n%s", data)
	}
	if !strings.HasPrefix(string(data), armourBegin) {
		t.Fatalf("the file is not in grain's armour:\n%s", data)
	}
}

func TestAnotherKeyCannotReadTheFile(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := store.Set("db", "password", []byte("hunter2")); err != nil {
		t.Fatalf("setting: %v", err)
	}
	// Somebody clones the repository and points their own key at it.
	other, err := GenerateKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	if err := WriteKeyFile(filepath.Join(dir, DefaultKeyFileName), other); err != nil {
		t.Fatalf("writing a different key: %v", err)
	}
	if _, err := New(dir).Resolve(context.Background(), "db/password"); err == nil {
		t.Fatal("a different key read the secret")
	} else if !strings.Contains(err.Error(), "different key") {
		t.Fatalf("the error does not say what is wrong: %v", err)
	}
}

func TestALostKeyIsReportedNotReplaced(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := store.Set("db", "password", []byte("hunter2")); err != nil {
		t.Fatalf("setting: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, DefaultKeyFileName)); err != nil {
		t.Fatalf("removing the key: %v", err)
	}
	// Minting a fresh key here would leave an undecryptable file behind
	// and look like it had worked.
	reopened := New(dir)
	if _, err := reopened.List(); err == nil {
		t.Fatal("a store with a lost key opened as if nothing were wrong")
	}
	if _, err := os.Stat(filepath.Join(dir, DefaultKeyFileName)); err == nil {
		t.Fatal("a new key was minted over an existing secrets file")
	}
}

func TestAFreshInstallMintsItsOwnKey(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if !store.KeyCreated() {
		t.Fatal("a fresh install did not mint a key, so it needs an operator to do it by hand")
	}
	pub, err := store.PublicKey()
	if err != nil {
		t.Fatalf("reading the public key: %v", err)
	}
	if !strings.HasPrefix(pub, pubPrefix) {
		t.Fatalf("the public key is not tagged as one: %q", pub)
	}
	// And a second open finds the key rather than minting another.
	if New(dir).KeyCreated() {
		t.Fatal("reopening minted a second key")
	}
}

func TestKeyRoundTripsThroughItsTextForm(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	// The operator's copy of the key is this string; it has to come back
	// as the same key, whitespace from a paste and all.
	back, err := ParseKey("  " + key.String() + "\n")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if back.String() != key.String() || back.Public() != key.Public() {
		t.Fatal("the key did not survive its own text form")
	}
	if strings.Contains(key.Public(), strings.TrimPrefix(key.String(), keyPrefix)) {
		t.Fatal("the public form carries the private material")
	}
}

func TestParseKeyRejectsRubbish(t *testing.T) {
	for _, in := range []string{"", "hunter2", keyPrefix + "not-base64!", keyPrefix + "c2hvcnQ="} {
		if _, err := ParseKey(in); err == nil {
			t.Fatalf("%q was accepted as a key", in)
		}
	}
}

func TestEncryptRoundTrip(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	plain := []byte("a value\nwith a newline and some \x00 bytes")
	sealed, err := Encrypt(key, plain)
	if err != nil {
		t.Fatalf("encrypting: %v", err)
	}
	got, err := Decrypt(key, sealed)
	if err != nil {
		t.Fatalf("decrypting: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("got %q, want %q", got, plain)
	}
	// Every write is a fresh ephemeral key, so two encryptions of the same
	// plaintext differ -- which is what makes the fixed nonce safe.
	again, err := Encrypt(key, plain)
	if err != nil {
		t.Fatalf("encrypting again: %v", err)
	}
	if string(again) == string(sealed) {
		t.Fatal("two encryptions of the same plaintext are identical")
	}
}

func TestDecryptRejectsATamperedFile(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	sealed, err := Encrypt(key, []byte("hunter2"))
	if err != nil {
		t.Fatalf("encrypting: %v", err)
	}
	// Flip one character of the body. AES-GCM authenticates, so this must
	// fail rather than yield different plaintext.
	lines := strings.Split(string(sealed), "\n")
	body := lines[4]
	if body[0] == 'A' {
		lines[4] = "B" + body[1:]
	} else {
		lines[4] = "A" + body[1:]
	}
	if _, err := Decrypt(key, []byte(strings.Join(lines, "\n"))); err == nil {
		t.Fatal("a tampered file decrypted")
	}
}
