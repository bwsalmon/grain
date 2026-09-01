package gitproxy

// Sandbox identity: a per-sandbox bearer token, consumed via HTTP Basic
// auth. docs/design.md is specific that this is "not an SSH key" and that
// "git consumes it via a credential helper, so agents never handle it" --
// a sandbox's git credential helper supplies the token as the password
// half of HTTP Basic auth; the username is irrelevant, and the agent
// inside the sandbox never sees the token at all.
//
// This identity is deliberately separate from Authorizer (authorize.go):
// SandboxTokens answers "who is calling" from a secret only the proxy and
// the sandbox share, minted once and never touched by the task model.
// Authorizer then answers "what may they touch" by reading the model live
// -- the token is how the proxy learns *which* sandbox to ask the model
// about, not what the model itself contains.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// SandboxTokens maps a bearer token to the sandbox it identifies.
//
// File format: {"sandbox-0": "<token>", "sandbox-1": "<token>", ...},
// minted as each sandbox is created -- see docs/design.md, "sandbox
// identity."
//
// It re-reads that file whenever it is shown a token it does not
// recognise. That used to be unnecessary and was deliberately not done:
// every sandbox was a slot, every slot's token was minted before the
// proxy started, and the map could be read once and then treated as
// fixed for the life of the process. A sandbox created per run mints its
// token while the proxy is already serving, so a map pinned at startup
// would reject every git operation from every run -- fail-closed, but
// closed against everything.
//
// Re-reading on a miss rather than on every request keeps the ordinary
// path (a known token, already cached) a map lookup with no I/O, and
// bounds the extra reads to genuinely unknown tokens. A token that is
// unknown because it is bogus costs one file read to establish that,
// which is the same cost as one that is unknown because it is new.
type SandboxTokens struct {
	path string

	mu      sync.RWMutex
	byToken map[string]string
}

// LoadSandboxTokens reads the token map from path. A missing file yields
// an empty map, matching the Python original: the git-proxy service can
// be enabled before any sandbox has ever minted a token.
func LoadSandboxTokens(path string) (*SandboxTokens, error) {
	t := &SandboxTokens{path: path}
	if err := t.reload(); err != nil {
		return nil, err
	}
	return t, nil
}

// Authenticate returns the sandbox name owning this token, and false if it
// is unknown -- unknown meaning "not in the file as of just now," since a
// miss against the cached map re-reads the file before answering (see the
// type's own doc comment).
func (t *SandboxTokens) Authenticate(token string) (string, bool) {
	t.mu.RLock()
	name, ok := t.byToken[token]
	t.mu.RUnlock()
	if ok {
		return name, true
	}
	return t.reloadAndLookup(token)
}

// reloadAndLookup re-reads the file and answers for token, both under one
// hold of the write lock.
//
// Reading the file and swapping the map have to be one critical section,
// not two. Split, a reload that read the file *earlier* can install its
// map *later* -- so a goroutine that just re-read a file containing a
// freshly minted token can have that map replaced by a staler one before
// it looks the token up, and answer false for a token that is on disk.
// That is a spurious 401 on a run's very first git operation, which is
// the exact failure re-reading on a miss exists to prevent.
//
// The cost is that concurrent misses serialize, and a miss holds the
// write lock across a file read. Both are bounded by how rare a miss is:
// a known token never reaches here, so this runs once per new sandbox and
// once per genuinely bogus token, not once per request.
//
// The re-check before reloading is not just an optimization: whichever
// goroutine reloaded while this one waited for the lock may have already
// brought the token in, and answering from that map is both correct and
// one fewer read.
func (t *SandboxTokens) reloadAndLookup(token string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if name, ok := t.byToken[token]; ok {
		return name, true
	}
	if err := t.reloadLocked(); err != nil {
		return "", false
	}
	name, ok := t.byToken[token]
	return name, ok
}

// reload takes the write lock and re-reads the file -- LoadSandboxTokens'
// own first read, where nothing else holds a reference yet.
func (t *SandboxTokens) reload() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reloadLocked()
}

// reloadLocked replaces the cached map with the file's current contents.
// The caller holds t.mu for writing.
//
// A read error leaves the previous map in place: a token minted before a
// transient failure is still a token this proxy should honour, and
// dropping the whole map would fail every in-flight run's git rather than
// the one lookup that could not be answered.
func (t *SandboxTokens) reloadLocked() error {
	raw, err := readTokenFile(t.path)
	if err != nil {
		return err
	}
	byToken := make(map[string]string, len(raw))
	for name, token := range raw {
		byToken[token] = name
	}
	t.byToken = byToken
	return nil
}

func readTokenFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gitproxy: reading %s: %w", path, err)
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("gitproxy: parsing %s: %w", path, err)
	}
	return raw, nil
}

// SandboxTokenStore is the write side of the same sandbox-tokens.json file
// SandboxTokens reads -- called as each run's sandbox is prepared, to mint
// that sandbox's token (orchestrator's own runOne). Split from
// SandboxTokens because the two do different things to the same file: the
// proxy only ever looks tokens up, caching what it has read, while
// minting one is a read-modify-write against the file the proxy trusts.
//
// Every method here is a read-modify-write, and mu is what makes that
// safe. It did not need to be while a token was minted per slot in
// runDaemon's own startup preamble -- one goroutine, one call after
// another, before the daemon dispatched anything. A sandbox per run mints
// from orchestrator's runOne, which reconcileDispatch runs one goroutine
// per dispatch of, so two mints genuinely overlap the moment
// -max-concurrent is above 1. Unguarded, both read the same map, both add
// their own sandbox, and whichever renames last drops the other's token
// on the floor: that run then holds a token the proxy will never find in
// the file, and every git operation inside its sandbox fails closed for
// the life of the run.
type SandboxTokenStore struct {
	path string

	mu sync.Mutex
}

func NewSandboxTokenStore(path string) *SandboxTokenStore {
	return &SandboxTokenStore{path: path}
}

// EnsureToken returns the sandbox's existing token, or a freshly minted
// one recorded to disk. Idempotent per sandbox name, and safe to call
// from several goroutines at once -- see the type's own doc comment.
func (s *SandboxTokenStore) EnsureToken(sandbox string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens, err := readTokenFile(s.path)
	if err != nil {
		return "", err
	}
	if token, ok := tokens[sandbox]; ok {
		return token, nil
	}
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	tokens[sandbox] = token
	return token, s.save(tokens)
}

// Rotate mints and records a fresh token unconditionally, replacing any
// existing one.
func (s *SandboxTokenStore) Rotate(sandbox string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens, err := readTokenFile(s.path)
	if err != nil {
		return "", err
	}
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	tokens[sandbox] = token
	return token, s.save(tokens)
}

// Revoke drops a sandbox's token, so this file holds one entry per
// sandbox that still exists rather than one per run the deployment has
// ever dispatched. A sandbox with no token is not an error: a run that
// failed before it minted one has nothing to revoke, and revoking twice
// is what a retried cleanup does.
//
// This is upkeep, not authorization. A finished run's token already
// authorizes nothing -- Store.GitScope resolves a sandbox through the
// live run on it, and there is none (Store.liveTaskID) -- so nothing is
// unsafe without this. What it stops is unbounded growth: a token per
// slot was a fixed handful for the life of a deployment, a token per run
// is one more every dispatch, and since every mint reads and rewrites the
// whole file, an un-pruned file makes each mint slower than the last.
func (s *SandboxTokenStore) Revoke(sandbox string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens, err := readTokenFile(s.path)
	if err != nil {
		return err
	}
	if _, ok := tokens[sandbox]; !ok {
		return nil
	}
	delete(tokens, sandbox)
	return s.save(tokens)
}

// save writes tokens atomically: a temp file plus rename, so a killed
// write can't corrupt the file the proxy trusts on its very next
// (unrestarted) request. Callers hold s.mu.
//
// The temp file carries a random suffix rather than a fixed ".tmp". Two
// concurrent saves through one fixed name are not atomic at all: the
// second truncates and rewrites the file the first is about to rename, so
// a rename can publish a half-written file, and the loser's own rename
// then fails outright with ENOENT. s.mu already serializes this type's
// own callers; the suffix keeps that true against a second process, or a
// leftover from a killed one, sharing the same data dir.
func (s *SandboxTokenStore) save(tokens map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := s.path + "." + suffix + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ExtractBasicAuthToken pulls the token out of an
// "Authorization: Basic base64(user:token)" header. The username is
// ignored -- a credential helper is configured with an arbitrary username
// and the token as the password, so the token is what identifies the
// caller.
func ExtractBasicAuthToken(header string) (string, bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		return "", false
	}
	_, token, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", false
	}
	return token, true
}
