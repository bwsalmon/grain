package gitproxy

// The credential ladder: the narrowest credential that covers a repo.
//
// docs/design.md's shape:
//
//	/data/secrets/github/
//	  credentials.json     # repo/owner pattern -> credential name
//	  bot.token            # machine account, most repos
//	  personal.token       # last resort, only what nothing else reaches
//	  ci-app.app.json      # a GitHub App installation, instead of a PAT
//
// Unlike Authorizer (authorize.go), which now reads the live task model,
// credentials are not hot-reloaded and are not the model's concern: which
// *secret* backs a repo is an operator decision recorded in files under
// /data/secrets, loaded once at construction, matching docs/design.md's
// "replace a file under /data/secrets and restart the one service that
// reads it." A *.app.json credential is the one exception to "loaded
// once": what's on disk is loaded once same as a *.token file, but its
// Token is re-minted from that App's own private key roughly every hour
// rather than read as-is -- see apptoken.go and load below.

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/github"
)

// Credential is a named credential. A nil Token means anonymous -- no
// Authorization header at all, which is what a public repo needs and is a
// deliberate credential shape, not an error case.
type Credential struct {
	Name  string
	Token *string
}

// appTokenRefreshSkew is how much of an App installation token's own
// remaining lifetime load treats as "already expired" -- re-minting a
// little before GitHub's own expires_at rather than exactly at it, so a
// request that reads the cache and then takes a moment to actually use
// the token (a git push's own upload, an in-flight REST call) never finds
// GitHub has invalidated it out from under that same request.
const appTokenRefreshSkew = 2 * time.Minute

// appMintFailureRetryDelay is how long load caches a failed mint attempt
// before trying again, rather than calling GitHub's token-exchange
// endpoint on every single request a broken App credential is asked to
// serve -- a wrong or revoked private key does not get less wrong by
// retrying immediately, and every sandbox's git traffic can reach this
// concurrently (TestSelectAndGetAreSafeForConcurrentCallers).
const appMintFailureRetryDelay = 30 * time.Second

// cacheEntry is one load result. expiresAt is the zero time for a static
// *.token credential (or "anonymous"), which load reads as "never
// expires" -- the cache behavior every credential had before App-backed
// ones existed. refreshEarly marks a successfully minted App token,
// whose expiresAt is when GitHub itself invalidates it -- appTokenRefreshSkew
// only applies there, never to a mint-failure entry's own short
// expiresAt (appMintFailureRetryDelay out): applying the same multi-minute
// skew to a thirty-second backoff would put its deadline in the past the
// instant it's set, and load would retry on every call instead of waiting
// out the backoff at all.
type cacheEntry struct {
	cred         Credential
	expiresAt    time.Time
	refreshEarly bool
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

	// AppTransport is where a *.app.json credential's installation token
	// gets minted -- always github.com's own REST API (see
	// mintInstallationToken's own doc comment on why App auth has no
	// Enterprise path here yet). nil uses a real github.NewRealTransport
	// against github.com; a test overrides it with a github.FakeTransport
	// the same way live_test.go already overrides RealForwarder.
	AppTransport github.Transport
	// Now is load's clock, overridable so a test can advance past an App
	// token's expiry without an actual hour passing. nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// LoadCredentialSet reads credentials.json (a pattern -> credential name
// map) from secretsDir. A missing file yields an empty ladder -- every
// select then fails closed with "no credential configured."
func LoadCredentialSet(secretsDir string) (*CredentialSet, error) {
	patterns, err := readStringMap(filepath.Join(secretsDir, "credentials.json"))
	if err != nil {
		return nil, err
	}
	return &CredentialSet{dir: secretsDir, patterns: patterns, cache: map[string]cacheEntry{}}, nil
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
// (neither <name>.token nor <name>.app.json exists, and name isn't the
// literal "anonymous").
func (c *CredentialSet) Get(name string) (Credential, bool) {
	if name != "anonymous" {
		_, tokenErr := os.Stat(filepath.Join(c.dir, name+".token"))
		_, appErr := os.Stat(filepath.Join(c.dir, name+".app.json"))
		if tokenErr != nil && appErr != nil {
			return Credential{}, false
		}
	}
	return c.load(name), true
}

func (c *CredentialSet) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// entryIsFresh reports whether entry can still be served without
// recomputing it. A zero expiresAt (a static *.token credential, or
// "anonymous") never expires; anything else is fresh until its own
// deadline -- entry.expiresAt itself for a mint-failure backoff,
// appTokenRefreshSkew before it for a real App token (cacheEntry's own
// doc comment on why those two cases apply the skew differently).
func entryIsFresh(entry cacheEntry, now time.Time) bool {
	if entry.expiresAt.IsZero() {
		return true
	}
	deadline := entry.expiresAt
	if entry.refreshEarly {
		deadline = deadline.Add(-appTokenRefreshSkew)
	}
	return now.Before(deadline)
}

func (c *CredentialSet) appTransport() github.Transport {
	if c.AppTransport != nil {
		return c.AppTransport
	}
	return github.NewRealTransport("github.com")
}

// load resolves name to a Credential, consulting the cache first. A
// cached entry is used as-is when it never expires (a *.token credential,
// or "anonymous") or hasn't yet crossed into appTokenRefreshSkew of its
// own expiry (an App-backed one); otherwise it is (re)computed and the
// cache updated before returning, so the next call -- including one
// racing this one, both blocked on mu -- gets the same answer without
// minting twice.
func (c *CredentialSet) load(name string) Credential {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if entry, ok := c.cache[name]; ok && entryIsFresh(entry, now) {
		return entry.cred
	}
	if name == "anonymous" {
		cred := Credential{Name: name}
		c.cache[name] = cacheEntry{cred: cred}
		return cred
	}
	if appCred, ok := c.loadAppCredential(name); ok {
		token, expiresAt, err := mintInstallationToken(c.appTransport(), appCred, now)
		if err != nil {
			log.Printf("gitproxy: minting an installation token for GitHub App credential %q: %v -- "+
				"serving it as anonymous until the next retry", name, err)
			cred := Credential{Name: name}
			c.cache[name] = cacheEntry{cred: cred, expiresAt: now.Add(appMintFailureRetryDelay)}
			return cred
		}
		cred := Credential{Name: name, Token: &token}
		c.cache[name] = cacheEntry{cred: cred, expiresAt: expiresAt, refreshEarly: true}
		return cred
	}
	var token *string
	data, err := os.ReadFile(filepath.Join(c.dir, name+".token"))
	if err == nil {
		t := strings.TrimSpace(string(data))
		token = &t
	}
	cred := Credential{Name: name, Token: token}
	c.cache[name] = cacheEntry{cred: cred}
	return cred
}

// loadAppCredential reads name.app.json, if it exists: app_id,
// installation_id and private_key (that App's own PEM-encoded key,
// exactly as GitHub hands it out -- ParseAppPrivateKey's own doc
// comment). Any problem with the file (missing, malformed JSON, a key
// that won't parse) is reported here as "no such App credential" rather
// than propagated, the same fail-soft-to-anonymous contract a missing
// *.token file already gets from load above -- logged either way, so an
// operator's typo in the file does not read as a silent, permanent
// "nothing is auto-merging" the way #483 was before it had a log line to
// point at.
func (c *CredentialSet) loadAppCredential(name string) (AppCredential, bool) {
	data, err := os.ReadFile(filepath.Join(c.dir, name+".app.json"))
	if err != nil {
		return AppCredential{}, false
	}
	var raw struct {
		AppID          string `json:"app_id"`
		InstallationID string `json:"installation_id"`
		PrivateKey     string `json:"private_key"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("gitproxy: %s.app.json is not valid JSON: %v", name, err)
		return AppCredential{}, false
	}
	key, err := ParseAppPrivateKey([]byte(raw.PrivateKey))
	if err != nil {
		log.Printf("gitproxy: %s.app.json: %v", name, err)
		return AppCredential{}, false
	}
	return AppCredential{AppID: raw.AppID, InstallationID: raw.InstallationID, PrivateKey: key}, true
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
