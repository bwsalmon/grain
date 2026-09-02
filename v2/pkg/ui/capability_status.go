package ui

import (
	"slices"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/capability/bootstrap"
	"github.com/bwsalmon/grain/v2/pkg/capability/gcpkey"
	"github.com/bwsalmon/grain/v2/pkg/capability/geminikey"
	"github.com/bwsalmon/grain/v2/pkg/capability/githubsandbox"
	"github.com/bwsalmon/grain/v2/pkg/capability/selfdebug"
	"github.com/bwsalmon/grain/v2/pkg/capability/selfrepair"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
)

// CapabilityStatus is one capability's deployment-wide readiness, as
// shown on Settings' "Capabilities" tab -- distinct from the Capability
// type labels.go already exports, which is the per-task picker's
// attach/detach listing and says nothing about whether granting one
// would actually work. Ready mirrors what cmd/grain/daemon.go's own
// capabilityProviders would register plus, for a capability that
// registers unconditionally (github-sandbox, self-debug, self-repair,
// bootstrap-playbooks, per that function's own doc comment), whether
// every secret it
// resolves through CapabilityContext.Credentials is actually set --
// registering is not the same as working, and a task granted an
// unready capability today only discovers that later as a refused
// resolution or a failed materialize.
type CapabilityStatus struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Ready       bool   `json:"ready"`
	// MissingConfig is every deployment setting (this Settings tab's own
	// General fields) this capability still needs -- e.g. "GCP project"
	// for gcp-key/gemini-key. Empty for a capability with no such gate
	// (github-sandbox, self-debug, self-repair and bootstrap-playbooks
	// all register unconditionally).
	MissingConfig []string `json:"missingConfig,omitempty"`
	// MissingSecrets is every CapabilitySpec.Requires entry this
	// deployment's secrets store has no matching secret/key for -- the
	// same check TargetReposMissingCredentials already runs for the git
	// proxy's credential ladder, applied here to a capability's own
	// Requires instead. Always empty when Config.Secrets is nil (a UI
	// not colocated with the store this checks against has nothing
	// actionable to say, the same nil-means-unavailable contract that
	// field's own doc comment gives every other check built on it).
	MissingSecrets []string `json:"missingSecrets,omitempty"`
}

// capabilityDisplayNames gives each capabilityCatalog entry a short,
// human-facing name for Settings to show -- CapabilitySpec.Label is not
// it: that field is what used to be a GitHub label before v1's label
// taxonomy was retired, deliberately kept out of every wire shape this
// package sends since (TestConfigEndpointReportsActorAndCapabilities'
// own "the GitHub label a capability used to carry is gone from the
// wire shape along with the labels themselves"). gemini-key, self-debug
// and self-repair repeat the same names DefaultCapabilities already
// gives the per-task picker's own listing of the very same capabilities
// -- two different views of one capability, the same duplication
// DefaultCapabilities and each provider's own Spec().Description
// already accept for their descriptions.
var capabilityDisplayNames = map[string]string{
	"gcp-key":             "GCP key",
	"gemini-key":          "Gemini key",
	"github-sandbox":      "GitHub sandbox",
	"self-debug":          "Self debug",
	"self-repair":         "Self repair",
	"bootstrap-playbooks": "Bootstrap playbooks",
}

// capabilityCatalog is every capability grain ships a provider for,
// regardless of whether this deployment currently has enough
// configuration to register it -- unlike cmd/grain/daemon.go's own
// capabilityProviders, which only builds the ones a real dispatch can
// use, this exists purely to read each provider's own Spec() back out,
// so Settings has one place to learn a capability's id/name/description/
// requirements from rather than repeating them. Every constructor here
// is called with zero-value config on purpose: Spec() reads none of it,
// and gemini-key's own credential name is the same
// gcpkey.DefaultMinterCredential daemon.go always passes regardless of
// whether GCPProject is set, since that constant, not the project, is
// what Requires needs to name.
func capabilityCatalog() []model.CapabilitySpec {
	return []model.CapabilitySpec{
		gcpkey.NewProvider(gcpkey.Config{}).Spec(),
		geminikey.New("", model.CredentialRef{Name: gcpkey.DefaultMinterCredential}).Spec(),
		githubsandbox.NewProvider(githubsandbox.Config{}).Spec(),
		selfdebug.New().Spec(),
		selfrepair.New().Spec(),
		bootstrap.New().Spec(),
	}
}

// missingConfigFor is capabilityProviders' own gates
// (cmd/grain/daemon.go), read back as the human-facing labels Settings'
// Capabilities tab already uses for the same two fields -- gcp-key needs
// both a project and the service account it mints keys for; gemini-key
// needs only the project, sharing it with whatever standing credential
// mints under it.
func missingConfigFor(name string, cfg model.Config) []string {
	switch name {
	case "gcp-key":
		var missing []string
		if cfg.GCPProject == "" {
			missing = append(missing, "GCP project")
		}
		if cfg.GCPServiceAccountEmail == "" {
			missing = append(missing, "GCP service account email")
		}
		return missing
	case "gemini-key":
		if cfg.GCPProject == "" {
			return []string{"GCP project"}
		}
	}
	return nil
}

// missingSecretsFor is requires filtered down to the names list has no
// covering secret/key for, following the exact two forms
// secrets.Store.Resolve itself accepts: "<secret>/<key>" names one key
// directly, and a bare "<secret>" resolves only when that secret holds
// exactly one key -- so a secret sitting there with the wrong number of
// keys is reported missing here the same way Resolve would refuse it.
func missingSecretsFor(requires []string, list []secrets.SecretInfo) []string {
	byName := make(map[string][]string, len(list))
	for _, s := range list {
		byName[s.Name] = s.Keys
	}
	var missing []string
	for _, name := range requires {
		secret, key, explicit := strings.Cut(name, "/")
		keys, ok := byName[secret]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if explicit {
			if !slices.Contains(keys, key) {
				missing = append(missing, name)
			}
			continue
		}
		if len(keys) != 1 {
			missing = append(missing, name)
		}
	}
	return missing
}

// capabilityStatuses builds every CapabilityStatus for cfg -- the
// deployment's current store-backed settings -- and c.Config.Secrets,
// this Client's own secrets store, if any.
func (c *Client) capabilityStatuses(cfg model.Config) []CapabilityStatus {
	var secretList []secrets.SecretInfo
	if c.Config.Secrets != nil {
		// Best-effort: a listing error here would only ever be a store
		// this Client could not open, which GetSettings/UpdateSettings
		// have no other reason to fail on -- reported as "nothing known
		// to be missing" rather than surfaced as an error this endpoint
		// never returned before.
		if list, err := c.Config.Secrets.List(); err == nil {
			secretList = list
		}
	}

	catalog := capabilityCatalog()
	out := make([]CapabilityStatus, 0, len(catalog))
	for _, spec := range catalog {
		status := CapabilityStatus{
			ID:            spec.Name,
			Name:          capabilityDisplayNames[spec.Name],
			Description:   spec.Description,
			MissingConfig: missingConfigFor(spec.Name, cfg),
		}
		if c.Config.Secrets != nil {
			status.MissingSecrets = missingSecretsFor(spec.Requires, secretList)
		}
		status.Ready = len(status.MissingConfig) == 0 && len(status.MissingSecrets) == 0
		out = append(out, status)
	}
	return out
}
