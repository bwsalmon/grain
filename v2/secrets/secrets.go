// Package secrets is the extension point bwsalmon/agents#240 asks for: a
// model.CredentialResolver backed by a directory laid out the way
// Kubernetes mounts a Secret as a volume --
// https://kubernetes.io/docs/concepts/configuration/secret/#using-secrets-as-files-from-a-pod
// -- one entry per secret, the entry's name the secret's name and its
// content the material. That shape is what a real cluster already
// produces for any Secret projected as a volume, kubelet's own atomic
// writer included (a "..data" symlink to a timestamped directory, and
// each key a symlink into it), so a deployment that runs on Kubernetes
// hands this package a volume mount and nothing else, and a deployment
// that does not can build the same directory by hand -- one flat file per
// secret -- with no format of this package's own invention to keep in
// sync.
//
// This is deliberately the same shape grain/proxy/credentials.py's
// CredentialSet already reads (a flat directory, one file per name), not
// a new one: the difference is format compatibility with a real Secret
// volume (skipping kubelet's "..*" bookkeeping entries, following the
// symlinks it writes) rather than the `<name>.token` filename convention
// that package chose for itself.
//
// Store is one implementation of model.CredentialResolver, which is the
// actual extension point (model/capability.go): a provider is handed a
// CredentialResolver, never a concrete Store, so a deployment that wants
// secrets from somewhere other than a local directory -- a cloud secret
// manager, say -- implements the same three-method interface and nothing
// here has to change.
package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bwsalmon/grain/v2/model"
)

// Store implements model.CredentialResolver. Asserted here rather than
// left to a caller's assignment to fail on, since that assignment does
// not exist anywhere in this package for the compiler to check.
var _ model.CredentialResolver = (*Store)(nil)

// Store resolves a secret's name to its material, read once from a
// directory at construction.
//
// Loaded once, not hot-reloaded -- docs/data-model.md's existing rule for
// grain/proxy/credentials.py's CredentialSet applies here unchanged:
// rotation is "replace a file and restart the one service that reads
// it," so a live directory watch would add a cache-invalidation path
// nothing asks for. A Store is therefore safe for concurrent Resolve
// calls with no locking of its own: nothing after Open ever writes to
// its map.
type Store struct {
	values map[string]string
}

// Open reads every secret out of dir as it looks at the moment of the
// call. dir is expected in Kubernetes's own Secret-volume shape: each
// top-level entry that is a regular file, or a symlink to one (exactly
// what kubelet's atomic writer produces for a mounted Secret), is a
// secret named after that entry; entries whose name starts with ".." --
// kubelet's own "..data" and "..<timestamp>" bookkeeping -- are never
// secrets and are skipped, and neither is any other subdirectory, since a
// Secret volume never nests one.
func Open(dir string) (*Store, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("secrets: reading %s: %w", dir, err)
	}
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "..") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(path) // follows a kubelet-style symlink
		if err != nil {
			return nil, fmt.Errorf("secrets: reading %s: %w", name, err)
		}
		if info.IsDir() {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("secrets: reading %s: %w", name, err)
		}
		// Trimmed the same way grain/proxy/credentials.py's `.token`
		// files are: a value authored by hand in an editor almost always
		// carries a trailing newline that is never part of the secret
		// itself.
		values[name] = strings.TrimRight(string(content), "\n")
	}
	return &Store{values: values}, nil
}

// Resolve implements model.CredentialResolver: name is a secret's name --
// the name of its entry in the directory Open read -- resolved to its
// content. An unknown name is an error, not an empty string, so a
// provider that names a secret nobody configured fails loudly at the
// point of use rather than authenticating with "".
func (s *Store) Resolve(ctx context.Context, name string) (string, error) {
	v, ok := s.values[name]
	if !ok {
		return "", fmt.Errorf("secrets: no secret named %q", name)
	}
	return v, nil
}

// Names reports every secret Open found, sorted -- for checking a set of
// CapabilitySpec.RequiredSecrets is satisfied before anything tries to
// resolve one, without granting the checker Resolve's own access to the
// material itself.
func (s *Store) Names() []string {
	out := make([]string, 0, len(s.values))
	for name := range s.values {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Missing filters names down to the ones Store has no secret for -- the
// check a deployment runs against a CapabilityRegistry's own
// RequiredSecrets() before granting anything, so an operator sees every
// absent secret in one pass rather than one Resolve failure at a time.
func (s *Store) Missing(names []string) []string {
	var missing []string
	for _, name := range names {
		if _, ok := s.values[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}
