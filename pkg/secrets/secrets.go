// Package secrets is a model.CredentialResolver backed by a single
// encrypted file, which lives in grain's state repository
// (pkg/staterepo) beside everything else grain knows.
//
// It used to be a SQLite database of its own, deliberately separate from
// the task store so that an operator could back up, ship or lock down
// their credentials without also handling every task grain had ever
// filed. The separation stays, and is now stronger rather than weaker:
// the secrets are the one thing in the state repository that is
// ciphertext, encrypted to a public key whose private half grain reads
// from one file the operator manages and never copies anywhere else --
// not into the repository, and not into any backup of it. Cloning the
// repository gets you everything grain knows and none of what it can
// authenticate as.
//
// docs/data-model.md's "no secret store in the model" governs the task
// model (model): a Task, a Grant, a Lease never carry material, only
// a CredentialRef naming one. This package is where the name actually
// resolves -- read fresh off disk on every call, matching
// gitproxy/credentials.go's "replace a file and restart the one
// service that reads it".
//
// Agents do not read this file and cannot. A run gets a secret only
// through the secret input a human asked for, exactly as before: nothing
// here is exposed to a sandbox, and the private key never leaves the
// host's data directory.
//
// Set, DeleteKey and DeleteSecret exist for a different caller
// (bwsalmon/agents#357): a UI or a CLI running on the same host as the
// server, which the file is local disk to, and which therefore needs no
// daemon RPC to edit it. List reports which secrets and keys currently
// exist, never their values, so a caller can render what is set without
// ever being able to read one back through this package.
package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// The well-known names an agent framework's own credential is stored
// under -- one per framework, resolved by cmd/grain's daemon before every
// dispatch and pkg/ui's Settings pane writes (its "Agent frameworks"
// section) and reports the presence of. Named here, in the one package
// both sides already depend on, so neither has to repeat a string the
// other has to match exactly for a key pasted into the UI to be the key
// a run authenticates with.
//
// Each holds a single key, AgentCredentialKey, so a caller resolving one
// can name the bare secret ("gemini-api-key") and let Resolve's
// sole-key form find it.
const (
	// GeminiAPIKeySecret holds the Gemini API key agent/gemini's own
	// client is built with -- the daemon's operating key, distinct from
	// the short-lived keys the gemini-key capability mints per task.
	GeminiAPIKeySecret = "gemini-api-key"
	// ClaudeOAuthTokenSecret holds the Claude Code OAuth token
	// agent/claude passes its subprocess as CLAUDE_CODE_OAUTH_TOKEN.
	ClaudeOAuthTokenSecret = "claude-oauth-token"
	// OpenAIAPIKeySecret holds the OpenAI API key agent/codex passes its
	// subprocess as OPENAI_API_KEY. Named for the credential rather than
	// for the framework, the way the other two are: it is an OpenAI
	// account's key, and a second framework built on one would want this
	// same secret rather than a copy of it under another name.
	OpenAIAPIKeySecret = "openai-api-key"
	// AgentCredentialKey is the one key each of the secrets above holds.
	// "value" rather than a per-secret name ("api-key", "token")
	// so nothing has to remember which is which: the secret's own name
	// already says what the value is.
	AgentCredentialKey = "value"
)

// DefaultFileName is what the encrypted file is called inside the state
// repository, and DefaultKeyFileName what the private key is called in
// the data directory. Spelled out here so cmd/grain and pkg/staterepo's
// .gitignore agree with this package without repeating a literal.
const (
	DefaultFileName    = "secrets.enc"
	DefaultKeyFileName = "secrets.key"
)

// Config says where the two files are.
type Config struct {
	// File is the encrypted secrets file. It belongs in the state
	// repository's working tree, where grain's sync commits and pushes it
	// along with everything else.
	File string
	// KeyFile is the private key. It belongs outside the repository --
	// under the data directory -- and is the operator's to manage, back
	// up and, if they choose, keep only on a machine that is not this
	// one (in which case grain cannot start; there is no half state where
	// it runs without being able to read its own secrets).
	KeyFile string
}

// Store resolves a name to secret material held in an encrypted file.
// The zero value is not useful; build one with Open or New.
type Store struct {
	cfg     Config
	openErr error // set instead of a usable store when Open could not prepare one
	// created records that Open minted the key itself, which the daemon
	// reports once at startup: a key nobody has backed up yet is the one
	// thing about a fresh install that is urgent.
	created bool
	// mu serialises the read-modify-write a Set or a Delete is. The
	// daemon is the only writer (cmd/grain's own note on the UI and the
	// CLI reaching it over REST), but its own handlers are concurrent.
	mu sync.Mutex
}

// Open prepares the store, minting a key if this is a fresh install.
//
// "Fresh" means precisely: no key file and no secrets file. A key file
// that is missing while a secrets file exists is never quietly replaced
// -- that is an operator who has lost their key, the secrets are
// unrecoverable, and saying so beats starting over with a new key and
// leaving an undecryptable file in the repository for grain to trip over
// later.
//
// Minting without being asked is what lets a local-only install need no
// input at all: `grain daemon` on a laptop comes up with working secret
// storage and a key on disk, and the operator can go and put that key
// somewhere safe whenever they like.
func Open(cfg Config) *Store {
	s := &Store{cfg: cfg}
	if cfg.File == "" || cfg.KeyFile == "" {
		s.openErr = errors.New("secrets: both a file and a key file are required")
		return s
	}
	_, err := ReadKeyFile(cfg.KeyFile)
	switch {
	case err == nil:
	case errors.Is(err, ErrNoKey):
		if fileExists(cfg.File) {
			s.openErr = fmt.Errorf(
				"secrets: %s holds encrypted secrets but %s is missing -- "+
					"restore that key file, or delete the secrets file to start over",
				cfg.File, cfg.KeyFile)
			return s
		}
		key, genErr := GenerateKey()
		if genErr != nil {
			s.openErr = genErr
			return s
		}
		if writeErr := WriteKeyFile(cfg.KeyFile, key); writeErr != nil {
			s.openErr = writeErr
			return s
		}
		s.created = true
	default:
		s.openErr = err
	}
	return s
}

// New opens a store whose two files sit in one directory, which is what
// a test wants and what a single-directory install gets. A deployment
// uses Open instead, because the point of this package's layout is that
// the two files live in different places: the encrypted one inside the
// state repository, the key outside it.
func New(dir string) *Store {
	return Open(Config{
		File:    filepath.Join(dir, DefaultFileName),
		KeyFile: filepath.Join(dir, DefaultKeyFileName),
	})
}

// KeyCreated reports whether Open minted the key rather than finding it.
func (s *Store) KeyCreated() bool { return s.created }

// KeyFile is where the private key is read from, for a caller that has
// to tell an operator which file to go and back up.
func (s *Store) KeyFile() string { return s.cfg.KeyFile }

// File is the encrypted file this store reads and writes.
func (s *Store) File() string { return s.cfg.File }

// PublicKey renders the public half of the key, which is safe to display
// and is what the bootstrap shows an operator so they can confirm the
// key they hold is the key this deployment encrypts to.
func (s *Store) PublicKey() (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	key, err := ReadKeyFile(s.cfg.KeyFile)
	if err != nil {
		return "", err
	}
	return key.Public(), nil
}

func (s *Store) ready() error {
	if s.openErr != nil {
		return s.openErr
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// contents is the plaintext under the ciphertext: every secret, every
// key, every value. Values are base64 because a value is arbitrary bytes
// -- a PEM, a binary token -- and JSON strings are not.
type contents struct {
	Version int                          `json:"version"`
	Secrets map[string]map[string]string `json:"secrets"`
}

// load decrypts the file. A file that is not there is an empty store,
// not an error: "missing is empty, not broken" is the same answer
// Resolve gives for one name at a time, and it is what makes a fresh
// install work before anything has been set.
func (s *Store) load() (*contents, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.cfg.File)
	if os.IsNotExist(err) {
		return &contents{Version: 1, Secrets: map[string]map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: reading %s: %w", s.cfg.File, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return &contents{Version: 1, Secrets: map[string]map[string]string{}}, nil
	}
	key, err := ReadKeyFile(s.cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	plain, err := Decrypt(key, data)
	if err != nil {
		return nil, err
	}
	var c contents
	if err := json.Unmarshal(plain, &c); err != nil {
		return nil, fmt.Errorf("secrets: %s decrypted to something that is not a secrets file: %w", s.cfg.File, err)
	}
	if c.Secrets == nil {
		c.Secrets = map[string]map[string]string{}
	}
	return &c, nil
}

// save encrypts and writes the file atomically, 0600. The write goes
// through a temporary file in the same directory so a reader -- or a git
// commit racing the daemon's own sync -- never sees a half-written
// ciphertext, which would decrypt to nothing at all.
func (s *Store) save(c *contents) error {
	key, err := ReadKeyFile(s.cfg.KeyFile)
	if err != nil {
		return err
	}
	c.Version = 1
	plain, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	sealed, err := Encrypt(key, plain)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.cfg.File)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secrets: preparing %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".secrets-*")
	if err != nil {
		return fmt.Errorf("secrets: creating a temporary file next to %s: %w", s.cfg.File, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(sealed); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.cfg.File)
}

// Resolve reads one secret by name. "<secret>/<key>" names one key in
// one secret directly. "<secret>" alone resolves only if that secret has
// exactly one key, so a capability naming a single-value secret (a
// token, a minted key) does not also have to know and repeat its key's
// name.
//
// The returned value is the key's exact bytes, with no trimming: a
// value that legitimately ends in whitespace round-trips unchanged.
func (s *Store) Resolve(ctx context.Context, name string) (string, error) {
	secret, key, explicit := strings.Cut(name, "/")
	if !validComponent(secret) {
		return "", fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	if explicit && !validComponent(key) {
		return "", fmt.Errorf("secrets: %q names no valid key after %q/", name, secret)
	}
	c, err := s.load()
	if err != nil {
		return "", err
	}
	keys, ok := c.Secrets[secret]
	if !ok || len(keys) == 0 {
		return "", fmt.Errorf("secrets: no secret named %q", secret)
	}
	if !explicit {
		if len(keys) != 1 {
			names := make([]string, 0, len(keys))
			for k := range keys {
				names = append(names, k)
			}
			sort.Strings(names)
			return "", fmt.Errorf(
				"secrets: secret %q has %d keys, name one explicitly as %q",
				secret, len(keys), secret+"/<key>")
		}
		for k := range keys {
			key = k
		}
	}
	encoded, ok := keys[key]
	if !ok {
		return "", fmt.Errorf("secrets: no key %q in secret %q", key, secret)
	}
	value, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("secrets: %q in secret %q is corrupt: %w", key, secret, err)
	}
	return string(value), nil
}

// SecretInfo names one secret and the keys it currently holds -- never
// their values. This is List's whole return shape, deliberately:
// bwsalmon/agents#357 asks that a colocated UI or CLI be able to tell
// which secrets are set without ever being able to read one's value back
// through this package.
type SecretInfo struct {
	Name string   `json:"name"`
	Keys []string `json:"keys"`
}

// List reports every secret currently stored and the keys each one
// holds, sorted by name (and each one's keys sorted too) for a stable,
// diffable rendering. A store with nothing ever Set reports no secrets
// rather than an error, the same "missing is empty, not broken" Resolve
// already gives one name at a time.
func (s *Store) List() ([]SecretInfo, error) {
	c, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []SecretInfo
	for name, keys := range c.Secrets {
		info := SecretInfo{Name: name}
		for k := range keys {
			info.Keys = append(info.Keys, k)
		}
		sort.Strings(info.Keys)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Set writes one key's value. An existing key is overwritten outright --
// there is no versioning here, the same "replace a file" shape
// gitproxy/credentials.go already gives the GitHub credential ladder.
func (s *Store) Set(secret, key string, value []byte) error {
	if !validComponent(secret) {
		return fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	if !validComponent(key) {
		return fmt.Errorf("secrets: %q is not a valid key name", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load()
	if err != nil {
		return err
	}
	if c.Secrets[secret] == nil {
		c.Secrets[secret] = map[string]string{}
	}
	c.Secrets[secret][key] = base64.StdEncoding.EncodeToString(value)
	return s.save(c)
}

// DeleteKey removes one key from a secret.
func (s *Store) DeleteKey(secret, key string) error {
	if !validComponent(secret) {
		return fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	if !validComponent(key) {
		return fmt.Errorf("secrets: %q is not a valid key name", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := c.Secrets[secret][key]; !ok {
		return fmt.Errorf("secrets: no key %q in secret %q", key, secret)
	}
	delete(c.Secrets[secret], key)
	if len(c.Secrets[secret]) == 0 {
		delete(c.Secrets, secret)
	}
	return s.save(c)
}

// DeleteSecret removes a secret and every key in it.
func (s *Store) DeleteSecret(secret string) error {
	if !validComponent(secret) {
		return fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load()
	if err != nil {
		return err
	}
	if len(c.Secrets[secret]) == 0 {
		return fmt.Errorf("secrets: no secret named %q", secret)
	}
	delete(c.Secrets, secret)
	return s.save(c)
}

// validComponent reports whether s is safe to use as a secret or key
// name: non-empty, not "." or "..", and free of path separators. Nothing
// here puts a name on disk directly the way the old file-tree store did,
// but keeping the same restriction means a name that was invalid before
// this package moved off a file tree stays invalid now, rather than
// quietly starting to work.
func validComponent(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\")
}
