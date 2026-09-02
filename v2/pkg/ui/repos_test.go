package ui_test

// AddTargetRepo/RemoveTargetRepo's own tests, plus /api/repos' HTTP
// surface -- the repos pane's add/remove (bwsalmon/agents#473), which
// settings_test.go's own UpdateSettings tests don't cover: this exercises
// them as the single-repo mutations the repos pane actually calls, not
// as a whole-array PUT.

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func TestAddTargetRepoAppendsAndIsIdempotent(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	got, err := c.AddTargetRepo(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("adding a repo: %v", err)
	}
	want := []string{"acme/widgets"}
	if !reflect.DeepEqual(got.TargetRepos, want) {
		t.Fatalf("targetRepos = %v, want %v", got.TargetRepos, want)
	}

	// Adding the same repo again is a no-op, not a duplicate entry.
	got, err = c.AddTargetRepo(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("adding the same repo again: %v", err)
	}
	if !reflect.DeepEqual(got.TargetRepos, want) {
		t.Fatalf("targetRepos after re-adding = %v, want %v", got.TargetRepos, want)
	}

	got, err = c.AddTargetRepo(ctx, "acme/gadgets")
	if err != nil {
		t.Fatalf("adding a second repo: %v", err)
	}
	want = []string{"acme/widgets", "acme/gadgets"}
	if !reflect.DeepEqual(got.TargetRepos, want) {
		t.Fatalf("targetRepos = %v, want %v", got.TargetRepos, want)
	}
}

func TestAddTargetRepoRejectsAMalformedRepo(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	_, err := c.AddTargetRepo(ctx, "not-a-repo")
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a ValidationError", err)
	}
}

func TestRemoveTargetRepoRemovesAndIsIdempotentWhenAbsent(t *testing.T) {
	c, _, ctx := testClient(t)
	repos := []string{"acme/widgets", "acme/gadgets"}
	req := firstSettings()
	req.TargetRepos = &repos
	if _, err := c.UpdateSettings(ctx, req); err != nil {
		t.Fatal(err)
	}

	got, err := c.RemoveTargetRepo(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("removing a repo: %v", err)
	}
	want := []string{"acme/gadgets"}
	if !reflect.DeepEqual(got.TargetRepos, want) {
		t.Fatalf("targetRepos = %v, want %v", got.TargetRepos, want)
	}

	// Removing a repo that isn't there (including on a deployment that
	// never had any target repos at all) is a no-op, not an error.
	got, err = c.RemoveTargetRepo(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("removing an absent repo: %v", err)
	}
	if !reflect.DeepEqual(got.TargetRepos, want) {
		t.Fatalf("targetRepos after removing an absent repo = %v, want %v", got.TargetRepos, want)
	}
}

// TestAddTargetRepoIsVisibleThroughConfigInTheSameProcess guards against
// the gap Client.setTargetRepos closes: Config.TargetRepos used to only
// ever be set once, at NewClient, so a change UpdateSettings wrote to the
// store stayed invisible to anything reading Client.Config directly (GET
// /api/config, CreateTask's own allowlist check) until a restart. Adding
// a repo through the repos pane has to be visible immediately, or the
// pane would look broken the moment anyone actually used it.
func TestAddTargetRepoIsVisibleThroughConfigInTheSameProcess(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPut, "/api/settings",
		`{"pollInterval":"30s","maxConcurrent":1,"geminiModel":"gemini-2.5-pro","claudeModel":"claude-sonnet-5","githubHost":"github.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first-time settings save status = %d, want 200: %s", rec.Code, rec.Body)
	}

	rec = do(t, srv, http.MethodPost, "/api/repos", `{"repo":"acme/widgets"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add repo status = %d, want 200: %s", rec.Code, rec.Body)
	}

	rec = do(t, srv, http.MethodGet, "/api/config", "")
	cfg := decode[map[string]any](t, rec)
	got, _ := cfg["targetRepos"].([]any)
	if len(got) != 1 || got[0] != "acme/widgets" {
		t.Fatalf("GET /api/config targetRepos = %v, want [acme/widgets]", cfg["targetRepos"])
	}
}

func TestReposCreateThenDelete(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPut, "/api/settings",
		`{"pollInterval":"30s","maxConcurrent":1,"geminiModel":"gemini-2.5-pro","claudeModel":"claude-sonnet-5","githubHost":"github.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first-time settings save status = %d, want 200: %s", rec.Code, rec.Body)
	}

	rec = do(t, srv, http.MethodPost, "/api/repos", `{"repo":"acme/widgets"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add status = %d, want 200: %s", rec.Code, rec.Body)
	}
	added := decode[ui.Settings](t, rec)
	if !reflect.DeepEqual(added.TargetRepos, []string{"acme/widgets"}) {
		t.Fatalf("targetRepos = %v, want [acme/widgets]", added.TargetRepos)
	}

	rec = do(t, srv, http.MethodDelete, "/api/repos/acme/widgets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200: %s", rec.Code, rec.Body)
	}
	removed := decode[ui.Settings](t, rec)
	if len(removed.TargetRepos) != 0 {
		t.Fatalf("targetRepos after delete = %v, want none", removed.TargetRepos)
	}
}

func TestAddRepoRejectsAMalformedRepoWith400(t *testing.T) {
	srv, _ := testServer(t)
	do(t, srv, http.MethodPut, "/api/settings",
		`{"pollInterval":"30s","maxConcurrent":1,"geminiModel":"gemini-2.5-pro","claudeModel":"claude-sonnet-5","githubHost":"github.com"}`)

	rec := do(t, srv, http.MethodPost, "/api/repos", `{"repo":"not-a-repo"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}
