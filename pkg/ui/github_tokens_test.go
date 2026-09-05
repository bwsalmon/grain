package ui_test

// grain/task-117: a deployment's extra named GitHub tokens are
// capabilities like any other -- offered by the picker, attachable to a
// task, reportable on Settings' Capabilities tab -- and the point of
// giving each one an id rather than building a second, token-shaped
// pane is that none of the machinery in between has to know they exist.
// These tests are that claim, from the offered row through to the
// override the git proxy reads back off the task.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/gitproxy"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

// withTokenCapabilities is testClient with the picker listing a real
// deployment would have on top of the fixed rows: one per named token
// cmd/grain/daemon.go found under secrets/github.
func withTokenCapabilities(t *testing.T, names ...string) (*ui.Client, *model.Store, context.Context) {
	t.Helper()
	c, store, ctx := testClient(t)
	c.Config.Capabilities = append(ui.OfferedCapabilities(), ui.GitHubTokenCapabilities(names)...)
	return c, store, ctx
}

func TestGitHubTokenCapabilitiesIsOneRowPerNamedToken(t *testing.T) {
	got := ui.GitHubTokenCapabilities([]string{"release-bot", "", "docs-bot"})
	want := []ui.Capability{
		{ID: "github-credential:release-bot", Name: "GitHub token: release-bot",
			Description: got[0].Description},
		{ID: "github-credential:docs-bot", Name: "GitHub token: docs-bot",
			Description: got[1].Description},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GitHubTokenCapabilities = %+v, want %+v (an empty name is skipped)", got, want)
	}
	for _, capability := range got {
		if capability.Description == "" {
			t.Errorf("%s: no description for the picker to show", capability.ID)
		}
	}
}

func TestGitHubTokenCapabilitiesAreNoneWithoutExtraTokens(t *testing.T) {
	if got := ui.GitHubTokenCapabilities(nil); len(got) != 0 {
		t.Errorf("GitHubTokenCapabilities(nil) = %+v, want none -- a deployment with only a default token offers no rows", got)
	}
}

// The whole path a human takes: tick the token on a task, and the git
// proxy resolving that task's sandbox is told to use it in place of the
// owner/repo ladder.
func TestAttachingATokenCapabilityOverridesTheSandboxCredential(t *testing.T) {
	c, store, ctx := withTokenCapabilities(t, "release-bot")
	task := create(t, c, ctx)

	if err := c.SetCapability(ctx, task.ID, "github-credential:release-bot", true); err != nil {
		t.Fatalf("attaching the token capability: %v", err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "sandbox-0", Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}

	name, ok, err := store.GitCredentialOverride(ctx, "sandbox-0")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || name != "release-bot" {
		t.Fatalf("GitCredentialOverride = %q, %v, want %q true", name, ok, "release-bot")
	}

	// Detaching puts the sandbox back on the default ladder, the same as
	// any other capability coming off a task.
	if err := c.SetCapability(ctx, task.ID, "github-credential:release-bot", false); err != nil {
		t.Fatalf("detaching the token capability: %v", err)
	}
	if _, ok, err := store.GitCredentialOverride(ctx, "sandbox-0"); err != nil || ok {
		t.Fatalf("GitCredentialOverride ok=%v err=%v, want false, nil after detaching", ok, err)
	}
}

// A token this deployment does not have is rejected the same way any
// other unknown capability id is -- the picker listing is still the one
// gate every grant passes through.
func TestAttachingAnUnconfiguredTokenIsRejected(t *testing.T) {
	c, _, ctx := withTokenCapabilities(t, "release-bot")
	task := create(t, c, ctx)
	if err := c.SetCapability(ctx, task.ID, "github-credential:not-configured", true); err == nil {
		t.Fatal("expected attaching a token this deployment has no credential for to be rejected")
	}
}

func TestSettingsReportsEachNamedTokenAsAReadyCapability(t *testing.T) {
	c, _, ctx := withTokenCapabilities(t, "release-bot")

	got, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := capabilityStatus(t, got.Capabilities, "github-credential:release-bot")
	if !status.Ready || !status.Grantable {
		t.Fatalf("release-bot: Ready=%t Grantable=%t, want both true (status: %+v)",
			status.Ready, status.Grantable, status)
	}
	if status.Name != "GitHub token: release-bot" {
		t.Errorf("Name = %q, want the same name the picker shows", status.Name)
	}
	if status.Default {
		t.Errorf("Default = true, want false -- nothing has defaulted this token (status: %+v)", status)
	}
}

// Defaulting one deployment-wide goes through the same validation and
// the same reporting as every other capability id.
func TestANamedTokenCanBeADeploymentDefault(t *testing.T) {
	c, _, ctx := withTokenCapabilities(t, "release-bot")
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	defaults := []string{"github-credential:release-bot"}
	got, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{DefaultCapabilities: &defaults})
	if err != nil {
		t.Fatalf("defaulting a named token: %v", err)
	}
	if !slices.Equal(got.DefaultCapabilities, defaults) {
		t.Fatalf("defaultCapabilities = %v, want %v", got.DefaultCapabilities, defaults)
	}
	if status := capabilityStatus(t, got.Capabilities, "github-credential:release-bot"); !status.Default {
		t.Errorf("Default = false on a token every new task is filed with (status: %+v)", status)
	}

	// And a task filed afterwards really does carry it, which is what
	// makes the git proxy use that token for its pushes.
	task := create(t, c, ctx)
	if !slices.Contains(task.Capabilities, "github-credential:release-bot") {
		t.Fatalf("a task filed with the token defaulted holds %v", task.Capabilities)
	}
}

// --- adding and removing a named token from the UI --------------------
//
// grain/task-137: task-117 above made a named token grantable; this half
// makes one definable without shell access to the host. The store behind
// these endpoints is the credential ladder's own directory
// (pkg/gitproxy), not the secrets database -- see that package's doc
// comment for why -- so these assert on the files a git proxy would
// actually read back.

// gitHubTokenServer is a Server fronting a real credential directory:
// patterns become credentials.json, tokens become <name>.token files,
// and offered is the named tokens this "process" started with as
// capabilities (what cmd/grain/daemon.go computes once at startup).
func gitHubTokenServer(t *testing.T, patterns, tokens map[string]string, offered ...string) (*ui.Server, *gitproxy.CredentialSet, string) {
	t.Helper()
	dir := t.TempDir()
	if patterns != nil {
		data, err := json.Marshal(patterns)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, token := range tokens {
		if err := os.WriteFile(filepath.Join(dir, name+".token"), []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	credentials, err := gitproxy.LoadCredentialSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	client, _, _ := testClient(t)
	client.Config.Credentials = credentials
	client.Config.Capabilities = append(ui.OfferedCapabilities(), ui.GitHubTokenCapabilities(offered)...)
	return ui.NewServerWithClient(client), credentials, dir
}

// tokenBody decodes a github-tokens response loosely, so these tests
// read the JSON an actual browser gets rather than a Go struct this
// package could rename out from under it. Decoded once per response --
// decode drains the recorder's body, so a second call would see nothing.
func tokenBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	return decode[map[string]any](t, rec)
}

// tokenRow is one credential's entry in that body.
func tokenRow(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	rows, _ := body["tokens"].([]any)
	for _, row := range rows {
		row, _ := row.(map[string]any)
		if row["name"] == name {
			return row
		}
	}
	t.Fatalf("no row for %q in %v", name, body)
	return nil
}

func TestGitHubTokensUnavailableWithoutACredentialLadder(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/github-tokens", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := decode[map[string]any](t, rec); got["enabled"] != false {
		t.Fatalf("enabled = %v, want false -- this UI has no credential directory", got["enabled"])
	}
	for _, call := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/github-tokens/release-bot", `{"value":"ghp-fake"}`},
		{http.MethodDelete, "/api/github-tokens/release-bot", ""},
	} {
		rec := do(t, srv, call.method, call.path, call.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404: %s", call.method, call.path, rec.Code, rec.Body)
		}
	}
}

// The whole point of the change: a token added from the UI lands exactly
// where an operator's own file would have, and says out loud that the
// daemon has to restart before it can be ticked on a task.
func TestAddingATokenWritesTheFileTheProxyReads(t *testing.T) {
	srv, credentials, dir := gitHubTokenServer(t,
		map[string]string{"*": "bot"}, map[string]string{"bot": "bot-token"})

	rec := do(t, srv, http.MethodPut, "/api/github-tokens/release-bot", `{"value":"  ghp-release\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "ghp-release") {
		t.Fatalf("the response leaked the token: %s", rec.Body)
	}

	data, err := os.ReadFile(filepath.Join(dir, "release-bot.token"))
	if err != nil {
		t.Fatalf("reading the file the UI should have written: %v", err)
	}
	if strings.TrimSpace(string(data)) != "ghp-release" {
		t.Fatalf("release-bot.token holds %q, want the trimmed token", data)
	}
	// And the ladder itself resolves it -- the same call the git proxy
	// makes for a task holding this token's capability.
	if cred, ok := credentials.Get("release-bot"); !ok || cred.Token == nil || *cred.Token != "ghp-release" {
		t.Fatalf("Get(release-bot) = %+v, %v", cred, ok)
	}

	body := tokenBody(t, rec)
	row := tokenRow(t, body, "release-bot")
	if row["capability"] != "github-credential:release-bot" {
		t.Errorf("capability = %v, want the id a task holds", row["capability"])
	}
	if row["present"] != true || row["offered"] != false || row["needsRestart"] != true {
		t.Errorf("row = %v, want present, not yet offered, and needing a restart", row)
	}
	if body["restartRequired"] != true {
		t.Errorf("restartRequired = %v, want true", body["restartRequired"])
	}
}

// A token this process started with is already a capability, so nothing
// about it is pending; the default credential is reported as the ladder's
// own rather than as a capability at all.
func TestListingReportsTheDefaultAndTheOfferedTokens(t *testing.T) {
	srv, _, _ := gitHubTokenServer(t,
		map[string]string{"*": "bot", "acme/*": "bot"},
		map[string]string{"bot": "bot-token", "release-bot": "release-token"},
		"release-bot")

	rec := do(t, srv, http.MethodGet, "/api/github-tokens", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	body := tokenBody(t, rec)
	if body["enabled"] != true || body["restartRequired"] != false {
		t.Fatalf("enabled/restartRequired = %v/%v, want true/false", body["enabled"], body["restartRequired"])
	}

	def := tokenRow(t, body, "bot")
	if def["default"] != true || def["capability"] != nil {
		t.Errorf("bot = %v, want the deployment default with no capability id", def)
	}
	if !reflect.DeepEqual(def["patterns"], []any{"*", "acme/*"}) {
		t.Errorf("bot patterns = %v, want every ladder entry naming it", def["patterns"])
	}
	extra := tokenRow(t, body, "release-bot")
	if extra["offered"] != true || extra["needsRestart"] != false {
		t.Errorf("release-bot = %v, want offered and settled", extra)
	}
}

func TestRemovingATokenDeletesItAndAsksForARestart(t *testing.T) {
	srv, _, dir := gitHubTokenServer(t,
		map[string]string{"*": "bot"},
		map[string]string{"bot": "bot-token", "release-bot": "release-token"},
		"release-bot")

	rec := do(t, srv, http.MethodDelete, "/api/github-tokens/release-bot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "release-bot.token")); !os.IsNotExist(err) {
		t.Errorf("release-bot.token still exists (stat err %v)", err)
	}
	// Still listed, because this process is still offering it: the row
	// is what tells an operator the picker will keep showing a token
	// nothing backs until the daemon restarts.
	row := tokenRow(t, tokenBody(t, rec), "release-bot")
	if row["present"] != false || row["offered"] != true || row["needsRestart"] != true {
		t.Errorf("row = %v, want absent, still offered, needing a restart", row)
	}
}

// Deleting a credential the ladder still points at would take every push
// it covers down, so it is refused rather than done -- the error names
// the entries to repoint or remove first, which is now work this same
// pane does (grain/task-4).
func TestRemovingACredentialTheLadderNamesIsRefused(t *testing.T) {
	srv, _, dir := gitHubTokenServer(t,
		map[string]string{"*": "bot", "acme/*": "acme-bot"},
		map[string]string{"bot": "bot-token", "acme-bot": "acme-token"},
		"acme-bot")

	for _, name := range []string{"bot", "acme-bot"} {
		rec := do(t, srv, http.MethodDelete, "/api/github-tokens/"+name, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("DELETE %s status = %d, want 400: %s", name, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "credential ladder") {
			t.Errorf("DELETE %s said %q, want it to name the ladder entries in the way", name, rec.Body)
		}
		if _, err := os.Stat(filepath.Join(dir, name+".token")); err != nil {
			t.Errorf("%s.token was removed by a refused delete: %v", name, err)
		}
	}
}

func TestAddingATokenRejectsAnUnusableNameOrAnEmptyValue(t *testing.T) {
	srv, _, dir := gitHubTokenServer(t, map[string]string{"*": "bot"}, map[string]string{"bot": "bot-token"})

	for _, call := range []struct{ path, body string }{
		{"/api/github-tokens/release%2Fbot", `{"value":"ghp-fake"}`},
		{"/api/github-tokens/anonymous", `{"value":"ghp-fake"}`},
		{"/api/github-tokens/release-bot", `{"value":"   "}`},
	} {
		rec := do(t, srv, http.MethodPut, call.path, call.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %s status = %d, want 400: %s", call.path, rec.Code, rec.Body)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("directory holds %d files after three refused writes, want the original 2", len(entries))
	}
}
