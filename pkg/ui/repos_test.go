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

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
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

// AddTargetRepo/RemoveTargetRepo are a read-modify-write against whatever
// UpdateSettings already has stored (repos.go's own doc comment), so a
// deployment that has never saved settings at all gets UpdateSettings'
// own "poll interval etc. are required the first time" refusal here too,
// rather than silently seeding a Config no daemon could start against.
func TestAddTargetRepoRefusesOnAnUnconfiguredDeployment(t *testing.T) {
	c, _, ctx := testClient(t)

	_, err := c.AddTargetRepo(ctx, "acme/widgets")
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("AddTargetRepo on an unconfigured deployment: err = %v, want a ValidationError", err)
	}
}

// RemoveTargetRepo never reaches UpdateSettings at all here: an
// unconfigured deployment's TargetRepos is empty, so removing anything
// from it is the same no-op RemoveTargetRepoRemovesAndIsIdempotentWhenAbsent
// already covers for a configured one -- unlike AddTargetRepo above,
// there is no way for a repo to be present to remove before settings
// have ever been saved.
func TestRemoveTargetRepoIsANoOpOnAnUnconfiguredDeployment(t *testing.T) {
	c, _, ctx := testClient(t)

	got, err := c.RemoveTargetRepo(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("RemoveTargetRepo on an unconfigured deployment: %v", err)
	}
	if got.Configured {
		t.Fatalf("settings = %+v, want still Configured false", got)
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
		`{"pollInterval":"30s","maxWorkers":1,"geminiModel":"gemini-2.5-pro","claudeModel":"claude-sonnet-5","githubHost":"github.com"}`)
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
		`{"pollInterval":"30s","maxWorkers":1,"geminiModel":"gemini-2.5-pro","claudeModel":"claude-sonnet-5","githubHost":"github.com"}`)
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
		`{"pollInterval":"30s","maxWorkers":1,"geminiModel":"gemini-2.5-pro","claudeModel":"claude-sonnet-5","githubHost":"github.com"}`)

	rec := do(t, srv, http.MethodPost, "/api/repos", `{"repo":"not-a-repo"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

// grain/task-24: a repo's own default capability set -- the per-repo
// layer of what Settings chooses deployment-wide. Stored as given, and
// reported back alongside the deployment layer it composes with and the
// union a task filed here would actually start with, since the repo's
// own list says nothing useful on its own.
func TestSetRepoDefaultCapabilitiesStoresAndReportsAllThreeSets(t *testing.T) {
	c, store, ctx := testClient(t)
	putDefaultCapabilities(t, ctx, store, "gemini-key")

	got, err := c.SetRepoDefaultCapabilities(ctx, "acme/widgets", []string{"gcp-key"})
	if err != nil {
		t.Fatalf("setting a repo's default capabilities: %v", err)
	}
	if !reflect.DeepEqual(got.DefaultCapabilities, []string{"gcp-key"}) {
		t.Errorf("defaultCapabilities = %v, want [gcp-key]", got.DefaultCapabilities)
	}
	if !reflect.DeepEqual(got.DeploymentDefaultCapabilities, []string{"gemini-key"}) {
		t.Errorf("deploymentDefaultCapabilities = %v, want [gemini-key]", got.DeploymentDefaultCapabilities)
	}
	if !reflect.DeepEqual(got.EffectiveDefaultCapabilities, []string{"gemini-key", "gcp-key"}) {
		t.Errorf("effectiveDefaultCapabilities = %v, want [gemini-key gcp-key]: the deployment's, then this repo's",
			got.EffectiveDefaultCapabilities)
	}

	read, err := c.RepoDefaults(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("reading them back: %v", err)
	}
	if !reflect.DeepEqual(read, got) {
		t.Fatalf("read back = %+v, want what the write returned %+v", read, got)
	}

	// A repo nobody has configured is not an error: it adds nothing,
	// which is exactly what it contributes.
	none, err := c.RepoDefaults(ctx, "acme/gadgets")
	if err != nil {
		t.Fatalf("reading an unconfigured repo: %v", err)
	}
	if len(none.DefaultCapabilities) != 0 {
		t.Errorf("defaultCapabilities = %v, want none", none.DefaultCapabilities)
	}
	if !reflect.DeepEqual(none.EffectiveDefaultCapabilities, []string{"gemini-key"}) {
		t.Errorf("effectiveDefaultCapabilities = %v, want the deployment's [gemini-key]",
			none.EffectiveDefaultCapabilities)
	}
}

// Emptying the set is how a repo stops adding anything -- and it leaves
// no row behind to be listed or wondered about (model.RepoConfig.Empty).
func TestSetRepoDefaultCapabilitiesToNoneClearsTheRepo(t *testing.T) {
	c, store, ctx := testClient(t)
	if _, err := c.SetRepoDefaultCapabilities(ctx, "acme/widgets", []string{"gcp-key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetRepoDefaultCapabilities(ctx, "acme/widgets", nil); err != nil {
		t.Fatalf("clearing a repo's default capabilities: %v", err)
	}
	configs, err := store.ListRepoConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 0 {
		t.Fatalf("repo configs = %+v, want none left", configs)
	}
}

// Rejected on the way in, the same way UpdateSettings rejects an unknown
// id for the deployment-wide set: this is somebody choosing a set, and a
// choice that could never take effect should be refused while whoever
// made it is still looking at it. Duplicates are a picker artifact, not
// a mistake, so they are folded rather than refused.
func TestSetRepoDefaultCapabilitiesValidates(t *testing.T) {
	c, _, ctx := testClient(t)

	if _, err := c.SetRepoDefaultCapabilities(ctx, "acme/widgets", []string{"nope"}); err == nil {
		t.Fatal("setting an unknown capability: want a validation error, got nil")
	} else {
		var ve *ui.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("error = %v, want a *ui.ValidationError", err)
		}
	}
	if _, err := c.SetRepoDefaultCapabilities(ctx, "not-a-repo", []string{"gcp-key"}); err == nil {
		t.Fatal("setting defaults on a malformed repo: want a validation error, got nil")
	}

	got, err := c.SetRepoDefaultCapabilities(ctx, "acme/widgets", []string{"gcp-key", "gcp-key"})
	if err != nil {
		t.Fatalf("setting a duplicated id: %v", err)
	}
	if !reflect.DeepEqual(got.DefaultCapabilities, []string{"gcp-key"}) {
		t.Fatalf("defaultCapabilities = %v, want [gcp-key] once", got.DefaultCapabilities)
	}
}

// grain/task-43: the per-repo counterpart of TestUpdateSettingsAccepts
// DroppingARetiredDefaultCapability. A set that drops a stored id this
// build no longer offers is accepted, though one that adds an unknown id
// is not (TestSetRepoDefaultCapabilitiesValidates) -- what is validated
// is the set being written, not the set already there, which is the only
// reason the repos pane's own picker can offer a retired id a row to be
// unticked with (capabilityRows, ui/src/state.js) and have the save that
// follows go through.
func TestSetRepoDefaultCapabilitiesAcceptsDroppingARetiredOne(t *testing.T) {
	c, store, ctx := testClient(t)
	repo, err := model.ParseRepo("acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	// Written straight to the store: SetRepoDefaultCapabilities itself
	// would never have accepted "scratch-repo", so this is the state an
	// upgrade that retired an id leaves behind.
	if err := store.PutRepoConfig(ctx, model.RepoConfig{
		Repo: repo, DefaultCapabilities: []string{"gcp-key", "scratch-repo"},
	}); err != nil {
		t.Fatal(err)
	}

	read, err := c.RepoDefaults(ctx, "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read.DefaultCapabilities, []string{"gcp-key", "scratch-repo"}) {
		t.Fatalf("defaultCapabilities = %v, want it reported as stored: an operator can only clear one they can see",
			read.DefaultCapabilities)
	}
	if !reflect.DeepEqual(read.EffectiveDefaultCapabilities, []string{"gcp-key"}) {
		t.Fatalf("effectiveDefaultCapabilities = %v, want [gcp-key]: a retired id grants nothing",
			read.EffectiveDefaultCapabilities)
	}

	got, err := c.SetRepoDefaultCapabilities(ctx, "acme/widgets", []string{"gcp-key"})
	if err != nil {
		t.Fatalf("dropping a retired default capability: %v", err)
	}
	if !reflect.DeepEqual(got.DefaultCapabilities, []string{"gcp-key"}) {
		t.Fatalf("defaultCapabilities = %v, want [gcp-key] with the retired id gone", got.DefaultCapabilities)
	}
	if read, err = c.RepoDefaults(ctx, "acme/widgets"); err != nil {
		t.Fatal(err)
	} else if !reflect.DeepEqual(read.DefaultCapabilities, []string{"gcp-key"}) {
		t.Fatalf("defaultCapabilities = %v after a re-read, want [gcp-key]: the retired id is gone for good",
			read.DefaultCapabilities)
	}
}

func TestRepoCapabilitiesGetAndPut(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPut, "/api/repos/acme/widgets/capabilities",
		`{"defaultCapabilities":["gcp-key"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200: %s", rec.Code, rec.Body)
	}
	saved := decode[ui.RepoDefaults](t, rec)
	if saved.Repo != "acme/widgets" || !reflect.DeepEqual(saved.DefaultCapabilities, []string{"gcp-key"}) {
		t.Fatalf("put response = %+v, want acme/widgets with [gcp-key]", saved)
	}

	rec = do(t, srv, http.MethodGet, "/api/repos/acme/widgets/capabilities", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := decode[ui.RepoDefaults](t, rec); !reflect.DeepEqual(got, saved) {
		t.Fatalf("get = %+v, want %+v", got, saved)
	}

	rec = do(t, srv, http.MethodPut, "/api/repos/acme/widgets/capabilities",
		`{"defaultCapabilities":["nope"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown capability status = %d, want 400: %s", rec.Code, rec.Body)
	}
}
