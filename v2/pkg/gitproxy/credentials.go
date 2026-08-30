package gitproxy

// The credential ladder: the narrowest credential that covers a repo.
//
// docs/design.md's shape:
//
//	/data/secrets/github/
//	  credentials.json     # repo/owner pattern -> credential name
//	  bot.token            # machine account, most repos
//	  personal.token       # last resort, only what nothing else reaches
//
// Unlike Authorizer (authorize.go), which now reads the live task model,
// credentials are not hot-reloaded and are not the model's concern: which
// *secret* backs a repo is an operator decision recorded in files under
// /data/secrets, loaded once at construction, matching docs/design.md's
// "replace a file under /data/secrets and restart the one service that
// reads it."

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Credential is a named credential. A nil Token means anonymous -- no
// Authorization header at all, which is what a public repo needs and is a
// deliberate credential shape, not an error case.
type Credential struct {
	Name  string
	Token *string
}

// CredentialSet is the credential ladder loaded from a secrets directory.
//
// Select and Get are called from GitProxy.Handle, which NewHandler wires
// straight into net/http -- one goroutine per inbound request, so every
// sandbox's concurrent git traffic reaches load's cache at once. mu
// guards it for exactly that reason.
type CredentialSet struct {
	dir      string
	patterns map[string]string

	mu    sync.Mutex
	cache map[string]Credential
}

// LoadCredentialSet reads credentials.json (a pattern -> credential name
// map) from secretsDir. A missing file yields an empty ladder -- every
// select then fails closed with "no credential configured."
func LoadCredentialSet(secretsDir string) (*CredentialSet, error) {
	patterns, err := readStringMap(filepath.Join(secretsDir, "credentials.json"))
	if err != nil {
		return nil, err
	}
	return &CredentialSet{dir: secretsDir, patterns: patterns, cache: map[string]Credential{}}, nil
}

// Select returns the narrowest pattern covering (owner, repo): exact,
// then owner/*, then the global * fallback. The second return is false if
// nothing covers it -- a distinct, fail-closed condition from "not
// allowed at all."
func (c *CredentialSet) Select(owner, repo string) (Credential, bool) {
	owner, repo = canonicalizeRepo(owner, repo)
	for _, pattern := range []string{owner + "/" + repo, owner + "/*", "*"} {
		if name, ok := c.patterns[pattern]; ok && name != "" {
			return c.load(name), true
		}
	}
	return Credential{}, false
}

// Get returns a named credential directly, bypassing the owner/repo
// pattern ladder Select uses -- bwsalmon/agents#52's per-task
// grain-github-<name> label, for a task that needs a scope the default
// ladder deliberately withholds (docs/design.md, "scopes to withhold").
// The second return is false if no such credential is configured
// (<name>.token missing, and name isn't the literal "anonymous").
func (c *CredentialSet) Get(name string) (Credential, bool) {
	if name != "anonymous" {
		if _, err := os.Stat(filepath.Join(c.dir, name+".token")); err != nil {
			return Credential{}, false
		}
	}
	return c.load(name), true
}

func (c *CredentialSet) load(name string) Credential {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cred, ok := c.cache[name]; ok {
		return cred
	}
	var token *string
	if name != "anonymous" {
		data, err := os.ReadFile(filepath.Join(c.dir, name+".token"))
		if err == nil {
			t := strings.TrimSpace(string(data))
			token = &t
		}
	}
	cred := Credential{Name: name, Token: token}
	c.cache[name] = cred
	return cred
}

// canonicalizeRepo is a case-insensitive, .git-suffix-insensitive identity
// for a repo -- GitHub itself treats owner/repo case-insensitively, and a
// client may or may not include the .git suffix.
func canonicalizeRepo(owner, repo string) (string, string) {
	repo = strings.TrimSuffix(repo, ".git")
	return strings.ToLower(owner), strings.ToLower(repo)
}

func readStringMap(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
