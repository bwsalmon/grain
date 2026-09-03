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
//
// grain/task-137 asked whether this ladder should also resolve a
// credential out of the SQLite secrets store pkg/secrets keeps (secret
// "github", key <name>) -- the store the UI's own Secrets pane writes --
// so that "where secrets live" had one answer. It deliberately does not,
// and SetToken/Remove below are the other half of that decision: the UI
// writes *these files*, rather than the proxy learning to read a second
// store. Three reasons, recorded here because the question will come
// back:
//
//   - A GitHub App credential is three values with their own file shape
//     (*.app.json, loadAppCredential below). Backing it with a secrets
//     row would mean inventing a second encoding of the same thing, and
//     then keeping the two in step forever.
//   - This ladder is loaded by more than the daemon: `grain mcp-server`
//     builds one too. Reading credentials out of the secrets database
//     would put a second process on that SQLite file, for material a
//     0600 file already holds just as well.
//   - Two stores that can both answer "what is credential X" is a state
//     an operator has to reason about the moment they disagree. One
//     store cannot.
//
// So the file tree is the mechanism, everywhere: scripts/setup.sh seeds
// it, terraform/gcp/deploy/push-secrets.sh feeds that, the bootstrap
// playbook (pkg/capability/bootstrap/playbooks/github-connections.md)
// walks a human through it, and pkg/ui's Settings pane now writes it
// through SetToken instead of asking an operator for shell access.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
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

// Dir is the secrets directory this ladder was loaded from -- for a
// caller that has to *name* it to a human (pkg/ui's Settings pane,
// explaining where the token it just wrote landed). It is a path, and
// says nothing about what is in it.
func (c *CredentialSet) Dir() string { return c.dir }

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

// Names is every credential this deployment has configured: one per
// <name>.token or <name>.app.json file in the secrets directory, sorted,
// with a name that has both counted once (load prefers the App
// credential, so it is one credential either way).
//
// Read from the directory rather than from credentials.json, because the
// two answer different questions: the pattern file says which credential
// a repo falls back to, while this says which credentials exist at all
// -- including the extra named tokens an operator drops in for Get above
// to reach, which no pattern ever names (ExtraNames below).
//
// Unlike load's own cache this re-reads the directory on every call: it
// is called at startup, not per request, and a listing is cheap next to
// the "restart the one service that reads it" contract the rest of this
// file keeps.
func (c *CredentialSet) Names() []string {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		// A secrets directory that cannot be read is the same "no
		// credentials configured" this file already treats a missing
		// credentials.json as -- every Select then fails closed with
		// "no credential configured," which is the failure worth
		// surfacing, and it is surfaced where a request can see it.
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := credentialFileName(e.Name())
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DefaultName is the credential the global "*" pattern names -- the one
// every repo with no narrower entry is pushed and pulled with, and so
// the one a task needs no capability to be using already. Empty when
// this deployment has no "*" entry at all.
func (c *CredentialSet) DefaultName() string { return c.patterns["*"] }

// ExtraNames is Names minus DefaultName: every named GitHub token beyond
// the deployment default, which is exactly the set that becomes a
// capability of its own (model.GitCredentialCapability, pkg/capability/
// githubtoken). A credential some *narrower* ladder entry names is
// deliberately still in here: the ladder decides which repos reach it by
// default, and this decides which tokens a task can ask for by name,
// which are two independent choices about the same credential.
func (c *CredentialSet) ExtraNames() []string {
	def := c.DefaultName()
	var out []string
	for _, name := range c.Names() {
		if name == def {
			continue
		}
		out = append(out, name)
	}
	return out
}

// IsApp reports whether name is backed by a <name>.app.json file -- a
// GitHub App installation whose token load re-mints, rather than a
// *.token file read as-is. Callers that offer to *edit* a credential
// need this: SetToken below refuses to write over an App credential, and
// a UI is better off saying so up front than after the fact.
func (c *CredentialSet) IsApp(name string) bool {
	_, err := os.Stat(filepath.Join(c.dir, name+".app.json"))
	return err == nil
}

// PatternsFor is every credentials.json pattern that names this
// credential, sorted -- empty for a token no pattern mentions, which is
// the ordinary shape of an extra named token (ExtraNames' own doc
// comment: the ladder and the capability set are two independent choices
// about the same credential).
//
// This is what makes "is anything relying on this credential" answerable
// before deleting one: a caller that removes a credential some pattern
// still names leaves every repo that pattern covers failing closed with
// "no credential configured" on its next push.
func (c *CredentialSet) PatternsFor(name string) []string {
	var out []string
	for pattern, credential := range c.patterns {
		if credential == name {
			out = append(out, pattern)
		}
	}
	sort.Strings(out)
	return out
}

// ValidCredentialName reports whether name may be used for a credential
// written through SetToken: it becomes a filename in the secrets
// directory, and it becomes half of a capability id
// (model.GitCredentialCapability), so it is held to a conservative shape
// rather than to whatever the filesystem happens to tolerate. A name
// already on disk that does not match this is still read as it always
// was -- this gates writing, not loading.
//
// "anonymous" is refused because load reads it as a credential shape of
// its own (no Authorization header at all); a file of that name would be
// written, listed, and then never used.
func ValidCredentialName(name string) error {
	if name == "" {
		return fmt.Errorf("a credential name is required")
	}
	if name == "anonymous" {
		return fmt.Errorf("%q is reserved: it already names the no-credential-at-all case", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return fmt.Errorf(
				"%q is not a usable credential name: letters, digits, %q and %q only", name, "-", "_")
		}
	}
	return nil
}

// SetToken writes token as name's credential material -- name.token in
// the secrets directory, mode 0600, replacing whatever was there.
//
// Written to a temporary file and renamed into place, so a reader that
// catches this mid-write (the git proxy in another process, serving a
// push right now) sees either the old token or the new one, never half
// of one. The temporary file's own name deliberately does not end in
// .token, so a crash between create and rename cannot leave something
// Names would report as a credential.
//
// Refused for a name already backed by name.app.json: load prefers the
// App credential, so the file this would write would be loaded by
// nothing, and silently doing nothing is the worst answer available.
//
// The value takes effect for a process that has not loaded it yet;
// everything already running keeps what it cached (this file's own doc
// comment on why the ladder is not hot-reloaded). Callers that offer
// this to a human should say so.
func (c *CredentialSet) SetToken(name, token string) error {
	if err := ValidCredentialName(name); err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("a token value is required")
	}
	if c.IsApp(name) {
		return fmt.Errorf(
			"%q is a GitHub App credential (%s.app.json): replace that file to change it", name, name)
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("creating the GitHub secrets directory: %w", err)
	}
	tmp, err := os.CreateTemp(c.dir, "."+name+".token.tmp")
	if err != nil {
		return fmt.Errorf("writing credential %q: %w", name, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("writing credential %q: %w", name, err)
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("writing credential %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing credential %q: %w", name, err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(c.dir, name+".token")); err != nil {
		return fmt.Errorf("writing credential %q: %w", name, err)
	}
	c.forget(name)
	return nil
}

// Remove deletes the files backing name -- both forms, since a name with
// a *.token and a *.app.json is one credential either way (Names' own
// doc comment). It reports an error when nothing of that name is
// configured, so a caller can tell "removed" from "was never there."
//
// It says nothing about credentials.json: a pattern naming a credential
// that no longer exists is a real, and deliberately visible, failure
// (Select's own fail-closed "no credential configured"). PatternsFor
// above is how a caller checks before asking for this.
func (c *CredentialSet) Remove(name string) error {
	if err := ValidCredentialName(name); err != nil {
		return err
	}
	var removed bool
	for _, suffix := range []string{".token", ".app.json"} {
		err := os.Remove(filepath.Join(c.dir, name+suffix))
		switch {
		case err == nil:
			removed = true
		case os.IsNotExist(err):
		default:
			return fmt.Errorf("removing credential %q: %w", name, err)
		}
	}
	if !removed {
		return fmt.Errorf("no credential named %q is configured", name)
	}
	c.forget(name)
	return nil
}

// forget drops name from load's cache, so this CredentialSet's own next
// read of it goes back to the files SetToken/Remove just changed. It
// does nothing for the *other* CredentialSets a deployment has loaded
// (the git proxy's, the REST client's, the MCP server's) -- those are in
// other processes or were built separately, and the restart this file's
// doc comment describes is still what makes a change reach them.
func (c *CredentialSet) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, name)
}

// credentialFileName maps a file in the secrets directory back to the
// credential it backs -- "bot.token" and "bot.app.json" are both the
// "bot" credential (load's own two forms) -- and reports false for
// anything else there, credentials.json itself included.
func credentialFileName(file string) (string, bool) {
	for _, suffix := range []string{".token", ".app.json"} {
		if name, ok := strings.CutSuffix(file, suffix); ok && name != "" {
			return name, true
		}
	}
	return "", false
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
