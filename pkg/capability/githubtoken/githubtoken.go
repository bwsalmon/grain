// Package githubtoken is the capability behind each named GitHub token a
// deployment has configured beyond its default one (grain/task-117): a
// task granted one pushes and pulls through that token instead of the
// one its repo would otherwise fall back to.
//
// **One provider per token, not one provider with a name argument in the
// grant.** A grant is a capability id and nothing else (model.Grant), so
// "use the release-bot token" has to *be* an id -- model.
// GitCredentialCapability's "github-credential:<name>" -- rather than a
// parameter carried alongside one. That is what lets the whole existing
// machinery work unchanged: the picker offers a row per token
// (ui.GitHubTokenCapabilities), a human ticks one, and the id lands in
// task_grant like any other. Providers builds the matching set of
// providers from the same names, so a granted token is one
// model.ResolveGrants finds a provider for rather than refusing as
// unregistered.
//
// **A SELECT capability: it mints nothing and places nothing.** The
// token never enters the sandbox -- pkg/gitproxy's whole shape is that a
// run's route to GitHub is grain's, never the agent's -- so there is
// nothing here to materialize into a file and nothing to revoke.
// What actually takes effect is the proxy asking the store, per request,
// which credential this sandbox's live task named
// (model.Store.GitCredentialOverride -> gitproxy.GitProxy.
// selectCredential), and that reads the grant itself. Materialize
// records the choice as Materialization.CredentialOverride all the same,
// which is the field model/capability.go defines for exactly this kind
// of capability, so a dispatch's own record of what it applied says
// which token the run was routed through.
//
// **Which tokens exist is an operator's file, not a setting.** The
// names come from the credential ladder's own directory
// (gitproxy.CredentialSet.ExtraNames -- a <name>.token or
// <name>.app.json under secrets/github), the same place the default
// token lives, and are read once at startup like the rest of that
// ladder: "replace a file under /data/secrets and restart the one
// service that reads it" (docs/design.md). Adding a token is therefore
// adding a file; what this package adds is that the file is then
// something a human can attach to one task without editing
// credentials.json and changing every repo's default at once.
package githubtoken

import (
	"context"
	"fmt"

	"github.com/bwsalmon/grain/pkg/model"
)

// Provider is the capability for one named GitHub token.
//
// Resolve, Revoke and (below) most of the contract are
// model.BaseCapability's defaults: there is no credential to withhold at
// resolve time -- the token was named by an operator's own file, and a
// name with no file behind it is never offered in the first place -- and
// nothing minted to give back.
type Provider struct {
	model.BaseCapability
	name string
}

// New returns the provider for the credential named name --
// gitproxy.CredentialSet's own name for it, which is the file name under
// secrets/github with its .token/.app.json suffix removed.
func New(name string) *Provider { return &Provider{name: name} }

// Providers is one Provider per name, in the order given -- what
// cmd/grain/daemon.go registers alongside the capabilities grain ships
// unconditionally. An empty or duplicate name is skipped rather than
// registered: the first would be a capability id no human could tell
// apart from the prefix itself, and the second would silently replace
// the provider registered before it (model.CapabilityRegistry.Register).
func Providers(names []string) []model.CapabilityProvider {
	seen := make(map[string]bool, len(names))
	out := make([]model.CapabilityProvider, 0, len(names))
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, New(name))
	}
	return out
}

// Name is the credential this provider stands for.
func (p *Provider) Name() string { return p.name }

func (p *Provider) Spec() model.CapabilitySpec {
	return model.CapabilitySpec{
		Name:        model.GitCredentialCapability(p.name),
		Label:       "grain-github-" + p.name,
		Description: Description(p.name),
		Source:      model.GrantByLabel,
		Provision:   model.ProvisionSelect,
	}
}

// Description is what a picker shows beside this capability, shared with
// ui.GitHubTokenCapabilities so the row a human ticks and the spec the
// daemon registers say the same thing about the same token.
func Description(name string) string {
	return fmt.Sprintf(
		"Push and pull through the %q GitHub token instead of this deployment's default one", name)
}

// Materialize routes this run's git traffic through the named credential
// -- see this package's doc comment on why that is a recorded choice
// rather than an effect applied here.
func (p *Provider) Materialize(context.Context, model.CapabilityContext) (model.Materialization, error) {
	return model.Materialization{CredentialOverride: &model.CredentialRef{Name: p.name}}, nil
}

// PromptSection tells the agent which token its pushes carry, since that
// is the difference it can actually observe: a push that the default
// token would have been refused for is one this run may be allowed to
// make, and the reverse is just as possible.
func (p *Provider) PromptSection(context.Context, model.CapabilityContext, []model.Placement) (string, error) {
	return fmt.Sprintf(
		"Git operations in this sandbox are authenticated with the %q GitHub token, which this "+
			"task was granted in place of the default one. You never hold the token itself: git "+
			"traffic goes through grain's proxy, which attaches it. If a push or fetch is refused, "+
			"it is that token's access that is missing, and saying so is more useful than retrying.",
		p.name), nil
}
