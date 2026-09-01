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
	if err := t.reload(); err != nil {
		return "", false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	name, ok = t.byToken[token]
	return name, ok
}

// reload replaces the cached map with the file's current contents. A read
// error leaves the previous map in place: a token minted before a
// transient failure is still a token this proxy should honour, and
// dropping the whole map would fail every in-flight run's git rather than
// the one lookup that could not be answered.
func (t *SandboxTokens) reload() error {
	raw, err := readTokenFile(t.path)
	if err != nil {
		return err
	}
	byToken := make(map[string]string, len(raw))
	for name, token := range raw {
		byToken[token] = name
	}
	t.mu.Lock()
	t.byToken = byToken
	t.mu.Unlock()
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
type SandboxTokenStore struct {
	path string
}

func NewSandboxTokenStore(path string) *SandboxTokenStore {
	return &SandboxTokenStore{path: path}
}

// EnsureToken returns the sandbox's existing token, or a freshly minted
// one recorded to disk. Idempotent per sandbox name.
func (s *SandboxTokenStore) EnsureToken(sandbox string) (string, error) {
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

// save writes tokens atomically: a temp file plus rename, so a killed
// write can't corrupt the file the proxy trusts on its very next
// (unrestarted) request.
func (s *SandboxTokenStore) save(tokens map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
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
