package ui

// The picker listing (OfferedCapabilities, labels.go) and the set of
// capabilities grain actually ships providers for (capabilityCatalog,
// capability_status.go) are two hand-maintained tables with nothing in
// the type system tying them together, and they drifted apart in both
// directions at once: gcp-key and github-sandbox had providers and no
// picker row, so no task on any deployment could be granted either;
// "scratch-repo" had a picker row and no provider, so a task granted it
// failed every dispatch at model.ResolveGrants. These tests are the tie.
//
// They live in package ui rather than ui_test because capabilityCatalog
// and capabilityDisplayNames are unexported -- the catalog is the thing
// worth comparing against, since it reads each provider's own Spec()
// rather than repeating a third list.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/bwsalmon/grain/pkg/capability/githubtoken"
	"github.com/bwsalmon/grain/pkg/model"
)

// capabilityIDs is the picker listing's ids, in table order.
func capabilityIDs() []string {
	var ids []string
	for _, c := range OfferedCapabilities() {
		ids = append(ids, c.ID)
	}
	return ids
}

// catalogIDs is the shipped providers' ids, in catalog order.
func catalogIDs() []string {
	var ids []string
	for _, spec := range capabilityCatalog() {
		ids = append(ids, spec.Name)
	}
	return ids
}

func TestOfferedCapabilitiesCoversEveryShippedCapability(t *testing.T) {
	offered, shipped := capabilityIDs(), catalogIDs()
	for _, id := range shipped {
		if !slices.Contains(offered, id) {
			t.Errorf("capability %q has a provider but no OfferedCapabilities row: "+
				"grantsFor and SetCapability reject it as an unknown capability, so no task can ever be granted it "+
				"and its provider is never resolved, never materialized, and places nothing in any sandbox", id)
		}
	}
	for _, id := range offered {
		if !slices.Contains(shipped, id) {
			t.Errorf("capability %q is offered by OfferedCapabilities but grain ships no provider for it: "+
				"a task granted it is refused by model.ResolveGrants (\"no provider is registered\"), "+
				"which prepareCapabilities turns into a failed dispatch", id)
		}
	}
}

// Settings' Capabilities tab and the per-task picker name the same
// capabilities twice (capability_status.go's own note on why the
// duplication is accepted). Two views disagreeing about what a
// capability is called is the cheap half of the same drift.
func TestOfferedCapabilitiesNamesMatchSettingsDisplayNames(t *testing.T) {
	for _, c := range OfferedCapabilities() {
		if c.Description == "" {
			t.Errorf("capability %q has no description for the picker to show", c.ID)
		}
		want, ok := capabilityDisplayNames[c.ID]
		if !ok {
			t.Errorf("capability %q has no capabilityDisplayNames entry, so Settings shows it with a blank name", c.ID)
			continue
		}
		if c.Name != want {
			t.Errorf("capability %q: picker name %q, Settings name %q -- one capability, two names", c.ID, c.Name, want)
		}
	}
}

// The third drift of the same kind, and the one grain/task-172 is about:
// a capability that resolves a standing credential -- one an operator
// pasted once, which every later mint authenticates as -- but ships no
// way to test it. Settings can only ever say **Ready** for such a
// capability, meaning "the secret is set", while the key inside it may
// have been deleted or rotated away at the far end months ago. Every
// capability with a Requires entry is one that can fail that way, so
// every one of them owes a model.CredentialChecker.
func TestEveryCapabilityWithAStandingCredentialCanBeChecked(t *testing.T) {
	for _, p := range capabilityProviderCatalog() {
		spec := p.Spec()
		if len(spec.Requires) == 0 {
			continue
		}
		if !capabilityCheckable(spec.Name) {
			t.Errorf("capability %q resolves the standing credential(s) %v but implements no "+
				"model.CredentialChecker: Settings can report it Ready and nothing on any pane "+
				"can see whether what is in those secrets still works", spec.Name, spec.Requires)
		}
	}
}

// The same bar, held to the one listing on this pane that is not
// capabilityProviderCatalog: a named GitHub token (githubTokenStatuses).
// Those rows are deliberately not in the catalog -- which tokens exist
// is an operator's own files, not a property of this build -- so the
// test above cannot see them, and for a while nothing could: they
// reported Ready by construction, and a token revoked or rotated at
// GitHub's end went stale with nothing on any pane able to say so
// (grain/task-189).
//
// Two halves, because the drift can happen in either: the provider grain
// builds for a named token has to implement model.CredentialChecker, and
// the row this package builds for that same token has to report itself
// Checkable, or the pane offers no button to reach the check through.
func TestEveryNamedGitHubTokenRowCanBeChecked(t *testing.T) {
	if _, ok := any(githubtoken.New("release-bot", githubtoken.Config{})).(model.CredentialChecker); !ok {
		t.Fatal("githubtoken.Provider implements no model.CredentialChecker: a named token's row can " +
			"only ever say Ready, and nothing on any pane can see whether GitHub still accepts it")
	}
	c := &Client{Config: Config{
		Capabilities:     append(OfferedCapabilities(), GitHubTokenCapabilities([]string{"release-bot"})...),
		CapabilityChecks: unreachedChecker{},
	}}
	var rows int
	for _, status := range c.capabilityStatuses(model.Config{}, nil) {
		if _, ok := model.GitCredentialName(status.ID); !ok {
			continue
		}
		rows++
		if !status.Checkable {
			t.Errorf("named token %q reports Checkable false, so Settings offers no way to test a "+
				"credential it reports Ready by construction", status.ID)
		}
	}
	if rows != 1 {
		t.Fatalf("built %d named-token rows, want one -- the listing this test is about is missing", rows)
	}
}

// unreachedChecker satisfies Config.CapabilityChecks without being a
// place a check could actually go: what the tests above need from it is
// that this deployment *has* one (the nil-means-unavailable contract),
// not what it would answer.
type unreachedChecker struct{}

func (unreachedChecker) CheckCapability(ctx context.Context, id string) (CapabilityCheckResult, error) {
	return CapabilityCheckResult{}, errors.New("no check should have been made")
}

// The other direction: a capability with nothing standing behind it must
// not offer a check, since there would be nothing for one to authenticate
// as and the button would only ever produce a confusing failure.
func TestCapabilitiesWithNoCredentialOfferNoCheck(t *testing.T) {
	for _, p := range capabilityProviderCatalog() {
		spec := p.Spec()
		if len(spec.Requires) == 0 && capabilityCheckable(spec.Name) {
			t.Errorf("capability %q needs no credential but ships a credential check", spec.Name)
		}
	}
}
