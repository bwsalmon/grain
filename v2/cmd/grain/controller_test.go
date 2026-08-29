package main

import (
	"strings"
	"testing"
)

func TestAPIHostRewritesGitHubDotComOnly(t *testing.T) {
	if got := apiHost("github.com"); got != "api.github.com" {
		t.Errorf("apiHost(github.com) = %q, want api.github.com", got)
	}
	if got := apiHost("mock.example"); got != "mock.example" {
		t.Errorf("apiHost(mock.example) = %q, want it left unchanged for a test/mock host", got)
	}
}

func TestManifestSubmissionFormEscapesTheManifest(t *testing.T) {
	form := manifestSubmissionForm("https://github.com/settings/apps/new?state=abc", `{"name":"a\"b"}`)
	if !strings.Contains(form, "https://github.com/settings/apps/new?state=abc") {
		t.Errorf("form does not name the submit URL: %s", form)
	}
	if strings.Contains(form, `"a\"b"`) {
		t.Errorf("form embeds an unescaped quote from the manifest, which would break out of the value attribute: %s", form)
	}
	if !strings.Contains(form, "&quot;") {
		t.Errorf("form does not escape quotes in the manifest: %s", form)
	}
}

func TestRandomHexIsUnpredictableAndTheRightLength(t *testing.T) {
	a, err := randomHex(16)
	if err != nil {
		t.Fatal(err)
	}
	b, err := randomHex(16)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 {
		t.Errorf("randomHex(16) has length %d, want 32 hex characters", len(a))
	}
	if a == b {
		t.Errorf("two calls to randomHex(16) produced the same value")
	}
}
