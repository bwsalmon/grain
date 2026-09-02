package model

import "testing"

func TestGitCredentialOverrideFindsItsOwnGrant(t *testing.T) {
	grants := []Grant{
		{Capability: "gemini-key", Via: GrantByFolder},
		GitCredentialGrant("workflow"),
	}
	name, ok := gitCredentialOverride(grants)
	if !ok || name != "workflow" {
		t.Errorf("name=%q ok=%v, want %q true", name, ok, "workflow")
	}
}

func TestGitCredentialOverrideIgnoresUnrelatedGrants(t *testing.T) {
	grants := []Grant{{Capability: "gemini-key", Via: GrantByFolder}}
	if _, ok := gitCredentialOverride(grants); ok {
		t.Error("an unrelated capability should not be read as an override")
	}
}

func TestGitCredentialOverrideIsAbsentWithNoGrants(t *testing.T) {
	if _, ok := gitCredentialOverride(nil); ok {
		t.Error("no grants at all should not be read as an override")
	}
}

func TestGitCredentialGrantIsAppliedByLabel(t *testing.T) {
	g := GitCredentialGrant("workflow")
	if g.Via != GrantByLabel {
		t.Errorf("Via = %q, want %q -- a grain-github-<name> label is what produces this grant", g.Via, GrantByLabel)
	}
}

func TestGrantsSubsetOfIsTrueWithNoGrantsAtAll(t *testing.T) {
	if !GrantsSubsetOf(nil, nil) {
		t.Error("no grants requested is a subset of no grants allowed")
	}
	if !GrantsSubsetOf(nil, []Grant{{Capability: "gemini-key", Via: GrantByLabel}}) {
		t.Error("no grants requested is a subset of any allowed set")
	}
}

func TestGrantsSubsetOfComparesByCapabilityNameOnly(t *testing.T) {
	grants := []Grant{{Capability: "gemini-key", Via: GrantByDirective}}
	allowed := []Grant{{Capability: "gemini-key", Via: GrantByLabel, Folder: ParseFolder("acme/widgets")}}
	if !GrantsSubsetOf(grants, allowed) {
		t.Error("same capability name should match regardless of Via or Folder")
	}
}

func TestGrantsSubsetOfIsFalseForAWiderRequest(t *testing.T) {
	grants := []Grant{{Capability: "gemini-key", Via: GrantByLabel}, {Capability: "self-repair", Via: GrantByLabel}}
	allowed := []Grant{{Capability: "gemini-key", Via: GrantByLabel}}
	if GrantsSubsetOf(grants, allowed) {
		t.Error("a capability not in allowed should make this false")
	}
}
