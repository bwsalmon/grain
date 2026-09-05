package ui_test

// grain/task-4: the credential *ladder* -- which repos reach which
// credential -- is editable from the same pane as the material now.
// Editing secrets/github/credentials.json by hand was the last step of
// standing a deployment up that needed a shell on the host, and it was
// the step nothing in the UI told you was missing.

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// patternRow is one credentials.json entry in a github-tokens body.
func patternRow(t *testing.T, body map[string]any, pattern string) map[string]any {
	t.Helper()
	rows, _ := body["patterns"].([]any)
	for _, row := range rows {
		row, _ := row.(map[string]any)
		if row["pattern"] == pattern {
			return row
		}
	}
	t.Fatalf("no ladder entry for %q in %v", pattern, body)
	return nil
}

// The state a fresh deployment is in, and the whole of what it takes to
// leave it: paste one token, and every repo resolves to it. Nothing
// about this is a second decision an operator could be expected to know
// to make.
func TestTheFirstTokenAddedBecomesTheDeploymentDefault(t *testing.T) {
	srv, credentials, dir := gitHubTokenServer(t, nil, nil)

	rec := do(t, srv, http.MethodPut, "/api/github-tokens/bot", `{"value":"ghp-fake"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	body := tokenBody(t, rec)
	if body["defaultName"] != "bot" {
		t.Errorf("defaultName = %v, want bot", body["defaultName"])
	}
	if row := patternRow(t, body, "*"); row["credential"] != "bot" {
		t.Errorf("* entry = %v, want it to name bot", row)
	}
	if credentials.DefaultName() != "bot" {
		t.Errorf("DefaultName() = %q, want bot", credentials.DefaultName())
	}
	// On disk, where a git proxy in another process reads it.
	data, err := os.ReadFile(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatalf("credentials.json was not written: %v", err)
	}
	if !strings.Contains(string(data), `"bot"`) {
		t.Errorf("credentials.json = %s, want it to name bot", data)
	}
}

// A second token is an addition, not a promotion: the deployment's
// default is a choice already made, and adding a scoped token must never
// quietly move every repo onto it.
func TestASecondTokenDoesNotTakeOverTheDefault(t *testing.T) {
	srv, credentials, _ := gitHubTokenServer(t,
		map[string]string{"*": "bot"}, map[string]string{"bot": "bot-token"})

	rec := do(t, srv, http.MethodPut, "/api/github-tokens/release-bot", `{"value":"ghp-release"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if credentials.DefaultName() != "bot" {
		t.Fatalf("DefaultName() = %q, want the default left where it was", credentials.DefaultName())
	}
	if got := credentials.PatternsFor("release-bot"); len(got) != 0 {
		t.Errorf("PatternsFor(release-bot) = %v, want no ladder entry", got)
	}
}

// Pointing one repo at a token that is not the default: the case named
// tokens exist for, done without touching a file.
func TestSettingAPatternPointsARepoAtAnotherCredential(t *testing.T) {
	srv, credentials, _ := gitHubTokenServer(t,
		map[string]string{"*": "bot"},
		map[string]string{"bot": "bot-token", "release-bot": "release-token"})

	rec := do(t, srv, http.MethodPut, "/api/github-credential-patterns",
		`{"pattern":"ACME/Widgets","credential":"release-bot"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	// Canonicalized on the way in, so it matches what the proxy looks up.
	if row := patternRow(t, tokenBody(t, rec), "acme/widgets"); row["credential"] != "release-bot" {
		t.Errorf("acme/widgets = %v, want release-bot", row)
	}
	cred, ok := credentials.Select("acme", "widgets")
	if !ok || cred.Name != "release-bot" {
		t.Fatalf("Select(acme, widgets) = %+v, %v, want release-bot", cred, ok)
	}

	// And removing it puts those repos back on the default.
	rec = do(t, srv, http.MethodDelete,
		"/api/github-credential-patterns?pattern="+url.QueryEscape("acme/widgets"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if cred, ok := credentials.Select("acme", "widgets"); !ok || cred.Name != "bot" {
		t.Fatalf("Select(acme, widgets) = %+v, %v, want the default back", cred, ok)
	}
}

func TestSettingAPatternIsRefusedForACredentialThatIsNotThere(t *testing.T) {
	srv, credentials, _ := gitHubTokenServer(t,
		map[string]string{"*": "bot"}, map[string]string{"bot": "bot-token"})

	for _, body := range []string{
		`{"pattern":"acme/*","credential":"typo"}`,
		`{"pattern":"acme/wid*","credential":"bot"}`,
		`{"pattern":"","credential":"bot"}`,
	} {
		rec := do(t, srv, http.MethodPut, "/api/github-credential-patterns", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s status = %d, want 400: %s", body, rec.Code, rec.Body)
		}
	}
	if got := credentials.Patterns(); len(got) != 1 {
		t.Errorf("Patterns() = %v, want the ladder untouched", got)
	}
	rec := do(t, srv, http.MethodDelete, "/api/github-credential-patterns?pattern=acme%2F*", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("DELETE of an entry that is not there = %d, want 400: %s", rec.Code, rec.Body)
	}
}

// A ladder entry whose credential is gone is the drift that fails every
// push it covers -- listed as such rather than looking like any other
// row.
func TestAPatternNamingAMissingCredentialIsFlagged(t *testing.T) {
	srv, _, _ := gitHubTokenServer(t,
		map[string]string{"*": "bot", "acme/*": "vanished"}, map[string]string{"bot": "bot-token"})

	body := tokenBody(t, do(t, srv, http.MethodGet, "/api/github-tokens", ""))
	if row := patternRow(t, body, "acme/*"); row["missing"] != true {
		t.Errorf("acme/* = %v, want it flagged as naming no configured credential", row)
	}
	if row := patternRow(t, body, "*"); row["missing"] == true {
		t.Errorf("* = %v, want it not flagged", row)
	}
}

func TestGitHubCredentialPatternsUnavailableWithoutALadder(t *testing.T) {
	srv, _ := testServer(t)

	for _, call := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/github-credential-patterns", `{"pattern":"*","credential":"bot"}`},
		{http.MethodDelete, "/api/github-credential-patterns?pattern=*", ""},
	} {
		rec := do(t, srv, call.method, call.path, call.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404: %s", call.method, call.path, rec.Code, rec.Body)
		}
	}
}
