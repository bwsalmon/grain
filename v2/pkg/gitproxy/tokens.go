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
// generated and injected at provisioning time, replaced at recreate -- see
// docs/design.md, "sandbox identity."
type SandboxTokens struct {
	byToken map[string]string
}

// LoadSandboxTokens reads the token map from path. A missing file yields
// an empty map, matching the Python original: the git-proxy service can
// be enabled before any sandbox has ever minted a token.
func LoadSandboxTokens(path string) (*SandboxTokens, error) {
	raw, err := readTokenFile(path)
	if err != nil {
		return nil, err
	}
	byToken := make(map[string]string, len(raw))
	for name, token := range raw {
		byToken[token] = name
	}
	return &SandboxTokens{byToken: byToken}, nil
}

// Authenticate returns the sandbox name owning this token, and false if it
// is unknown.
func (t *SandboxTokens) Authenticate(token string) (string, bool) {
	name, ok := t.byToken[token]
	return name, ok
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
// SandboxTokens reads -- called wherever a sandbox is dispatched to for
// the first time, to mint its token. Split from SandboxTokens because the
// two run with different lifecycles: the proxy loads the map once at
// startup and only ever looks tokens up, while minting one is a
// read-modify-write against the file the proxy trusts.
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
