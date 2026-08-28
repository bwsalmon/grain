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
// service that reads it," and never written back to anything.
package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	if secret == "" {
		return "", fmt.Errorf("secrets: empty secret name")
	}
	dir := filepath.Join(s.dir, secret)
	if explicit {
		if key == "" {
			return "", fmt.Errorf("secrets: %q names no key after %q/", name, secret)
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
