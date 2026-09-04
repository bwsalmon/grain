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

func TestGitCredentialOverridePicksOneWinnerDeterministically(t *testing.T) {
	// Two named tokens on one task: the proxy uses one of them rather
	// than refusing, and which one follows the order the grants arrive
	// in -- Store.GitCredentialOverride reads them sorted by capability
	// id, so the same task resolves the same way on every request.
	grants := []Grant{
		GitCredentialGrant("bot-two"),
		GitCredentialGrant("release-bot"),
	}
	name, ok := gitCredentialOverride(grants)
	if !ok || name != "bot-two" {
		t.Errorf("name=%q ok=%v, want %q true", name, ok, "bot-two")
	}
}

func TestGitCredentialCapabilityRoundTrips(t *testing.T) {
	id := GitCredentialCapability("release-bot")
	if id != "github-credential:release-bot" {
		t.Errorf("GitCredentialCapability = %q, want %q", id, "github-credential:release-bot")
	}
	if name, ok := GitCredentialName(id); !ok || name != "release-bot" {
		t.Errorf("GitCredentialName(%q) = %q, %v, want %q true", id, name, ok, "release-bot")
	}
	if _, ok := GitCredentialName("gemini-key"); ok {
		t.Error("an ordinary capability id should not read as a named GitHub token")
	}
	if _, ok := GitCredentialName(GitCredentialCapabilityPrefix); ok {
		t.Error("the bare prefix names no credential and should not read as one")
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

// ParseRepo's own gate: a paste that is not owner/name is either
// normalised to it or refused here, because nothing downstream of this
// function looks at a RepoRef again before building a clone URL out of
// it. Found by hand (task 244): `grain create -repo
// https://github.com/bwsalmon/grain` used to file a task whose owner
// was "https:".
func TestParseRepoNormalisesTheFormsAnOperatorPastes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"acme/widgets", "acme/widgets"},
		{"  acme/widgets  ", "acme/widgets"},
		{"https://github.com/acme/widgets", "acme/widgets"},
		{"https://github.com/acme/widgets/", "acme/widgets"},
		{"https://github.com/acme/widgets.git", "acme/widgets"},
		{"http://github.example.com/acme/widgets", "acme/widgets"},
		{"git@github.com:acme/widgets.git", "acme/widgets"},
		{"ssh://git@github.com/acme/widgets.git", "acme/widgets"},
	} {
		got, err := ParseRepo(tc.in)
		if err != nil {
			t.Errorf("ParseRepo(%q): %v", tc.in, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("ParseRepo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseRepoRefusesWhatWouldNotSurviveBeingPutInAURL(t *testing.T) {
	for _, in := range []string{
		"",
		"not-a-repo",
		"acme/",
		"/widgets",
		"acme/widgets/extra",
		"https://github.com/acme",
		"acme/wid gets",
		"acme/../etc",
		"../acme/widgets",
		"acme/widgets#fragment",
		"acme/widgets?query",
		"acme/wid\ngets",
	} {
		if got, err := ParseRepo(in); err == nil {
			t.Errorf("ParseRepo(%q) = %q, want an error", in, got)
		}
	}
}
