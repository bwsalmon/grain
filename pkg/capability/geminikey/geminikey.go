// Package geminikey is a MINT model.CapabilityProvider: it mints a
// short-lived Gemini API key for a task labelled grain-gemini-key, places
// it in the sandbox, and revokes it when the task's slot frees --
// bwsalmon/agents#239, porting grain/automation/gemini_keys.py's
// "gemini-key" capability (docs/data-model.md's worked MINT example)
// into v2.
//
// README.md notes that a real MINT provider needs "standing
// credentials and a controller v2 has neither of" -- that gap is about
// wiring, not about whether this package's logic can be written and
// tested: nothing here assumes a controller process exists.
// model.CapabilityContext already carries a CredentialResolver, which is
// all minting needs to reach a standing credential by name; what v2 is
// still missing is something that constructs a real *store.Store-backed
// CapabilityContext and calls Materialize/Revoke against it during an
// actual dispatch. That wiring, and a gcp-key capability to go with it,
// are follow-on work.
package geminikey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/apikeys/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/bwsalmon/grain/pkg/model"
)

// KeyPath is where a minted key lands in the sandbox -- fixed, not
// configurable, because the prompt this capability writes names it
// verbatim (see PromptSection) and nothing here has a reason to move it.
const KeyPath = "/home/debian/.gemini-api-key"

// DefaultAPITargetService scopes every minted key to the Generative
// Language API alone, so a leaked key is useless against anything else
// enabled in the project -- the same restriction
// grain/automation/gemini_keys.py applies via --api-target=service=...
const DefaultAPITargetService = "generativelanguage.googleapis.com"

// displayNamePrefix marks a key as this capability's own. An API key is
// not scoped to a service account the way a gcp-key capability's keys
// are -- ListKeys returns every key in the project -- so this prefix is
// the only thing separating grain's keys from anyone else's, and
// DeleteExpired must never delete a key without it.
const displayNamePrefix = "grain-"

// OperatingKeyDisplayName names the daemon's own long-lived operating
// key -- the one pkg/agent/antigravity runs as, seeded at deploy time by
// `grain secrets mint-gemini-key` (cmd/grain/secrets.go) rather than
// supplied by hand.
//
// It deliberately keeps displayNamePrefix, because that prefix is what
// marks a key in this project as grain's at all. That alone would make
// it reapable, though, and this key must outlive maxLease by design:
// deleteExpired therefore exempts it by exact name. Renaming this
// constant out from under that check would delete the running daemon's
// own key roughly a day after any deploy -- see deleteExpired.
//
// The name is fixed rather than derived so the exemption is a literal
// comparison with no run-shaped input in it: Materialize's own keys are
// grain-<run id>, and no run id is this string.
const OperatingKeyDisplayName = displayNamePrefix + "daemon-operating-key"

// maxLease is the unconditional backstop past which a lease is revoked
// regardless of whether its task ever released cleanly -- "clean up
// after 24 hours if leaked" (bwsalmon/agents#239). A GCP API key has no
// native TTL, so this is enforced by DeleteExpired, not by the key
// itself; see that function's doc comment.
const maxLease = 24 * time.Hour

// minter is the narrow surface Materialize, Revoke and DeleteExpired
// need against the API Keys API -- narrow enough that Capability's own
// tests fake it and need no network, the same "test needs no sandbox and
// no cloud" bar docs/data-model.md sets for a MINT provider.
type minter interface {
	CreateKey(ctx context.Context, displayName, apiTargetService string) (name, keyString string, err error)
	DeleteKey(ctx context.Context, name string) error
	ListKeys(ctx context.Context) ([]mintedKey, error)
}

// mintedKey is one entry from ListKeys -- just enough for DeleteExpired
// to decide age and ownership without carrying the key string.
type mintedKey struct {
	Name        string
	DisplayName string
	CreateTime  time.Time
}

// minterFactory builds a minter from resolved credential material (a
// standing service-account key's raw JSON) and the project it mints
// into. A field on Capability rather than a package-level default, so
// tests substitute a fake with no network involved.
type minterFactory func(ctx context.Context, credentialJSON, projectID string) (minter, error)

// Capability mints Gemini API keys, scoped to one GCP project and
// authenticated as one standing credential.
//
// Credential deliberately names the same standing credential a gcp-key
// capability would mint service-account keys with (bwsalmon/agents#239:
// "This can share the same account from the gcp capability") -- there is
// no separate minter identity here the way
// grain/automation/gemini_keys.py's impersonation dance has, because
// nothing in this project yet grants that identity narrower permissions
// than the account it would otherwise share.
type Capability struct {
	// ProjectID is the GCP project keys are minted in and listed from.
	// Empty makes Resolve refuse -- see Resolve.
	ProjectID string
	// Credential names the standing credential Materialize and Revoke
	// resolve, through model.CapabilityContext.Credentials, to get
	// material to mint with. Empty makes Resolve refuse.
	Credential model.CredentialRef
	// APITargetService restricts every minted key to one API. Empty
	// means DefaultAPITargetService.
	APITargetService string

	factory minterFactory // nil means newAPIKeysMinter
}

// New builds a Capability minting into project, authenticated as
// credential, restricted to DefaultAPITargetService.
func New(projectID string, credential model.CredentialRef) *Capability {
	return &Capability{ProjectID: projectID, Credential: credential}
}

func (c *Capability) apiTargetService() string {
	if c.APITargetService != "" {
		return c.APITargetService
	}
	return DefaultAPITargetService
}

func (c *Capability) Spec() model.CapabilitySpec {
	spec := model.CapabilitySpec{
		Name:        "gemini-key",
		Label:       "grain-gemini-key",
		Description: "Mint a short-lived Gemini API key for this task",
		Source:      model.GrantByLabel,
		Provision:   model.ProvisionMint,
		MaxLease:    maxLease,
	}
	// The standing credential Materialize/Revoke mint under (this
	// package's doc comment: "there is no separate minter identity
	// here") is resolved through CapabilityContext.Credentials, so it
	// belongs here the same way gcpkey.Provider.Spec() lists its own
	// minter credential. Omitted, not the empty string, when this
	// Capability was never configured with one -- Resolve already
	// refuses that case, and an unconfigured deployment has no name to
	// require.
	if c.Credential.Name != "" {
		spec.Requires = []string{c.Credential.Name}
	}
	return spec
}

// Resolve refuses when this Capability was never configured with a
// project and a standing credential to mint with -- the same
// "unhonourable means parked, never silently downgraded" treatment
// grain/automation/gemini_keys.py's GeminiKey.resolve gives a deployment
// missing gemini_key_config, translated to "this value was never wired
// up" since v2 has no deployment config to be absent from yet.
//
// It also refuses when that credential names nothing this deployment's
// secret store can resolve. Materialize would fail on the very same call
// a moment later, so nothing new is caught here -- what changes is which
// half of the contract reports it. A refusal is posted to the task
// verbatim (Resolution.Reason), so an operator reads a sentence naming
// the secret to set; a failed Materialize is wrapped into a run's error
// detail as "materializing capabilities: geminikey: resolving credential
// ...", which is grain describing its own internals. The distinction
// matters most for exactly this capability, whose configuration is
// spread across a deployment setting, a secret and a GCP IAM grant, and
// which therefore has three separate ways to be half-wired.
func (c *Capability) Resolve(ctx context.Context, cc model.CapabilityContext) (model.Resolution, error) {
	if c.ProjectID == "" || c.Credential.Name == "" {
		return model.RefusedBecause(
			"this task asks for a Gemini key this deployment isn't configured to " +
				"mint. An operator sets the GCP project the key is minted in, under " +
				"Settings -> Capabilities (`grain settings -gcp-project <project>`).",
		), nil
	}
	if cc.Credentials == nil {
		return model.RefusedBecause(
			"this task asks for a Gemini key, but nothing here can reach the " +
				"standing credential `" + c.Credential.Name + "` the key would be " +
				"minted under.",
		), nil
	}
	if _, err := cc.Credentials.Resolve(ctx, c.Credential.Name); err != nil {
		return model.RefusedBecause(
			"this task asks for a Gemini key, but the standing credential `" +
				c.Credential.Name + "` it is minted under is not set on this " +
				"deployment. An operator pastes the GCP minter service account's " +
				"key file into Settings -> Secrets, or runs `grain secrets set " +
				c.Credential.Name + " key.json -value-file <path>`.",
		), nil
	}
	return model.Honoured(), nil
}

// Materialize mints a fresh key named grain-<run id> -- the janitor's
// positive signal falling out of ordinary use, the same reasoning
// docs/data-model.md gives for gemini_keys.py's own display_name -- and
// returns it as a single sandbox Placement at KeyPath plus the Lease to
// revoke it by later.
func (c *Capability) Materialize(ctx context.Context, cc model.CapabilityContext) (model.Materialization, error) {
	m, err := c.minter(ctx, cc)
	if err != nil {
		return model.Materialization{}, err
	}
	displayName := displayNamePrefix + cc.Run.ID
	name, keyString, err := m.CreateKey(ctx, displayName, c.apiTargetService())
	if err != nil {
		return model.Materialization{}, fmt.Errorf("geminikey: minting a key for run %s: %w", cc.Run.ID, err)
	}
	expires := cc.Now.Add(maxLease)
	return model.Materialization{
		Lease: &model.Lease{
			Capability: c.Spec().Name,
			Resource:   name,
			MintedBy:   c.Credential,
			IssuedAt:   cc.Now,
			ExpiresAt:  &expires,
		},
		Placements: []model.Placement{
			{Side: model.SideSandbox, Path: KeyPath, Content: keyString},
		},
	}, nil
}

func (c *Capability) PromptSection(ctx context.Context, cc model.CapabilityContext, placements []model.Placement) (string, error) {
	return fmt.Sprintf("A Gemini API key is at %s, readable only by you:\n\n"+
		"    export GEMINI_API_KEY=\"$(cat %s)\"\n", KeyPath, KeyPath), nil
}

// Revoke deletes the key Materialize minted, by the resource name the
// Lease carries. Idempotent: a lease revoked twice, or one whose key is
// already gone, is not an error -- see (*apiKeysMinter).DeleteKey.
func (c *Capability) Revoke(ctx context.Context, cc model.CapabilityContext, lease model.Lease) error {
	m, err := c.minter(ctx, cc)
	if err != nil {
		return err
	}
	if err := m.DeleteKey(ctx, lease.Resource); err != nil {
		return fmt.Errorf("geminikey: revoking key %s: %w", lease.Resource, err)
	}
	return nil
}

func (c *Capability) minter(ctx context.Context, cc model.CapabilityContext) (minter, error) {
	material, err := cc.Credentials.Resolve(ctx, c.Credential.Name)
	if err != nil {
		return nil, fmt.Errorf("geminikey: resolving credential %q: %w", c.Credential.Name, err)
	}
	build := c.factory
	if build == nil {
		build = newAPIKeysMinter
	}
	return build(ctx, material, c.ProjectID)
}

// MintOperatingKey mints the daemon's own long-lived Gemini API key --
// the credential pkg/agent/antigravity runs as, distinct from the per-task
// keys Materialize mints -- authenticating with the credential named
// credentialName, the same standing minter credential the capability
// itself uses.
//
// This exists so a deployment that already grants its minter
// roles/serviceusage.apiKeysAdmin (terraform/gcp's enable_gemini_key)
// does not also need a Gemini key supplied by hand before its daemon
// will start. It returns the key's resource name alongside the secret so
// a caller can report what it created; the key is restricted to
// DefaultAPITargetService exactly like every other key minted here.
//
// It is deliberately not a Capability method: nothing about it is
// per-task, it holds no Lease, and DeleteExpired exempts what it mints
// -- see OperatingKeyDisplayName.
func MintOperatingKey(ctx context.Context, credentials model.CredentialResolver, credentialName, projectID string) (name, keyString string, err error) {
	material, err := credentials.Resolve(ctx, credentialName)
	if err != nil {
		return "", "", fmt.Errorf("geminikey: resolving credential %q: %w", credentialName, err)
	}
	m, err := newAPIKeysMinter(ctx, material, projectID)
	if err != nil {
		return "", "", err
	}
	return mintOperatingKey(ctx, m)
}

func mintOperatingKey(ctx context.Context, m minter) (name, keyString string, err error) {
	name, keyString, err = m.CreateKey(ctx, OperatingKeyDisplayName, DefaultAPITargetService)
	if err != nil {
		return "", "", fmt.Errorf("geminikey: minting the daemon's operating key: %w", err)
	}
	return name, keyString, nil
}

// DeleteExpired deletes every grain-minted key in projectID older than
// maxAge, authenticating with the credential named credentialName --
// the safety net for "clean up after 24 hours if leaked"
// (bwsalmon/agents#239), independent of whether the task that minted a
// key ever released its lease cleanly. Mirrors
// grain/automation/gemini_keys.py's delete_expired_keys: a GCP API key
// has no native TTL, so this reap is what actually enforces one, run
// periodically by whatever plays sweeper.py's role once v2 has one.
//
// Best-effort per key, and a key whose createTime is unparseable is left
// alone rather than guessed at -- "absent data loses, doesn't crash",
// the same stance the ported function takes.
func DeleteExpired(ctx context.Context, credentials model.CredentialResolver, credentialName, projectID string, now time.Time, maxAge time.Duration) ([]string, error) {
	material, err := credentials.Resolve(ctx, credentialName)
	if err != nil {
		return nil, fmt.Errorf("geminikey: resolving credential %q: %w", credentialName, err)
	}
	m, err := newAPIKeysMinter(ctx, material, projectID)
	if err != nil {
		return nil, err
	}
	return deleteExpired(ctx, m, now, maxAge)
}

func deleteExpired(ctx context.Context, m minter, now time.Time, maxAge time.Duration) ([]string, error) {
	keys, err := m.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("geminikey: listing keys: %w", err)
	}
	cutoff := now.Add(-maxAge)
	var deleted []string
	for _, k := range keys {
		if !strings.HasPrefix(k.DisplayName, displayNamePrefix) {
			continue
		}
		// The daemon's own operating key is grain's, and old by
		// design -- it is the credential this process runs as, not a
		// per-task lease that leaked. Reaping it on age would stop the
		// daemon a day after every deploy.
		if k.DisplayName == OperatingKeyDisplayName {
			continue
		}
		if !k.CreateTime.Before(cutoff) {
			continue
		}
		if err := m.DeleteKey(ctx, k.Name); err != nil {
			continue
		}
		deleted = append(deleted, k.Name)
	}
	return deleted, nil
}

// --- the real minter, against the live API Keys API ------------------

// pollInterval paces polling a Create/Delete operation to completion.
// Not configurable by an operator: it trades a handful of extra round
// trips for one fewer knob, and every caller already governs the overall
// wait through ctx. A var rather than a const only so this package's own
// tests can drive await's polling in milliseconds instead of seconds.
var pollInterval = 2 * time.Second

type apiKeysMinter struct {
	svc       *apikeys.Service
	projectID string
}

// newAPIKeysMinter is the real minterFactory: credentialJSON is a
// standing service account's raw key file, exactly what
// model.CredentialResolver.Resolve returns for a GCP credential name.
//
// Unlike grain/automation/gemini_keys.py, which shells out to gcloud
// because this project is otherwise stdlib-only Python, this calls the
// API Keys API directly through its Go client library -- the "the GCP Go
// SDK would retire the gcloud exception" README.md already names as
// one of the things a Go port corrects. That also sidesteps the CLI
// quirks gemini_keys.py's docstring documents at length (create prints
// an operation id, not a key id, on some gcloud versions): the client
// library's Create returns a proper long-running Operation to poll, with
// a typed Response to unmarshal once it is done, rather than text this
// project would otherwise have to parse defensively.
func newAPIKeysMinter(ctx context.Context, credentialJSON, projectID string) (minter, error) {
	creds, err := google.CredentialsFromJSON(ctx, []byte(credentialJSON), apikeys.CloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("geminikey: parsing credential material: %w", err)
	}
	svc, err := apikeys.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("geminikey: building an API Keys client: %w", err)
	}
	return &apiKeysMinter{svc: svc, projectID: projectID}, nil
}

func (a *apiKeysMinter) parent() string {
	return "projects/" + a.projectID + "/locations/global"
}

func (a *apiKeysMinter) CreateKey(ctx context.Context, displayName, apiTargetService string) (name, keyString string, err error) {
	key := &apikeys.V2Key{DisplayName: displayName}
	if apiTargetService != "" {
		key.Restrictions = &apikeys.V2Restrictions{
			ApiTargets: []*apikeys.V2ApiTarget{{Service: apiTargetService}},
		}
	}
	op, err := a.svc.Projects.Locations.Keys.Create(a.parent(), key).Context(ctx).Do()
	if err != nil {
		return "", "", fmt.Errorf("creating key: %w", advise(a.projectID, err))
	}
	op, err = a.await(ctx, op)
	if err != nil {
		return "", "", advise(a.projectID, err)
	}

	var created apikeys.V2Key
	if err := json.Unmarshal(op.Response, &created); err != nil {
		return "", "", fmt.Errorf("parsing the created key out of the operation response: %w", err)
	}
	if created.Name == "" {
		return "", "", fmt.Errorf("created key carries no resource name: %s", op.Response)
	}

	// Past this point a key exists in the project whether or not the
	// rest of this call succeeds -- a caller only gets a name to revoke
	// later if CreateKey returns, so a failure reading the key string
	// back must clean up here rather than strand it, the same
	// bwsalmon/agents#104 lesson gemini_keys.py's own create_key learned.
	got, err := a.svc.Projects.Locations.Keys.GetKeyString(created.Name).Context(ctx).Do()
	if err != nil {
		_ = a.DeleteKey(ctx, created.Name)
		return "", "", fmt.Errorf("reading back the key string: %w", advise(a.projectID, err))
	}
	// An empty key string is the one failure here that looks like
	// success: it would be placed at KeyPath, described to the agent as a
	// working key by PromptSection, and only fail much later as an
	// authentication error from Gemini with nothing in it naming grain.
	// Treated as a failed mint, and cleaned up the same way an unreadable
	// key is, for the same bwsalmon/agents#104 reason.
	if got.KeyString == "" {
		_ = a.DeleteKey(ctx, created.Name)
		return "", "", fmt.Errorf("key %s was created but its key string came back empty", created.Name)
	}
	return created.Name, got.KeyString, nil
}

func (a *apiKeysMinter) await(ctx context.Context, op *apikeys.Operation) (*apikeys.Operation, error) {
	for !op.Done {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
		next, err := a.svc.Operations.Get(op.Name).Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("polling operation %s: %w", op.Name, err)
		}
		op = next
	}
	if op.Error != nil {
		return nil, fmt.Errorf("operation %s failed: %s", op.Name, op.Error.Message)
	}
	return op, nil
}

// DeleteKey waits for the deletion to actually finish, not just be
// accepted -- this is what makes a key gone in a caller's own next
// ListKeys, not merely queued to become so. Idempotent: a key already
// gone (deleted by a previous call, or by DeleteExpired's reap racing
// this one) reports success rather than an error, the same tolerance
// docs/data-model.md asks of every provider's Revoke.
func (a *apiKeysMinter) DeleteKey(ctx context.Context, name string) error {
	op, err := a.svc.Projects.Locations.Keys.Delete(name).Context(ctx).Do()
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting key %s: %w", name, err)
	}
	if _, err := a.await(ctx, op); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting key %s: %w", name, err)
	}
	return nil
}

func (a *apiKeysMinter) ListKeys(ctx context.Context) ([]mintedKey, error) {
	var out []mintedKey
	err := a.svc.Projects.Locations.Keys.List(a.parent()).Pages(ctx, func(resp *apikeys.V2ListKeysResponse) error {
		for _, k := range resp.Keys {
			// A key with no parseable createTime is left out of the
			// listing entirely, rather than defaulting to a zero time
			// deleteExpired would then treat as ancient -- "absent data
			// loses, doesn't crash".
			created, err := time.Parse(time.RFC3339, k.CreateTime)
			if err != nil {
				continue
			}
			out = append(out, mintedKey{Name: k.Name, DisplayName: k.DisplayName, CreateTime: created})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func isNotFound(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == 404
}

// advise annotates the two ways a mint fails on a project nobody
// finished setting up, with the sentence that says what to do about it.
//
// Both arrive as an indistinguishable HTTP 403 from the API Keys API,
// and both are easy to reach: `grain setup gcp` defaults
// -enable-gemini-key to *off*, while terraform/gcp's own
// enable_gemini_key defaults to *on*, so a deployment installed by
// script rather than by that module has a GCP project, a minter
// credential, a "Ready" badge on Settings' Capabilities tab -- and a
// minter holding no roles/serviceusage.apiKeysAdmin and a project with
// apikeys.googleapis.com never enabled. The raw error for that is
// "googleapi: Error 403: Permission 'apikeys.keys.create' denied on
// resource ... (or it may not exist)", which reads like a bug in grain
// rather than like one unrun setup flag.
//
// The distinction between the two is drawn from the error's own
// SERVICE_DISABLED reason (an ErrorInfo detail Google returns for every
// call against an API a project has not enabled), falling back to the
// message text, since that reason is what separates "enable the API"
// from "grant the role" -- and the remedy names both anyway, because the
// one command that fixes either fixes both.
//
// Anything that is not a 403 is returned unchanged: a 404, a quota
// error, a transport failure all already say what they are, and wrapping
// every failure in advice about IAM would make the one case this is for
// harder to spot rather than easier.
func advise(projectID string, err error) error {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusForbidden {
		return err
	}
	remedy := fmt.Sprintf("run `grain setup gcp -project %s -enable-gemini-key`, "+
		"which grants the minter roles/serviceusage.apiKeysAdmin and enables both "+
		"apikeys.googleapis.com and %s", projectID, DefaultAPITargetService)
	if isServiceDisabled(apiErr) {
		return fmt.Errorf("the API Keys API is not enabled in project %s -- %s: %w", projectID, remedy, err)
	}
	return fmt.Errorf("the minter credential is not permitted to administer API keys in "+
		"project %s (it needs roles/serviceusage.apiKeysAdmin) -- %s: %w", projectID, remedy, err)
}

// isServiceDisabled reports whether a 403 is Google's "this API has
// never been enabled in this project" rather than an IAM refusal. The
// machine-readable form is an ErrorInfo detail with reason
// SERVICE_DISABLED; googleapi.Error keeps the raw body, so this looks
// there rather than reaching for a typed detail the generated client
// does not expose.
func isServiceDisabled(apiErr *googleapi.Error) bool {
	return strings.Contains(apiErr.Body, "SERVICE_DISABLED") ||
		strings.Contains(apiErr.Message, "has not been used in project")
}
