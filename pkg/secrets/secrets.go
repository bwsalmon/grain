// Package secrets is a model.CredentialResolver backed by its own
// embedded SQLite database, separate from the task/config store
// pkg/model/sqlite opens (bwsalmon/agents#366's "put secrets in a
// separate db, config and tasks in a common db") -- one file an operator
// can back up, ship, or lock down on its own, without also handling
// every task grain has ever filed.
//
// docs/data-model.md's "no secret store in the model" governs the task
// model (model): a Task, a Grant, a Lease never carry material, only
// a CredentialRef naming one. This package is where the name actually
// resolves -- read fresh off the database on every call, matching
// gitproxy/credentials.go's "replace a file and restart the one
// service that reads it," and never written back to anything by the
// daemon itself.
//
// Set, DeleteKey and DeleteSecret exist for a different caller
// (bwsalmon/agents#357): a UI or a CLI running on the same host as the
// server, which the store's own file is local disk to, and which
// therefore needs no daemon RPC to edit it -- it just opens the same
// database file directly, the same way the daemon reads it fresh on
// every Resolve. List reports which secrets and keys currently exist,
// never their values, so a caller can render what is set without ever
// being able to read one back through this package.
package secrets

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/bwsalmon/grain/pkg/model/sqlite"
)

// The well-known names an agent framework's own credential is stored
// under -- the two secrets cmd/grain's daemon resolves before every
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
	// AgentCredentialKey is the one key each of the two secrets above
	// holds. "value" rather than a per-secret name ("api-key", "token")
	// so nothing has to remember which is which: the secret's own name
	// already says what the value is.
	AgentCredentialKey = "value"
)

// Store resolves a name to secret material held in a SQLite database
// under dir. The zero value is not useful; build one with New.
type Store struct {
	db      *sql.DB
	openErr error // set instead of db when New could not open/prepare the database
}

// New opens (creating if absent) the secrets database under dir --
// dir/secrets.db, sitting alongside but never inside the task/config
// store dir/grain.db that pkg/model/sqlite opens. dir need not exist
// yet.
func New(dir string) *Store {
	db, err := sqlite.Open(sqlite.Config{Dir: dir, Name: "secrets"})
	if err != nil {
		// New has always been infallible -- every caller in this repo
		// constructs a Store at startup, before a context or an error
		// path to report through exists yet (cmd/grain's daemon.go,
		// ui.go and secrets.go all call it inline while building a
		// config struct). init deliberately does the same: the schema is
		// two small tables, and a directory New can create but cannot
		// then open into a working database is the same class of
		// unrecoverable startup failure -- disk full, permissions -- as
		// pkg/model/sqlite.Open failing for the main store, where the
		// caller does check that error today. Resolving into that error
		// here, rather than changing this signature, keeps every one of
		// those call sites unchanged; the error surfaces on the first
		// real use instead of at construction.
		return &Store{db: nil, openErr: err}
	}
	if err := initSchema(db); err != nil {
		db.Close()
		return &Store{db: nil, openErr: err}
	}
	return &Store{db: db}
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(
		"CREATE TABLE IF NOT EXISTS `secret_key` (" +
			"`secret` TEXT NOT NULL, `key` TEXT NOT NULL, `value` BLOB NOT NULL, " +
			"PRIMARY KEY (`secret`, `key`))")
	return err
}

func (s *Store) ready() error {
	if s.openErr != nil {
		return fmt.Errorf("secrets: %w", s.openErr)
	}
	return nil
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
	if err := s.ready(); err != nil {
		return "", err
	}
	secret, key, explicit := strings.Cut(name, "/")
	if !validComponent(secret) {
		return "", fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	if explicit {
		if !validComponent(key) {
			return "", fmt.Errorf("secrets: %q names no valid key after %q/", name, secret)
		}
		return s.readKey(ctx, secret, key)
	}
	return s.resolveSoleKey(ctx, secret)
}

// resolveSoleKey is the "<secret>" form: it succeeds only when secret
// holds exactly one key, since anything else is an ambiguous name --
// Resolve's caller must say which key it means.
func (s *Store) resolveSoleKey(ctx context.Context, secret string) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT `key` FROM `secret_key` WHERE `secret` = ? ORDER BY `key`", secret)
	if err != nil {
		return "", fmt.Errorf("secrets: reading secret %q: %w", secret, err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return "", fmt.Errorf("secrets: reading secret %q: %w", secret, err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("secrets: reading secret %q: %w", secret, err)
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("secrets: no secret named %q", secret)
	}
	if len(keys) != 1 {
		return "", fmt.Errorf(
			"secrets: secret %q has %d keys, name one explicitly as %q",
			secret, len(keys), secret+"/<key>",
		)
	}
	return s.readKey(ctx, secret, keys[0])
}

func (s *Store) readKey(ctx context.Context, secret, key string) (string, error) {
	var value []byte
	err := s.db.QueryRowContext(ctx,
		"SELECT `value` FROM `secret_key` WHERE `secret` = ? AND `key` = ?", secret, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("secrets: no key %q in secret %q", key, secret)
	}
	if err != nil {
		return "", fmt.Errorf("secrets: reading %q in secret %q: %w", key, secret, err)
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
// diffable rendering. A database with nothing ever Set reports no
// secrets rather than an error, the same "missing is empty, not broken"
// Resolve already gives one name at a time.
func (s *Store) List() ([]SecretInfo, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT `secret`, `key` FROM `secret_key` ORDER BY `secret`, `key`")
	if err != nil {
		return nil, fmt.Errorf("secrets: listing: %w", err)
	}
	defer rows.Close()

	var out []SecretInfo
	var current *SecretInfo
	for rows.Next() {
		var secret, key string
		if err := rows.Scan(&secret, &key); err != nil {
			return nil, fmt.Errorf("secrets: listing: %w", err)
		}
		if current == nil || current.Name != secret {
			out = append(out, SecretInfo{Name: secret})
			current = &out[len(out)-1]
		}
		current.Keys = append(current.Keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("secrets: listing: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Set writes one key's value. An existing key is overwritten outright --
// there is no versioning here, the same "replace a file" shape
// gitproxy/credentials.go already gives the GitHub credential ladder.
func (s *Store) Set(secret, key string, value []byte) error {
	if err := s.ready(); err != nil {
		return err
	}
	if !validComponent(secret) {
		return fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	if !validComponent(key) {
		return fmt.Errorf("secrets: %q is not a valid key name", key)
	}
	if _, err := s.db.Exec(
		"REPLACE INTO `secret_key` (`secret`, `key`, `value`) VALUES (?, ?, ?)",
		secret, key, value); err != nil {
		return fmt.Errorf("secrets: writing %q in secret %q: %w", key, secret, err)
	}
	return nil
}

// DeleteKey removes one key from a secret.
func (s *Store) DeleteKey(secret, key string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if !validComponent(secret) {
		return fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	if !validComponent(key) {
		return fmt.Errorf("secrets: %q is not a valid key name", key)
	}
	res, err := s.db.Exec(
		"DELETE FROM `secret_key` WHERE `secret` = ? AND `key` = ?", secret, key)
	if err != nil {
		return fmt.Errorf("secrets: deleting %q in secret %q: %w", key, secret, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("secrets: deleting %q in secret %q: %w", key, secret, err)
	}
	if n == 0 {
		return fmt.Errorf("secrets: no key %q in secret %q", key, secret)
	}
	return nil
}

// DeleteSecret removes a secret and every key in it.
func (s *Store) DeleteSecret(secret string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if !validComponent(secret) {
		return fmt.Errorf("secrets: %q is not a valid secret name", secret)
	}
	res, err := s.db.Exec("DELETE FROM `secret_key` WHERE `secret` = ?", secret)
	if err != nil {
		return fmt.Errorf("secrets: deleting secret %q: %w", secret, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("secrets: deleting secret %q: %w", secret, err)
	}
	if n == 0 {
		return fmt.Errorf("secrets: no secret named %q", secret)
	}
	return nil
}

// validComponent reports whether s is safe to use as a secret or key
// name: non-empty, not "." or "..", and free of path separators. Nothing
// here puts a name on disk directly the way the old file-tree store did,
// but keeping the same restriction means a name that was invalid before
// this package moved to SQLite stays invalid now, rather than quietly
// starting to work.
func validComponent(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\")
}
