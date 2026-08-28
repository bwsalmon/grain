// Package gcpkey is the "gcp-key" capability: a MINT provider
// (docs/data-model.md, model/capability.go) that mints a short-lived
// service-account key for a narrow "agent" service account and pushes it
// into the sandbox, mirroring grain/automation/gcp_keys.py's shape --
// bwsalmon/agents#126, ported to v2's declarative provider contract rather
// than v1's Runner-shelling-to-gcloud one.
//
// **A real key, not an impersonated token**, for the same reason
// gcp_keys.py gives: bwsalmon/agents#126 asks for exactly this tradeoff,
// mirroring gemini_keys.py's own "mint one per task, push the raw secret
// into the sandbox, revoke at end of session" shape. See that module's
// docstring for the fuller argument; nothing about the tradeoff changes
// here, only the mechanism.
//
// **Authentication: a minter credential, resolved fresh for every call,
// never the agent account's own.** Minting a *new* agent-account key has to
// be done by some identity other than the agent account itself -- a
// sandbox holding a leaked agent key must not be able to mint itself a
// fresh one and defeat the whole "expires" premise this package exists
// for. Provider.Config.MinterCredential names a credential
// (CapabilityContext.Credentials resolves it) that GCP's IAM grants
// roles/iam.serviceAccountKeyAdmin on the agent account and nothing wider
// -- gcp_keys.py's separate gcp-key-minter.json, never
// gcp-service-account.json, for the same reason. A fresh Minter is built
// from that material on every Materialize/Revoke/Reap rather than cached,
// the same reason gcp_keys.py's _activate runs before every gcloud
// invocation: two credentials authenticating as two different accounts
// must never leave which one "wins" to ambient global state.
//
// **"N-hour expiry" is enforced by grain, not GCP**, for the same reason
// gcp_keys.py's own docstring gives: user-managed IAM service-account keys
// carry no native TTL. Two independent mechanisms enforce it here, neither
// alone sufficient: Materialize sets Lease.ExpiresAt and
// CapabilitySpec.MaxLease from Config.MaxKeyAge, for whatever future sweep
// walks recorded Leases (docs/data-model.md's "an expiry reaper" row); and
// Reap, below, is the actual safety net -- it deletes whatever GCP itself
// reports as too old, independent of whether any record of it survived.
//
// **Directly against the IAM API, not gcloud.** v2/README.md already notes
// "the GCP Go SDK would retire the gcloud exception" as one of the two
// package-list wins Go buys over the Python controller; this package is
// that retirement for one capability. iam.go builds an
// *iam.Service straight from resolved credential material
// (google.golang.org/api/iam/v1), with no subprocess, no temp file on a
// controller's own disk, and no dependence on a particular gcloud version's
// stdout shape -- gcp_keys.py's create_key carries a long comment on
// exactly that last failure mode (bwsalmon/agents#140).
package gcpkey

import (
	"context"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/model"
)

// capabilityName is model.CapabilitySpec.Name and the string every Lease
// this package mints carries as Lease.Capability.
const capabilityName = "gcp-key"

// SandboxKeyPath is where the minted key lands in the sandbox --
// docs/system-diagram.md's "Sandbox GCP key" row, restated here rather than
// imported since v2 has no sandbox-facing config package yet for either
// side to share.
const SandboxKeyPath = "/home/debian/.gcp-service-account.json"

// DefaultMaxKeyAge is Config.MaxKeyAge's default, and
// grain/automation/gcp_keys.DEFAULT_MAX_KEY_AGE_HOURS restated in Go --
// bwsalmon/agents#126's own "these keys should have an expiry of 24 hours".
const DefaultMaxKeyAge = 24 * time.Hour

// DefaultMinterCredential is Config.MinterCredential's default: the name a
// CapabilityContext.Credentials resolver is expected to answer with the
// host service-account key gcp_keys.py calls the minter -- deliberately not
// the same name a deployment's general host identity might use elsewhere,
// for the separation this package's doc comment explains.
const DefaultMinterCredential = "gcp-key-minter"

// KeyInfo is one key as GCP itself reports it -- what Reap needs and
// nothing more.
type KeyInfo struct {
	ID        string
	CreatedAt time.Time
}

// Minter mints, deletes, and lists user-managed keys for one GCP service
// account, authenticated as some other identity permitted to administer
// them. account is always a fully-qualified resource name,
// "projects/{project}/serviceAccounts/{email}" -- callers never assemble a
// per-key name themselves, since Delete's shape
// ("{account}/keys/{id}") is Minter's own implementation detail.
//
// A provider is handed no cached Minter, only a constructor
// (Provider.NewMinter) called fresh with resolved credential material for
// each Materialize/Revoke/Reap -- see this package's doc comment for why.
type Minter interface {
	CreateKey(ctx context.Context, account string) (id, keyJSON string, err error)
	DeleteKey(ctx context.Context, account, keyID string) error
	ListKeys(ctx context.Context, account string) ([]KeyInfo, error)
}

// Config is this deployment's gcp-key settings -- the same
// operator-set-tunables role grain/automation/gcp_keys.GcpKeyConfig plays
// in v1, held on the Provider itself rather than threaded through
// CapabilityContext, since v2's CapabilityContext (model/capability.go)
// carries nothing capability-specific.
type Config struct {
	// ServiceAccountEmail is the narrow "agent" account keys are minted
	// for -- terraform/gcp/iam.tf's google_service_account.agent.
	ServiceAccountEmail string
	// ProjectID is the GCP project ServiceAccountEmail lives in.
	ProjectID string
	// MaxKeyAge is both the lease's own expiry (Materialize) and Reap's
	// cutoff. Zero means DefaultMaxKeyAge, never "no expiry" -- there is
	// no such thing this package will honour.
	MaxKeyAge time.Duration
	// MinterCredential is resolved through CapabilityContext.Credentials
	// (Materialize, PromptSection is uninvolved) to the host
	// service-account key permitted to mint/delete ServiceAccountEmail's
	// own keys. Empty means DefaultMinterCredential.
	MinterCredential string
}

// Provider is the gcp-key capability. The zero value is not usable --
// construct one with NewProvider, which fills in NewMinter.
type Provider struct {
	model.BaseCapability
	Config Config
	// NewMinter builds a Minter authenticated with credentialJSON, the
	// material CapabilityContext.Credentials.Resolve returned for
	// Config.MinterCredential (or a Lease's own MintedBy, for Revoke).
	// NewProvider sets this to NewIAMMinter; tests substitute a fake with
	// no network and no real GCP project involved, the same "no sandbox
	// and no cloud" bar model/capability_test.go's mintCapability holds
	// to.
	NewMinter func(ctx context.Context, credentialJSON string) (Minter, error)
}

// NewProvider builds a Provider wired to the real IAM API.
func NewProvider(cfg Config) *Provider {
	return &Provider{Config: cfg, NewMinter: NewIAMMinter}
}

func (p *Provider) maxKeyAge() time.Duration {
	if p.Config.MaxKeyAge > 0 {
		return p.Config.MaxKeyAge
	}
	return DefaultMaxKeyAge
}

func (p *Provider) minterCredential() string {
	if p.Config.MinterCredential != "" {
		return p.Config.MinterCredential
	}
	return DefaultMinterCredential
}

func (p *Provider) account() string {
	return accountName(p.Config.ProjectID, p.Config.ServiceAccountEmail)
}

func accountName(projectID, email string) string {
	return fmt.Sprintf("projects/%s/serviceAccounts/%s", projectID, email)
}

func (p *Provider) newMinter(ctx context.Context, credentialJSON string) (Minter, error) {
	build := p.NewMinter
	if build == nil {
		build = NewIAMMinter
	}
	minter, err := build(ctx, credentialJSON)
	if err != nil {
		return nil, fmt.Errorf("gcpkey: authenticating as the minter: %w", err)
	}
	return minter, nil
}

func (p *Provider) Spec() model.CapabilitySpec {
	return model.CapabilitySpec{
		Name:        capabilityName,
		Label:       "grain-gcp-key",
		Description: "Mint a short-lived GCP service-account key for this task",
		Source:      model.GrantByLabel,
		Provision:   model.ProvisionMint,
		MaxLease:    p.maxKeyAge(),
	}
}

// Resolve refuses unless this deployment is actually configured for
// gcp-key -- ServiceAccountEmail and ProjectID both come from an operator,
// never a default, since a mistaken default here would mint against
// whichever project a stray empty string happened to resolve to. The
// message names the real `grain controller configure` flags
// (docs/runbook.md's "Enabling GCP access in sandboxes"), the same
// "posted verbatim, so it should read as a sentence a human can act on"
// bar Resolution.Reason sets.
func (p *Provider) Resolve(ctx context.Context, cc model.CapabilityContext) (model.Resolution, error) {
	if p.Config.ServiceAccountEmail == "" || p.Config.ProjectID == "" {
		return model.RefusedBecause(
			"this issue is labelled `grain-gcp-key`, asking for a GCP service-account " +
				"key this deployment isn't configured for. An operator runs `grain " +
				"controller configure --gcp-agent-service-account-email <email> " +
				"--gcp-project-id <project>`.",
		), nil
	}
	return model.Honoured(), nil
}

func (p *Provider) Materialize(ctx context.Context, cc model.CapabilityContext) (model.Materialization, error) {
	credential := p.minterCredential()
	minterKey, err := cc.Credentials.Resolve(ctx, credential)
	if err != nil {
		return model.Materialization{}, fmt.Errorf("gcpkey: resolving minter credential %q: %w", credential, err)
	}
	minter, err := p.newMinter(ctx, minterKey)
	if err != nil {
		return model.Materialization{}, err
	}
	id, keyJSON, err := minter.CreateKey(ctx, p.account())
	if err != nil {
		return model.Materialization{}, fmt.Errorf("gcpkey: minting a key for %s: %w", p.account(), err)
	}
	expires := cc.Now.Add(p.maxKeyAge())
	return model.Materialization{
		Lease: &model.Lease{
			Capability: capabilityName,
			Resource:   id,
			MintedBy:   model.CredentialRef{Name: credential},
			IssuedAt:   cc.Now,
			ExpiresAt:  &expires,
		},
		Placements: []model.Placement{
			{Side: model.SideSandbox, Path: SandboxKeyPath, Content: keyJSON},
		},
	}, nil
}

func (p *Provider) PromptSection(ctx context.Context, cc model.CapabilityContext, placements []model.Placement) (string, error) {
	if len(placements) != 1 {
		return "", fmt.Errorf("gcpkey: expected exactly one placement, got %d", len(placements))
	}
	path := placements[0].Path
	return fmt.Sprintf(
		"A GCP service-account key is at %s, readable only by you:\n\n"+
			"    export GOOGLE_APPLICATION_CREDENTIALS=%q\n\n"+
			"or, for the gcloud CLI:\n\n"+
			"    gcloud auth activate-service-account --key-file=%s\n",
		path, path, path,
	), nil
}

// Revoke deletes the key Materialize minted, authenticating with whatever
// credential the Lease itself names (Lease.MintedBy) rather than
// Config.MinterCredential's current value -- Lease.MintedBy's own doc
// comment is exactly this: "releasing asks the lease what to call", so a
// credential rotated between mint and revoke does not strand the key this
// lease is naming.
func (p *Provider) Revoke(ctx context.Context, cc model.CapabilityContext, lease model.Lease) error {
	minterKey, err := cc.Credentials.Resolve(ctx, lease.MintedBy.Name)
	if err != nil {
		return fmt.Errorf("gcpkey: resolving minter credential %q: %w", lease.MintedBy.Name, err)
	}
	minter, err := p.newMinter(ctx, minterKey)
	if err != nil {
		return err
	}
	if err := minter.DeleteKey(ctx, p.account(), lease.Resource); err != nil {
		return fmt.Errorf("gcpkey: revoking key %s: %w", lease.Resource, err)
	}
	return nil
}

// Reap implements model.Reaper: it deletes every key under
// Config.ServiceAccountEmail that GCP itself reports as older than
// maxKeyAge, independent of any Lease grain may or may not still have a
// record of -- the actual "destroy keys more than 24 hours old" backstop,
// see this package's doc comment. Best-effort per key, the same "one
// already-gone key must not stop the rest" rule delete_expired_keys
// follows: a caller sweeping many capabilities in one pass must not have
// this one's transient GCP error abort keys that would otherwise have been
// caught. Returns the ids it actually deleted, for a caller to log.
//
// A key with a zero CreatedAt -- ListKeys could not parse GCP's own
// timestamp for it, never observed in practice -- is left alone rather
// than guessed at, the same "absent data loses, doesn't crash" stance
// gcp_keys.py's own delete_expired_keys takes with a missing
// validAfterTime.
func (p *Provider) Reap(ctx context.Context, creds model.CredentialResolver, now time.Time) ([]string, error) {
	credential := p.minterCredential()
	minterKey, err := creds.Resolve(ctx, credential)
	if err != nil {
		return nil, fmt.Errorf("gcpkey: resolving minter credential %q: %w", credential, err)
	}
	minter, err := p.newMinter(ctx, minterKey)
	if err != nil {
		return nil, err
	}
	account := p.account()
	keys, err := minter.ListKeys(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("gcpkey: listing keys for %s: %w", account, err)
	}
	maxAge := p.maxKeyAge()
	var deleted []string
	for _, key := range keys {
		if key.CreatedAt.IsZero() || now.Sub(key.CreatedAt) < maxAge {
			continue
		}
		if err := minter.DeleteKey(ctx, account, key.ID); err != nil {
			continue
		}
		deleted = append(deleted, key.ID)
	}
	return deleted, nil
}
