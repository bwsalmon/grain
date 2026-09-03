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
	"slices"
	"testing"
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
