// Package secrets is a model.CredentialResolver backed by a directory
// shaped like a Kubernetes Secret volume mount: one subdirectory per
// secret, named for the Secret's own name, holding one file per key,
// named for the key, holding that key's raw value -- exactly what
// kubelet writes under a Pod's `volumes: - secret: {secretName: ...}`,
// so an operator points this at a real mounted Secret volume (or a
// directory shaped like one, for anything smaller) with no translation
// step of their own.
//
// docs/data-model.md's "no secret store in the model" governs the task
// model (v2/model): a Task, a Grant, a Lease never carry material, only
// a CredentialRef naming one. This package is where the name actually
// resolves -- read fresh off disk on every call, matching
// v2/gitproxy/credentials.go's "replace a file and restart the one
// service that reads it," and never written back to anything by the
// daemon itself.
//
// Set, DeleteKey and DeleteSecret exist for a different caller
// (bwsalmon/agents#357): a UI or a CLI running on the same host as the
// server, which the store's own directory is local disk to, and which
// therefore needs no daemon RPC to edit it -- it just changes the files
// directly, the same way the daemon reads them fresh on every Resolve.
// List reports which secrets and keys currently exist, never their
// values, so a caller can render what is set without ever being able to
// read one back through this package.
package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store resolves a name to secret material read from Dir. The zero value
// is not useful; build one with New.
type Store struct {
	dir string
}

// New builds a Store rooted at dir. dir need not exist yet -- Resolve
// simply fails closed for any name it can't find, the same "missing
// config parks the task" rule docs/data-model.md gives every other named
// credential.
func New(dir string) *Store {
	return &Store{dir: dir}
}

// Resolve reads one secret by name. "<secret>/<key>" names one key in
// one secret directly, matching Kubernetes' own secretKeyRef shape.
// "<secret>" alone resolves only if that secret has exactly one key, so
// a capability naming a single-value secret (a token, a minted key) does
// not also have to know and repeat its key's filename.
//
// The returned value is the key's exact bytes, with no trimming: unlike
// a hand-edited `<name>.token` file, a Kubernetes Secret's data carries
// no trailing-newline convention, and trimming here would silently
// corrupt any value that legitimately ends in whitespace.
func (s *Store) Resolve(ctx context.Context, name string) (string, error) {
	secret, key, explicit := strings.Cut(name, "/")
	if !validComponent(secret) {
		return "", fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	dir := filepath.Join(s.dir, secret)
	if explicit {
		if !validComponent(key) {
			return "", fmt.Errorf("secrets: %q names no valid key after %q/", name, secret)
		}
		return readKey(dir, secret, key)
	}
	return s.resolveSoleKey(secret, dir)
}

// resolveSoleKey is the "<secret>" form: it succeeds only when dir holds
// exactly one key, since anything else is an ambiguous name -- Resolve's
// caller must say which key it means.
func (s *Store) resolveSoleKey(secret, dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("secrets: no secret named %q", secret)
		}
		return "", fmt.Errorf("secrets: reading secret %q: %w", secret, err)
	}
	var keys []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			keys = append(keys, e.Name())
		}
	}
	if len(keys) != 1 {
		return "", fmt.Errorf(
			"secrets: secret %q has %d keys, name one explicitly as %q",
			secret, len(keys), secret+"/<key>",
		)
	}
	return readKey(dir, secret, keys[0])
}

func readKey(dir, secret, key string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, key))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("secrets: no key %q in secret %q", key, secret)
		}
		return "", fmt.Errorf("secrets: reading %q in secret %q: %w", key, secret, err)
	}
	return string(data), nil
}

// SecretInfo names one secret on disk and the keys it currently holds --
// never their values. This is List's whole return shape, deliberately:
// bwsalmon/agents#357 asks that a colocated UI or CLI be able to tell
// which secrets are set without ever being able to read one's value back
// through this package.
type SecretInfo struct {
	Name string   `json:"name"`
	Keys []string `json:"keys"`
}

// List reports every secret currently on disk and the keys each one
// holds, sorted by name (and each one's keys sorted too) for a stable,
// diffable rendering. A Dir that does not exist yet -- nothing has ever
// been Set -- reports no secrets rather than an error, the same "missing
// is empty, not broken" Resolve already gives one name at a time.
func (s *Store) List() ([]SecretInfo, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("secrets: listing %q: %w", s.dir, err)
	}
	out := make([]SecretInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		keyEntries, err := os.ReadDir(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("secrets: listing keys in %q: %w", e.Name(), err)
		}
		info := SecretInfo{Name: e.Name()}
		for _, k := range keyEntries {
			if k.Type().IsRegular() {
				info.Keys = append(info.Keys, k.Name())
			}
		}
		sort.Strings(info.Keys)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Set writes one key's value, creating its secret's directory (mode
// 0700) if this is the first key ever written into it. An existing key
// is overwritten outright -- there is no versioning here, the same
// "replace a file" shape v2/gitproxy/credentials.go already gives the
// GitHub credential ladder.
func (s *Store) Set(secret, key string, value []byte) error {
	if !validComponent(secret) {
		return fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	if !validComponent(key) {
		return fmt.Errorf("secrets: %q is not a valid key name", key)
	}
	dir := filepath.Join(s.dir, secret)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secrets: creating secret %q: %w", secret, err)
	}
	if err := os.WriteFile(filepath.Join(dir, key), value, 0o600); err != nil {
		return fmt.Errorf("secrets: writing %q in secret %q: %w", key, secret, err)
	}
	return nil
}

// DeleteKey removes one key from a secret. If that was the secret's last
// key, the now-empty secret directory is removed too, so List stops
// reporting a secret with nothing left in it.
func (s *Store) DeleteKey(secret, key string) error {
	if !validComponent(secret) {
		return fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	if !validComponent(key) {
		return fmt.Errorf("secrets: %q is not a valid key name", key)
	}
	dir := filepath.Join(s.dir, secret)
	if err := os.Remove(filepath.Join(dir, key)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("secrets: no key %q in secret %q", key, secret)
		}
		return fmt.Errorf("secrets: deleting %q in secret %q: %w", key, secret, err)
	}
	removeIfEmpty(dir)
	return nil
}

// DeleteSecret removes a secret and every key in it.
func (s *Store) DeleteSecret(secret string) error {
	if !validComponent(secret) {
		return fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	dir := filepath.Join(s.dir, secret)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("secrets: no secret named %q", secret)
		}
		return fmt.Errorf("secrets: reading secret %q: %w", secret, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("secrets: deleting secret %q: %w", secret, err)
	}
	return nil
}

// removeIfEmpty best-effort removes dir if it holds nothing -- leaving it
// behind on any error (permissions, a concurrent writer) is harmless,
// since List only reports a secret by the keys it finds inside.
func removeIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}

// validComponent reports whether s is safe to use as one path segment
// under Store's own directory: non-empty, not "." or "..", and free of
// path separators. Resolve always enforced this on the secret half of a
// name; Set and DeleteKey/DeleteSecret now put a caller's own string on
// disk instead of only reading one back, so every name -- secret or key
// -- is checked the same way, closing what used to be a path-traversal
// name like "../../etc" or "sub/../../escape" reaching outside dir.
func validComponent(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\")
}
