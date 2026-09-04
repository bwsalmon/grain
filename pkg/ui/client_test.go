package ui_test

// pkg/ui against a real embedded SQLite store, the same discipline every
// other package's own tests hold to (model/simulate_test.go: "Nothing
// here is a fake standing in for the store"). These used to run against
// an in-memory github.Client stand-in, because a task was a GitHub issue;
// there is no GitHub in this package at all now.

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/ui"
)

var baseTime = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func testClient(t *testing.T) (*ui.Client, *model.Store, context.Context) {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}

	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	client := ui.NewClient(ui.Config{
		Actor:         ui.DefaultActor("alice"),
		DefaultTarget: &repo,
		Capabilities:  ui.OfferedCapabilities(),
	}, store)
	client.Now = func() time.Time { return baseTime }
	return client, store, ctx
}

// create is the common setup: an approved task, so it reads queued. It
// states no opinion about where in the backlog it goes, so it joins
// whichever end the deployment currently remembers -- createAt below is
// the same task with that choice made.
func create(t *testing.T, c *ui.Client, ctx context.Context) ui.Task {
	t.Helper()
	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "fix the thing", Description: "please", Approved: true,
	})
	if err != nil {
		t.Fatalf("creating a task: %v", err)
	}
	return task
}

// createAt is create with grain/task-202's own choice stated: atFront
// true files the task ahead of everything already queued, false at the
// end behind it, and either way the deployment remembers it for the next
// task filed without one (ui.CreateTaskRequest.AtFront).
func createAt(t *testing.T, c *ui.Client, ctx context.Context, atFront bool) ui.Task {
	t.Helper()
	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "fix the thing", Description: "please", Approved: true, AtFront: &atFront,
	})
	if err != nil {
		t.Fatalf("creating a task at the %s of the backlog: %v", map[bool]string{true: "front", false: "end"}[atFront], err)
	}
	return task
}

// The point of the whole inversion: a task exists after one call, with no
// GitHub issue anywhere behind it.
func TestCreateTaskFilesStraightIntoTheStore(t *testing.T) {
	c, store, ctx := testClient(t)

	task := create(t, c, ctx)
	if task.ID == "" {
		t.Fatal("created task has no id")
	}
	if task.State != model.StateQueued {
		t.Fatalf("state = %q, want queued: an approved task is dispatchable at once", task.State)
	}
	if task.Repo != "acme/widgets" {
		t.Fatalf("repo = %q, want the configured default target", task.Repo)
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil {
		t.Fatal("task is not in the store")
	}
	if stored.Origin.Attribution.Actor.ID != "alice" {
		t.Fatalf("origin actor = %q, want the configured actor", stored.Origin.Attribution.Actor.ID)
	}

	// Dispatchable means dispatchable: task_ready is the view
	// dispatch.Cycle drains, and nothing polled anything to get here.
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0] != task.ID {
		t.Fatalf("ready = %v, want just the new task", ready)
	}
}

func TestCreateTaskUnapprovedFilesAsAProposal(t *testing.T) {
	c, store, ctx := testClient(t)

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "maybe do this"})
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.StateProposed {
		t.Fatalf("state = %q, want proposed", task.State)
	}
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("ready = %v, want nothing: a proposal is not dispatchable", ready)
	}
}

// TestCreateTaskInteractiveDispatchesAtOnce is bwsalmon/agents#539's
// whole point: an interactive task is dispatchable the moment it is
// filed even though Approved was never set, because CreateTaskRequest.
// Interactive's own doc comment says a chat nobody has opened yet has
// nothing to show for itself.
func TestCreateTaskInteractiveDispatchesAtOnce(t *testing.T) {
	c, store, ctx := testClient(t)

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "chat about this", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if !task.Interactive {
		t.Fatal("task.Interactive = false, want true")
	}
	if task.State != model.StateQueued {
		t.Fatalf("state = %q, want queued: an interactive task is approved at once", task.State)
	}
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0] != task.ID {
		t.Fatalf("ready = %v, want just the new task", ready)
	}
}

// TestCreateTaskInteractiveJumpsTheQueue mirrors
// TestNewestFirstSettingMovesNewTasksToTheFrontOfTheQueue: an interactive
// task dispatches ahead of whatever is already queued, on top of
// whatever model.Config.NewestFirst says, since starting it is the whole
// reason it was filed.
func TestCreateTaskInteractiveJumpsTheQueue(t *testing.T) {
	c, store, ctx := testClient(t)

	first := create(t, c, ctx)
	second := create(t, c, ctx)
	chat, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "chat about this", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}

	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{chat.ID, first.ID, second.ID}
	if !reflect.DeepEqual(ready, want) {
		t.Fatalf("Ready = %v, want the interactive task dispatched first %v", ready, want)
	}
}

// TestCreateTaskConfigurationAssemblesTheWholeBundle is bwsalmon/
// agents#621 (widened by bwsalmon/agents#620's bootstrap-playbooks
// grant): a caller asking for nothing but Configuration gets back a
// task that is interactive, carries the self-debug, self-repair and
// bootstrap-playbooks grants, and has a non-empty title and body --
// CreateTask assembles the whole thing itself rather than trusting a
// caller to ask for each piece by hand.
func TestCreateTaskConfigurationAssemblesTheWholeBundle(t *testing.T) {
	c, _, ctx := testClient(t)

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Configuration: true})
	if err != nil {
		t.Fatal(err)
	}
	if !task.Interactive {
		t.Error("task.Interactive = false, want true")
	}
	if !task.Configuration {
		t.Error("task.Configuration = false, want true")
	}
	if task.Title == "" {
		t.Error("task.Title is empty, want a default")
	}
	if task.Description == "" {
		t.Error("task.Description is empty, want the default prompt")
	}
	want := []string{"bootstrap-playbooks", "self-debug", "self-repair"}
	sort.Strings(task.Capabilities)
	if !reflect.DeepEqual(task.Capabilities, want) {
		t.Fatalf("capabilities = %v, want %v", task.Capabilities, want)
	}
}

// TestCreateTaskConfigurationKeepsACallerSuppliedTitleAndCapabilities
// checks the other half of the bundle: Configuration only fills in what
// the request left blank, rather than overwriting a title, description
// or capability list the caller actually supplied.
func TestCreateTaskConfigurationKeepsACallerSuppliedTitleAndCapabilities(t *testing.T) {
	c, _, ctx := testClient(t)

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Configuration: true,
		Title:         "why is the daemon restarting",
		Description:   "it keeps crash-looping, please debug",
		Capabilities:  &[]string{"gemini-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "why is the daemon restarting" {
		t.Errorf("task.Title = %q, want the caller's own", task.Title)
	}
	if task.Description != "it keeps crash-looping, please debug" {
		t.Errorf("task.Description = %q, want the caller's own", task.Description)
	}
	want := []string{"bootstrap-playbooks", "gemini-key", "self-debug", "self-repair"}
	sort.Strings(task.Capabilities)
	if !reflect.DeepEqual(task.Capabilities, want) {
		t.Fatalf("capabilities = %v, want the caller's own plus the configuration agent's", task.Capabilities)
	}
}

// A repo outside Config.TargetRepos is filed exactly as asked, but
// parked awaiting reply -- v1's "a task naming anything else is parked
// with a comment rather than dispatched" -- so it never reaches
// task_ready.
func TestCreateTaskOffTargetRepoListParksAwaitingReply(t *testing.T) {
	c, store, ctx := testClient(t)
	c.Config.TargetRepos = []string{"acme/widgets", "acme/gadgets"}

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "fix the other thing", Repo: "someone-else/unrelated", Approved: true,
	})
	if err != nil {
		t.Fatalf("creating a task off the allowlist: %v", err)
	}
	if task.Repo != "someone-else/unrelated" {
		t.Fatalf("repo = %q, want the requested repo -- parking must not rewrite Target", task.Repo)
	}
	if task.State != model.StateAwaitingReply {
		t.Fatalf("state = %q, want awaiting_reply", task.State)
	}

	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("ready = %v, want nothing: a parked task is not dispatchable", ready)
	}

	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Comments) != 1 {
		t.Fatalf("comments = %v, want exactly one explaining the park", detail.Comments)
	}
	if got := detail.Comments[0].Body; got == "" {
		t.Fatal("park comment has no body")
	}

	// Replying is how an operator un-parks it, the same as any other
	// awaiting_reply task -- AddComment's own doc comment.
	if err := c.AddComment(ctx, task.ID, "widened targetRepos, this can run now", nil); err != nil {
		t.Fatal(err)
	}
	requeued, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.State != model.StateQueued {
		t.Fatalf("state after reply = %q, want queued", requeued.State)
	}
}

// An empty Config.TargetRepos is v1's own "leave empty for a
// single-repo deployment": nothing is restricted.
func TestCreateTaskEmptyTargetRepoListAllowsAnyRepo(t *testing.T) {
	c, store, ctx := testClient(t)

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "fix it", Repo: "someone-else/unrelated", Approved: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.StateQueued {
		t.Fatalf("state = %q, want queued: an empty target repo list restricts nothing", task.State)
	}
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 {
		t.Fatalf("ready = %v, want just the new task", ready)
	}
}

func TestCreateTaskCarriesCapabilityGrants(t *testing.T) {
	c, store, ctx := testClient(t)

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "needs a key", Approved: true, Capabilities: &[]string{"gemini-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Capabilities) != 1 || task.Capabilities[0] != "gemini-key" {
		t.Fatalf("capabilities = %v, want [gemini-key]", task.Capabilities)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Grants) != 1 || stored.Grants[0].Capability != "gemini-key" {
		t.Fatalf("grants = %+v, want one gemini-key grant", stored.Grants)
	}
}

// putDefaultCapabilities stores ids as this deployment's default
// capability set, the way an operator saving Settings would -- through a
// full model.Config, since PutConfig writes the row wholesale.
func putDefaultCapabilities(t *testing.T, ctx context.Context, store *model.Store, ids ...string) {
	t.Helper()
	cfg := model.DefaultConfig()
	cfg.DefaultCapabilities = ids
	if err := store.PutConfig(ctx, cfg); err != nil {
		t.Fatalf("storing default capabilities %v: %v", ids, err)
	}
}

// grain/task-14: a deployment can say which capabilities every task
// filed on it starts out holding. The grant lands on the task itself, as
// GrantByDefault -- not as a deployment-level set read again at dispatch
// -- which is what makes it visible on the task and detachable from it.
func TestCreateTaskSeedsDeploymentDefaultCapabilities(t *testing.T) {
	c, store, ctx := testClient(t)
	putDefaultCapabilities(t, ctx, store, "gcp-key")

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "needs a sandbox key", Approved: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(task.Capabilities, "gcp-key") {
		t.Fatalf("capabilities = %v, want gcp-key among them: the deployment defaults it", task.Capabilities)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Grants) != 1 || stored.Grants[0].Via != model.GrantByDefault {
		t.Fatalf("grants = %+v, want one gcp-key grant recorded as %q", stored.Grants, model.GrantByDefault)
	}
	// Detachable like any other grant: a default is a starting point, not
	// something the task is stuck with.
	if err := c.SetCapability(ctx, task.ID, "gcp-key", false); err != nil {
		t.Fatalf("detaching a defaulted capability: %v", err)
	}
	detached, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detached.Grants) != 0 {
		t.Fatalf("grants after detaching = %+v, want none", detached.Grants)
	}
}

// A request that names its own capabilities names all of them: the
// defaults seed a task nobody said anything about, and an empty-but-
// present list is somebody saying "none". The UI's own form always sends
// a list -- seeded from the defaults -- so unticking a box on it has to
// mean the task is filed without that capability.
func TestCreateTaskCapabilitiesOverrideDeploymentDefaults(t *testing.T) {
	c, store, ctx := testClient(t)
	putDefaultCapabilities(t, ctx, store, "gcp-key")

	named, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "only this one", Approved: true, Capabilities: &[]string{"gemini-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(named.Capabilities, []string{"gemini-key"}) {
		t.Errorf("capabilities = %v, want [gemini-key] alone", named.Capabilities)
	}
	stored, err := store.GetTask(ctx, named.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Grants) != 1 || stored.Grants[0].Via != model.GrantByLabel {
		t.Errorf("grants = %+v, want one gemini-key grant recorded as %q", stored.Grants, model.GrantByLabel)
	}

	none, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "none at all", Approved: true, Capabilities: &[]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Capabilities) != 0 {
		t.Errorf("capabilities = %v, want none: an empty list is a choice, not an absent one", none.Capabilities)
	}
}

// A configuration-agent task gets its own three grants on top of
// whatever the deployment defaults, rather than either one replacing the
// other.
func TestCreateConfigurationTaskKeepsDeploymentDefaults(t *testing.T) {
	c, store, ctx := testClient(t)
	putDefaultCapabilities(t, ctx, store, "gcp-key")

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Configuration: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gcp-key", "self-debug", "self-repair", "bootstrap-playbooks"} {
		if !slices.Contains(task.Capabilities, want) {
			t.Errorf("capabilities = %v, want %s among them", task.Capabilities, want)
		}
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Grants) != 4 {
		t.Fatalf("grants = %+v, want four", stored.Grants)
	}
}

// A stored default this build no longer offers is skipped, not fatal:
// UpdateSettings validates the set when it is saved, so an id with no
// row can only be one grain has retired since, and a settings row left
// behind by an upgrade must not become a deployment where no task can be
// filed at all ("scratch-repo" is exactly that -- task-10 renamed it to
// github-sandbox).
func TestCreateTaskSkipsRetiredDefaultCapability(t *testing.T) {
	c, store, ctx := testClient(t)
	putDefaultCapabilities(t, ctx, store, "scratch-repo", "gemini-key")

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "still filable", Approved: true})
	if err != nil {
		t.Fatalf("filing a task on a deployment defaulting a retired capability: %v", err)
	}
	if !reflect.DeepEqual(task.Capabilities, []string{"gemini-key"}) {
		t.Fatalf("capabilities = %v, want [gemini-key]: the retired id is skipped, the rest still granted",
			task.Capabilities)
	}
}

// putRepoDefaultCapabilities stores ids as one repo's own default
// capability set, the way an operator saving the repos pane's own
// per-repo picker would.
func putRepoDefaultCapabilities(t *testing.T, ctx context.Context, store *model.Store, repo string, ids ...string) {
	t.Helper()
	parsed, err := model.ParseRepo(repo)
	if err != nil {
		t.Fatalf("parsing %q: %v", repo, err)
	}
	if err := store.PutRepoConfig(ctx, model.RepoConfig{Repo: parsed, DefaultCapabilities: ids}); err != nil {
		t.Fatalf("storing repo default capabilities %v for %s: %v", ids, repo, err)
	}
}

// grain/task-24: a repo can default capabilities of its own, on top of
// whatever the deployment defaults. Union, deployment-wide first -- a
// repo adds and never subtracts -- and only for the repo the task
// actually targets.
func TestCreateTaskUnionsRepoDefaultCapabilities(t *testing.T) {
	c, store, ctx := testClient(t)
	putDefaultCapabilities(t, ctx, store, "gemini-key")
	putRepoDefaultCapabilities(t, ctx, store, "acme/widgets", "gcp-key")

	here, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "on the repo that adds one", Approved: true, Repo: "acme/widgets",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Compared sorted: a task's capabilities come back in the order
	// Store.grantsOf reads them (by capability), not in the order they
	// were seeded.
	if !reflect.DeepEqual(here.Capabilities, []string{"gcp-key", "gemini-key"}) {
		t.Errorf("capabilities = %v, want both the deployment's gemini-key and the repo's gcp-key",
			here.Capabilities)
	}
	stored, err := store.GetTask(ctx, here.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range stored.Grants {
		if g.Via != model.GrantByDefault {
			t.Errorf("grant %+v: want every seeded grant recorded as %q", g, model.GrantByDefault)
		}
	}

	// A different repo gets the deployment's set alone -- the repo layer
	// is keyed on the target, not applied to everything once stored.
	elsewhere, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "on another repo", Approved: true, Repo: "acme/gadgets",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(elsewhere.Capabilities, []string{"gemini-key"}) {
		t.Errorf("capabilities = %v, want [gemini-key] alone on a repo with no defaults of its own",
			elsewhere.Capabilities)
	}
}

// A task filed with no repo named at all is filed against
// Config.DefaultTarget, and it is that repo's defaults it should get:
// the layer is keyed on the repo the task ends up targeting, not on
// whether the request happened to spell it out.
func TestCreateTaskUsesDefaultTargetRepoDefaultCapabilities(t *testing.T) {
	c, store, ctx := testClient(t)
	putRepoDefaultCapabilities(t, ctx, store, "acme/widgets", "gcp-key")

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "no repo named", Approved: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(task.Capabilities, []string{"gcp-key"}) {
		t.Fatalf("capabilities = %v, want [gcp-key]: the default target repo's own defaults", task.Capabilities)
	}
}

// A repo-less task (NoRepo) has no repo to key the second layer on, so
// it gets the deployment's set and nothing else -- not the default
// target's, which it deliberately is not filed against.
func TestCreateTaskWithNoRepoTakesOnlyDeploymentDefaultCapabilities(t *testing.T) {
	c, store, ctx := testClient(t)
	putDefaultCapabilities(t, ctx, store, "gemini-key")
	putRepoDefaultCapabilities(t, ctx, store, "acme/widgets", "gcp-key")

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "nothing to check out", Approved: true, NoRepo: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(task.Capabilities, []string{"gemini-key"}) {
		t.Fatalf("capabilities = %v, want [gemini-key] alone", task.Capabilities)
	}
}

// The same skip-rather-than-fail rule the deployment-wide set has, on
// the repo layer: a repo row naming a capability this build retired must
// not become a repo no task can be filed against.
func TestCreateTaskSkipsRetiredRepoDefaultCapability(t *testing.T) {
	c, store, ctx := testClient(t)
	putRepoDefaultCapabilities(t, ctx, store, "acme/widgets", "scratch-repo", "gemini-key")

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "still filable", Approved: true})
	if err != nil {
		t.Fatalf("filing a task against a repo defaulting a retired capability: %v", err)
	}
	if !reflect.DeepEqual(task.Capabilities, []string{"gemini-key"}) {
		t.Fatalf("capabilities = %v, want [gemini-key]", task.Capabilities)
	}
}

// A repo restating something the deployment already defaults grants it
// once, not twice -- the union is a set, and a duplicate grant would be
// two rows for one capability on the task.
func TestCreateTaskDeduplicatesOverlappingDefaultCapabilities(t *testing.T) {
	c, store, ctx := testClient(t)
	putDefaultCapabilities(t, ctx, store, "gcp-key")
	putRepoDefaultCapabilities(t, ctx, store, "acme/widgets", "gcp-key")

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "one key only", Approved: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(task.Capabilities, []string{"gcp-key"}) {
		t.Fatalf("capabilities = %v, want [gcp-key] once", task.Capabilities)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Grants) != 1 {
		t.Fatalf("grants = %+v, want exactly one", stored.Grants)
	}
}

// A request that names its own set still names all of it: the per-repo
// layer seeds a task nobody said anything about, exactly like the
// deployment-wide one, and unticking a box on the new-task form has to
// mean the task is filed without that capability.
func TestCreateTaskCapabilitiesOverrideRepoDefaults(t *testing.T) {
	c, store, ctx := testClient(t)
	putRepoDefaultCapabilities(t, ctx, store, "acme/widgets", "gcp-key")

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "not that one", Approved: true, Capabilities: &[]string{"gemini-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(task.Capabilities, []string{"gemini-key"}) {
		t.Fatalf("capabilities = %v, want [gemini-key] alone", task.Capabilities)
	}
}

// A dependency declared at creation is both the definition (DependsOn)
// and, while the blocker is still open, the signal (Blocked/BlockedBy) --
// docs/data-model.md's "blocked is not a state, it is derived from
// links", re-derived here rather than pinned at creation.
func TestCreateTaskCarriesDependsOnAndBlockedSignal(t *testing.T) {
	c, _, ctx := testClient(t)

	blocker := create(t, c, ctx)
	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "needs the other thing first", Approved: true, DependsOn: []string{blocker.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(task.DependsOn) != 1 || task.DependsOn[0] != blocker.ID {
		t.Fatalf("dependsOn = %v, want [%s]", task.DependsOn, blocker.ID)
	}
	if !task.Blocked {
		t.Fatal("blocked = false, want true: its dependency has not closed")
	}
	if len(task.BlockedBy) != 1 || task.BlockedBy[0] != blocker.ID {
		t.Fatalf("blockedBy = %v, want [%s]", task.BlockedBy, blocker.ID)
	}

	// Approved and blocked is still not ready: task_ready must agree with
	// the JSON signal, not just IsBlocked's own unit tests.
	ready, err := c.Store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ready {
		if id == task.ID {
			t.Fatalf("ready = %v, want the blocked task excluded", ready)
		}
	}

	if err := c.Close(ctx, blocker.ID, ui.CloseOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Blocked {
		t.Fatalf("blocked = true after its dependency closed, want false")
	}
	if len(got.DependsOn) != 1 || got.DependsOn[0] != blocker.ID {
		t.Fatalf("dependsOn after the blocker closed = %v, want it kept as the definition", got.DependsOn)
	}
}

// A read-only repo is stored and rendered as owner/name, and -- the
// design doc's "the single most important rule in this subsection" --
// naming one grants nothing: only Capabilities produce a Grant.
func TestCreateTaskCarriesReadOnlyRepos(t *testing.T) {
	c, store, ctx := testClient(t)

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "needs a shared lib", Approved: true,
		Reads: []string{"acme/shared-lib", "acme/schema"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The store does not promise to preserve the order of a set with no
	// ordering column of its own (task_read's primary key is (task_id,
	// owner, name)), so this compares membership, not order.
	got := append([]string(nil), task.Reads...)
	sort.Strings(got)
	want := []string{"acme/schema", "acme/shared-lib"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reads = %v, want %v", task.Reads, want)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Grants) != 0 {
		t.Fatalf("grants = %+v, want none: a read-only repo must grant nothing", stored.Grants)
	}
}

func TestCreateTaskValidates(t *testing.T) {
	c, _, ctx := testClient(t)

	for name, req := range map[string]ui.CreateTaskRequest{
		"empty title":              {Title: "  "},
		"unknown capability":       {Title: "t", Capabilities: &[]string{"nope"}},
		"unparseable repo":         {Title: "t", Repo: "not-a-repo"},
		"unknown dependency":       {Title: "t", DependsOn: []string{"404"}},
		"unparseable read":         {Title: "t", Reads: []string{"not-a-repo"}},
		"negative sandbox cpus":    {Title: "t", SandboxCPUs: -1},
		"sandbox memory below 128": {Title: "t", SandboxMemoryMB: 64},
		"negative sandbox disk":    {Title: "t", SandboxDiskGB: -1},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.CreateTask(ctx, req)
			var ve *ui.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %v, want a ValidationError", err)
			}
		})
	}
}

func TestCreateTaskNeedsATargetWhenThereIsNoDefault(t *testing.T) {
	c, _, ctx := testClient(t)
	c.Config.DefaultTarget = nil

	_, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "nowhere to go"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

// NoRepo (bwsalmon/agents#614) is the deliberate choice, distinct from a
// blank Repo: even with a configured DefaultTarget to fall back to, an
// explicit NoRepo files the task standalone rather than pinning it to
// the default.
func TestCreateTaskNoRepoFilesAStandaloneTask(t *testing.T) {
	c, store, ctx := testClient(t)

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "standalone", NoRepo: true})
	if err != nil {
		t.Fatalf("creating a no-repo task: %v", err)
	}
	if task.Repo != "" {
		t.Fatalf("repo = %q, want none", task.Repo)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil || stored == nil {
		t.Fatalf("reading the stored task: %v", err)
	}
	if stored.Target != nil {
		t.Fatalf("stored target = %+v, want nil", stored.Target)
	}
}

func TestCreateTaskRejectsRepoAndNoRepoTogether(t *testing.T) {
	c, _, ctx := testClient(t)

	_, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "confused", Repo: "acme/other", NoRepo: true})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestUpdateTaskChangesOnlyTheFieldsGiven(t *testing.T) {
	c, _, ctx := testClient(t)
	task := create(t, c, ctx)

	title := "renamed"
	got, err := c.UpdateTask(ctx, task.ID, ui.UpdateTaskRequest{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "renamed" {
		t.Fatalf("title = %q, want renamed", got.Title)
	}
	if got.Description != "please" {
		t.Fatalf("description = %q, want it untouched", got.Description)
	}
	if got.Repo != "acme/widgets" {
		t.Fatalf("repo = %q, want it untouched", got.Repo)
	}
}

func TestUpdateTaskEditsEveryField(t *testing.T) {
	c, _, ctx := testClient(t)
	task := create(t, c, ctx)

	repo, base, autoMerge := "other/repo", "release", true
	reads := []string{"acme/shared-lib"}
	got, err := c.UpdateTask(ctx, task.ID, ui.UpdateTaskRequest{
		Repo: &repo, Base: &base, AutoMerge: &autoMerge, Reads: &reads,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != "other/repo" || got.Base != "release" || !got.AutoMerge {
		t.Fatalf("task = %+v, want the edited fields", got)
	}
	if !reflect.DeepEqual(got.Reads, reads) {
		t.Fatalf("reads = %v, want %v", got.Reads, reads)
	}
}

// bwsalmon/agents#534, grain/task-41: a task's own SandboxCPUs/
// SandboxMemoryMB/SandboxDiskGB override
// round-trips through CreateTask and UpdateTask the same as every other
// task field, and setting any of them back to 0 through UpdateTask clears
// that override (distinct from leaving the request field nil, which
// leaves it alone -- UpdateTaskRequest's own doc comment).
func TestTaskSandboxShapeOverrideRoundTrips(t *testing.T) {
	c, _, ctx := testClient(t)

	created, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "t", Repo: "acme/widgets", Approved: true,
		SandboxCPUs: 4, SandboxMemoryMB: 8192, SandboxDiskGB: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.SandboxCPUs != 4 || created.SandboxMemoryMB != 8192 || created.SandboxDiskGB != 40 {
		t.Fatalf("created task sandbox shape = %d/%d/%d, want 4/8192/40",
			created.SandboxCPUs, created.SandboxMemoryMB, created.SandboxDiskGB)
	}

	cpus, memoryMB, diskGB := 2, 4096, 20
	updated, err := c.UpdateTask(ctx, created.ID, ui.UpdateTaskRequest{
		SandboxCPUs: &cpus, SandboxMemoryMB: &memoryMB, SandboxDiskGB: &diskGB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.SandboxCPUs != 2 || updated.SandboxMemoryMB != 4096 || updated.SandboxDiskGB != 20 {
		t.Fatalf("updated task sandbox shape = %d/%d/%d, want 2/4096/20",
			updated.SandboxCPUs, updated.SandboxMemoryMB, updated.SandboxDiskGB)
	}

	zero := 0
	cleared, err := c.UpdateTask(ctx, created.ID, ui.UpdateTaskRequest{
		SandboxCPUs: &zero, SandboxMemoryMB: &zero, SandboxDiskGB: &zero,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.SandboxCPUs != 0 || cleared.SandboxMemoryMB != 0 || cleared.SandboxDiskGB != 0 {
		t.Fatalf("cleared task sandbox shape = %d/%d/%d, want 0/0/0",
			cleared.SandboxCPUs, cleared.SandboxMemoryMB, cleared.SandboxDiskGB)
	}
}

// Reads has no attach/detach endpoint of its own (unlike Capabilities and
// DependsOn): a given Reads always replaces the whole set rather than
// adding to it.
func TestUpdateTaskReadsReplacesRatherThanAdds(t *testing.T) {
	c, _, ctx := testClient(t)
	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "t", Approved: true, Reads: []string{"acme/shared-lib"},
	})
	if err != nil {
		t.Fatal(err)
	}

	replacement := []string{"acme/schema"}
	got, err := c.UpdateTask(ctx, task.ID, ui.UpdateTaskRequest{Reads: &replacement})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Reads, replacement) {
		t.Fatalf("reads = %v, want %v (replaced, not appended)", got.Reads, replacement)
	}

	cleared := []string{}
	got, err = c.UpdateTask(ctx, task.ID, ui.UpdateTaskRequest{Reads: &cleared})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reads) != 0 {
		t.Fatalf("reads = %v, want none: an explicit empty slice clears the set", got.Reads)
	}
}

func TestUpdateTaskValidates(t *testing.T) {
	c, _, ctx := testClient(t)
	task := create(t, c, ctx)

	blank, bad := "  ", "not-a-repo"
	negativeCPUs, lowMemory, negativeDisk := -1, 64, -1
	for name, req := range map[string]ui.UpdateTaskRequest{
		"empty title": {Title: &blank},
		// Clearing the target is rejected rather than allowed: a task with
		// no target cannot be dispatched, and it is a real column now
		// rather than an optional directive line that could just be absent.
		"empty repo":               {Repo: &blank},
		"unparseable repo":         {Repo: &bad},
		"unparseable read":         {Reads: &[]string{"not-a-repo"}},
		"negative sandbox cpus":    {SandboxCPUs: &negativeCPUs},
		"sandbox memory below 128": {SandboxMemoryMB: &lowMemory},
		"negative sandbox disk":    {SandboxDiskGB: &negativeDisk},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.UpdateTask(ctx, task.ID, req)
			var ve *ui.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %v, want a ValidationError", err)
			}
		})
	}
}

// TestUpdateTaskNotesAnEditToTitleOrDescriptionAsAComment is bwsalmon/
// agents#523: a task's title and description are the two fields
// BuildPrompt actually hands a dispatched run, so an edit to either while
// a run is in flight has to reach it somehow -- and the mechanism that
// already exists for reaching a live run (orchestrator.addendaPoller) is
// "read the conversation," not "read the task's current row." Recording
// the edit as a Comment is what makes it visible there.
func TestUpdateTaskNotesAnEditToTitleOrDescriptionAsAComment(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	title := "rename the thing"
	if _, err := c.UpdateTask(ctx, task.ID, ui.UpdateTaskRequest{Title: &title}); err != nil {
		t.Fatal(err)
	}
	comments, err := store.Comments(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("comments = %+v, want exactly one noting the title edit", comments)
	}
	if got := comments[0]; got.Author.Actor.ID != "alice" || !strings.Contains(got.Body, title) {
		t.Fatalf("comment = %+v, want it attributed to alice and naming the new title", got)
	}

	description := "please, and hurry"
	if _, err := c.UpdateTask(ctx, task.ID, ui.UpdateTaskRequest{Description: &description}); err != nil {
		t.Fatal(err)
	}
	comments, err = store.Comments(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments = %+v, want a second one noting the description edit", comments)
	}
	if !strings.Contains(comments[1].Body, description) {
		t.Fatalf("comment = %+v, want it to include the new description", comments[1])
	}
}

// TestUpdateTaskNotesNothingWhenTitleAndDescriptionAreUnchanged checks the
// other half of the same rule: a request that only touches other fields,
// or that names a title/description identical to what is already stored,
// adds no comment -- an edit form that always submits every field on save
// must not spam a task's conversation on every save that changed nothing.
func TestUpdateTaskNotesNothingWhenTitleAndDescriptionAreUnchanged(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	base := "release"
	if _, err := c.UpdateTask(ctx, task.ID, ui.UpdateTaskRequest{Base: &base}); err != nil {
		t.Fatal(err)
	}
	sameTitle, sameDescription := task.Title, task.Description
	if _, err := c.UpdateTask(ctx, task.ID, ui.UpdateTaskRequest{Title: &sameTitle, Description: &sameDescription}); err != nil {
		t.Fatal(err)
	}
	comments, err := store.Comments(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 0 {
		t.Fatalf("comments = %+v, want none: nothing BuildPrompt reads ever changed", comments)
	}
}

func TestUpdateTaskNotFound(t *testing.T) {
	c, _, ctx := testClient(t)
	title := "x"
	_, err := c.UpdateTask(ctx, "404", ui.UpdateTaskRequest{Title: &title})
	var nf *ui.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error = %v, want a NotFoundError", err)
	}
}

func TestSetCapabilityAttachesAndDetaches(t *testing.T) {
	c, _, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := c.SetCapability(ctx, task.ID, "gemini-key", true); err != nil {
		t.Fatal(err)
	}
	got, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Capabilities) != 1 {
		t.Fatalf("capabilities = %v, want one", got.Capabilities)
	}

	// Attaching twice must not produce two grants.
	if err := c.SetCapability(ctx, task.ID, "gemini-key", true); err != nil {
		t.Fatal(err)
	}
	if got, _ = c.Task(ctx, task.ID); len(got.Capabilities) != 1 {
		t.Fatalf("capabilities after a second attach = %v, want still one", got.Capabilities)
	}

	if err := c.SetCapability(ctx, task.ID, "gemini-key", false); err != nil {
		t.Fatal(err)
	}
	if got, _ = c.Task(ctx, task.ID); len(got.Capabilities) != 0 {
		t.Fatalf("capabilities after detach = %v, want none", got.Capabilities)
	}
	// Detaching one that is not attached is a no-op, matching what
	// removing an absent label used to do.
	if err := c.SetCapability(ctx, task.ID, "gemini-key", false); err != nil {
		t.Fatalf("detaching an absent capability: %v", err)
	}
}

// gcp-key and github-sandbox each had a provider cmd/grain/daemon.go
// registered and no OfferedCapabilities row, so every attempt to attach
// one -- the only way a model.Grant is ever written -- was rejected as
// an unknown capability, and no sandbox on any deployment ever got
// gcpkey.SandboxKeyPath. Both routes a human has are covered here,
// since both validate against the same listing.
func TestGCPKeyAndGitHubSandboxCanBeGranted(t *testing.T) {
	c, _, ctx := testClient(t)

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "mint me a key", Approved: true,
		Capabilities: &[]string{"gcp-key", "github-sandbox"},
	})
	if err != nil {
		t.Fatalf("creating a task granting gcp-key and github-sandbox: %v", err)
	}
	for _, want := range []string{"gcp-key", "github-sandbox"} {
		if !slices.Contains(task.Capabilities, want) {
			t.Errorf("capabilities = %v, want %s among them", task.Capabilities, want)
		}
	}

	plain := create(t, c, ctx)
	for _, id := range []string{"gcp-key", "github-sandbox"} {
		if err := c.SetCapability(ctx, plain.ID, id, true); err != nil {
			t.Errorf("attaching %s: %v", id, err)
		}
	}
}

// A task can be holding a grant this deployment no longer offers -- a
// renamed capability, "scratch-repo" being the one that was here, which
// fails the task's every dispatch at model.ResolveGrants. Detaching it
// has to work, or the only route out is the store itself. Attaching one
// is still refused: that is the check that keeps an unknown id from
// becoming a grant in the first place.
func TestSetCapabilityDetachesAnIDNoLongerOffered(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)
	if err := store.UpdateTask(ctx, task.ID, func(tk *model.Task) error {
		tk.Grants = append(tk.Grants, model.Grant{Capability: "scratch-repo", Via: model.GrantByLabel})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := c.SetCapability(ctx, task.ID, "scratch-repo", false); err != nil {
		t.Fatalf("detaching a capability this deployment no longer offers: %v", err)
	}
	got, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Capabilities) != 0 {
		t.Fatalf("capabilities after detach = %v, want none", got.Capabilities)
	}

	err = c.SetCapability(ctx, task.ID, "scratch-repo", true)
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("attaching an unknown capability: error = %v, want a ValidationError", err)
	}
}

func TestSetDependencyAttachesAndDetaches(t *testing.T) {
	c, _, ctx := testClient(t)
	blocker := create(t, c, ctx)
	task := create(t, c, ctx)

	if err := c.SetDependency(ctx, task.ID, blocker.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Blocked || len(got.DependsOn) != 1 || got.DependsOn[0] != blocker.ID {
		t.Fatalf("task = %+v, want blocked and depending on %s", got, blocker.ID)
	}

	// Attaching twice must not produce two links -- the link table's own
	// primary key already forbids the duplicate, but SetDependency's own
	// mutate closure checks first so a retry cannot even attempt it.
	if err := c.SetDependency(ctx, task.ID, blocker.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ = c.Task(ctx, task.ID); len(got.DependsOn) != 1 {
		t.Fatalf("dependsOn after a second attach = %v, want still one", got.DependsOn)
	}

	if err := c.SetDependency(ctx, task.ID, blocker.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ = c.Task(ctx, task.ID); got.Blocked || len(got.DependsOn) != 0 {
		t.Fatalf("task after detach = %+v, want unblocked with no dependencies", got)
	}
	// Detaching one that is not attached is a no-op, matching SetCapability.
	if err := c.SetDependency(ctx, task.ID, blocker.ID, false); err != nil {
		t.Fatalf("detaching an absent dependency: %v", err)
	}
}

func TestSetDependencyValidates(t *testing.T) {
	c, _, ctx := testClient(t)
	task := create(t, c, ctx)

	if _, err := c.Task(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	var ve *ui.ValidationError

	if err := c.SetDependency(ctx, task.ID, task.ID, true); !errors.As(err, &ve) {
		t.Fatalf("depending on itself: error = %v, want a ValidationError", err)
	}
	if err := c.SetDependency(ctx, task.ID, "404", true); !errors.As(err, &ve) {
		t.Fatalf("depending on an unknown task: error = %v, want a ValidationError", err)
	}
}

func TestApproveMakesAProposalDispatchable(t *testing.T) {
	c, store, ctx := testClient(t)

	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "maybe"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Approve(ctx, task.ID); err != nil {
		t.Fatal(err)
	}

	got, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateQueued {
		t.Fatalf("state = %q, want queued", got.State)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Approval == nil || stored.Approval.Actor.ID != "alice" {
		t.Fatalf("approval = %+v, want it recorded against the configured actor", stored.Approval)
	}
	// Approving again is a no-op rather than an error.
	if err := c.Approve(ctx, task.ID); err != nil {
		t.Fatalf("approving twice: %v", err)
	}
}

// The inverse of TestApproveMakesAProposalDispatchable: a queued task
// goes back to being a proposal, keeping everything else about it, and
// approving it a second time queues it again.
func TestWithdrawApprovalTakesAQueuedTaskBackOutOfTheQueue(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := c.WithdrawApproval(ctx, task.ID); err != nil {
		t.Fatalf("withdrawing approval: %v", err)
	}
	got, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateProposed {
		t.Fatalf("state = %q, want proposed", got.State)
	}
	if got.Title != task.Title {
		t.Fatalf("title = %q, want it untouched at %q", got.Title, task.Title)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Approval != nil || stored.ApprovedAt != nil {
		t.Fatalf("approval = %+v, approvedAt = %v, want both cleared", stored.Approval, stored.ApprovedAt)
	}

	// Withdrawing from a task that carries no approval is a no-op rather
	// than an error, mirroring Approve on an already-approved one.
	if err := c.WithdrawApproval(ctx, task.ID); err != nil {
		t.Fatalf("withdrawing twice: %v", err)
	}
	if err := c.Approve(ctx, task.ID); err != nil {
		t.Fatalf("re-approving: %v", err)
	}
	if got, err = c.Task(ctx, task.ID); err != nil || got.State != model.StateQueued {
		t.Fatalf("state after re-approval = %q (err %v), want queued", got.State, err)
	}
}

// The states where the approval has already been spent on work that
// happened: clearing it there would erase a real queue wait and stop
// nothing, so it is refused rather than quietly rewriting the record.
func TestWithdrawApprovalRefusesATaskThatHasStarted(t *testing.T) {
	c, store, ctx := testClient(t)
	var ve *ui.ValidationError

	running := create(t, c, ctx)
	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: running.ID, Sandbox: "s1", Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := c.WithdrawApproval(ctx, running.ID); !errors.As(err, &ve) {
		t.Fatalf("withdrawing from a running task: error = %v, want a ValidationError", err)
	}

	completed := create(t, c, ctx)
	done := baseTime.Add(time.Hour)
	if err := store.Observe(ctx, model.Observation{TaskID: completed.ID, CompletedAt: &done}); err != nil {
		t.Fatal(err)
	}
	if err := c.WithdrawApproval(ctx, completed.ID); !errors.As(err, &ve) {
		t.Fatalf("withdrawing from a completed task: error = %v, want a ValidationError", err)
	}
	stored, err := store.GetTask(ctx, completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Approval == nil {
		t.Fatal("a refused withdrawal cleared the approval anyway")
	}

	var nf *ui.NotFoundError
	if err := c.WithdrawApproval(ctx, "404"); !errors.As(err, &nf) {
		t.Fatalf("withdrawing from an unknown task: error = %v, want a NotFoundError", err)
	}
}

func TestSubmitOptsIntoTheMergeQueue(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := c.Submit(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutoMerge {
		t.Fatalf("autoMerge = %v, want true after Submit", got.AutoMerge)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.AutoMerge {
		t.Fatalf("stored autoMerge = %v, want true", stored.AutoMerge)
	}
	// Submitting an already-submitted task is a no-op rather than an error.
	if err := c.Submit(ctx, task.ID); err != nil {
		t.Fatalf("submitting twice: %v", err)
	}
}

func TestSubmitNotFound(t *testing.T) {
	c, _, ctx := testClient(t)
	err := c.Submit(ctx, "404")
	var nf *ui.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error = %v, want a NotFoundError", err)
	}
}

func TestAddCommentAppendsToTheConversation(t *testing.T) {
	c, _, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := c.AddComment(ctx, task.ID, "any progress?", nil); err != nil {
		t.Fatal(err)
	}
	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].Body != "any progress?" {
		t.Fatalf("comments = %+v, want the one just posted", detail.Comments)
	}
	if detail.Comments[0].Author != "alice" {
		t.Fatalf("comment author = %q, want the configured actor", detail.Comments[0].Author)
	}
	if err := c.AddComment(ctx, task.ID, "   ", nil); err == nil {
		t.Fatal("an empty comment was accepted")
	}
}

// TestAddCommentAcceptsAttachmentsWithNoBody covers bwsalmon/agents#522's
// "attachable to follow-on comments" for the one case a bare body check
// alone would get wrong: a comment that carries only a file is not the
// same as one with nothing to say at all, so it must not be rejected the
// way an all-whitespace body with no attachment is just above.
func TestAddCommentAcceptsAttachmentsWithNoBody(t *testing.T) {
	c, _, ctx := testClient(t)
	task := create(t, c, ctx)

	upload := ui.AttachmentUpload{
		Filename: "screenshot.png", ContentType: "image/png",
		Content: base64.StdEncoding.EncodeToString([]byte("fake png bytes")),
	}
	if err := c.AddComment(ctx, task.ID, "", []ui.AttachmentUpload{upload}); err != nil {
		t.Fatal(err)
	}
	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Comments) != 1 {
		t.Fatalf("comments = %+v, want exactly one", detail.Comments)
	}
	got := detail.Comments[0].Attachments
	if len(got) != 1 || got[0].Filename != "screenshot.png" || got[0].ContentType != "image/png" {
		t.Fatalf("comment attachments = %+v, want one screenshot.png (image/png)", got)
	}
	if got[0].Size != int64(len("fake png bytes")) {
		t.Fatalf("attachment size = %d, want %d", got[0].Size, len("fake png bytes"))
	}

	content, err := c.Attachment(ctx, task.ID, got[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(content.Content) != "fake png bytes" {
		t.Fatalf("attachment content = %q, want %q", content.Content, "fake png bytes")
	}
}

// TestCreateTaskStoresAttachments covers the other half of bwsalmon/
// agents#522: a file carried by the task's own body (CommentID nil),
// landing on TaskDetail.Attachments rather than any comment's.
func TestCreateTaskStoresAttachments(t *testing.T) {
	c, _, ctx := testClient(t)
	task, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "Fix the thing", Repo: "acme/widgets", Approved: true,
		Attachments: []ui.AttachmentUpload{
			{Filename: "repro.zip", ContentType: "application/zip", Content: base64.StdEncoding.EncodeToString([]byte("PK\x03\x04fake"))},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Attachments) != 1 || detail.Attachments[0].Filename != "repro.zip" {
		t.Fatalf("task attachments = %+v, want one repro.zip", detail.Attachments)
	}
	if len(detail.Comments) != 0 {
		t.Fatalf("a task-body attachment leaked into the conversation: %+v", detail.Comments)
	}
}

// TestCreateTaskRejectsAnOversizedAttachment proves the size limit is
// actually enforced, and that nothing is left behind when it rejects a
// request -- the task must not exist at all, not exist with some
// attachments missing.
func TestCreateTaskRejectsAnOversizedAttachment(t *testing.T) {
	c, _, ctx := testClient(t)
	oversized := base64.StdEncoding.EncodeToString(make([]byte, ui.MaxAttachmentSize+1))
	_, err := c.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "Fix the thing", Repo: "acme/widgets",
		Attachments: []ui.AttachmentUpload{{Filename: "huge.bin", Content: oversized}},
	})
	if err == nil {
		t.Fatal("an oversized attachment was accepted")
	}
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v (%T), want a *ui.ValidationError", err, err)
	}
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("a task was left behind despite the rejected attachment: %+v", tasks)
	}
}

// GetTask surfaces every one of a task's runs, oldest first, so the UI
// can show attempts and their status in order (bwsalmon/agents#445)
// rather than just the failure streak's own count and most recent
// reason.
func TestGetTaskListsEveryAttemptOldestFirst(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", baseTime.Add(10*time.Minute), "failed", "build error"); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r2", TaskID: task.ID, Sandbox: "s1",
		Attempt: 2, StartedAt: baseTime.Add(time.Hour),
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}

	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Attempts) != 2 {
		t.Fatalf("attempts = %+v, want 2", detail.Attempts)
	}
	first, second := detail.Attempts[0], detail.Attempts[1]
	if first.Number != 1 || first.Outcome != "failed" || first.Detail != "build error" || first.FinishedAt == nil {
		t.Fatalf("attempts[0] = %+v, want attempt 1, failed, build error, finished", first)
	}
	if second.Number != 2 || second.Outcome != "" || second.FinishedAt != nil {
		t.Fatalf("attempts[1] = %+v, want attempt 2, still running", second)
	}
}

// TestGetTaskHidesFailedAttemptsOnceTheTaskHasCompleted covers
// bwsalmon/agents#514, the sibling of the bug model.Transitions' own
// guard already fixed for bwsalmon/agents#502. orchestrator.
// salvagePushedBranch turns a pushed branch into a pull request (and the
// task into StateCompleted) even for a run whose own outcome stays
// "failed" forever -- an agent that commits, pushes, and then runs out of
// turns did the work; only the ending failed (orchestrator/cycle.go's own
// comment, orchestrator/finish_test.go's
// TestRunCycleOpensAPullRequestForABranchAFailedRunAlreadyPushed). That
// leaves task_streak sitting at 1 or more forever, since a "succeeded"
// outcome is the only thing that resets it. GetTask read that streak
// straight onto FailedAttempts/LastFailureAt/LastFailureReason with no
// regard for whether the task had since completed or closed -- unlike
// model.StateOf and model.Transitions, which both give obs.CompletedAt/
// ClosedAt precedence over the streak -- so a task's own detail view kept
// showing "1 consecutive failed attempt" (grain get's own "failed
// attempts: N in a row" line) long after the task plainly succeeded.
func TestGetTaskHidesFailedAttemptsOnceTheTaskHasCompleted(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", baseTime.Add(10*time.Minute), "failed", "exceeded max turns (2) without a final answer"); err != nil {
		t.Fatal(err)
	}
	completedAt := baseTime.Add(11 * time.Minute)
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &completedAt}); err != nil {
		t.Fatal(err)
	}

	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != model.StateCompleted {
		t.Fatalf("state = %s, want completed", detail.State)
	}
	if detail.FailedAttempts != 0 || detail.LastFailureAt != nil || detail.LastFailureReason != "" {
		t.Fatalf("FailedAttempts = %d, LastFailureAt = %v, LastFailureReason = %q, want all zero on a completed task",
			detail.FailedAttempts, detail.LastFailureAt, detail.LastFailureReason)
	}
}

// TestRetryClearsAFailedTasksStreak covers bwsalmon/agents#403's own
// "Retry" button (Client.Retry, the UI's handleRetry): once a task has
// failed model.MaxConsecutiveFailures times in a row it reads StateFailed
// forever -- nothing else ever resets task_streak's own count -- until a
// human retries it. Retry itself only stamps Observation.RetryRequestedAt
// (Client.Retry's own doc comment); this proves that stamp actually
// carries all the way through Store.FailureStreak to State and back to a
// dispatchable task, the same round trip TestGetTaskHidesFailedAttempts
// OnceTheTaskHasCompleted proves for completion instead.
func TestRetryClearsAFailedTasksStreak(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	for i := 0; i < model.MaxConsecutiveFailures; i++ {
		id := "r" + strconv.Itoa(i+1)
		started := baseTime.Add(time.Duration(i) * time.Hour)
		if err := store.StartRun(ctx, model.Run{
			ID: id, TaskID: task.ID, Sandbox: "s1",
			Attempt: i + 1, StartedAt: started,
		}, model.Limits{}); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, id, started.Add(time.Minute), "failed", "boom"); err != nil {
			t.Fatal(err)
		}
	}

	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != model.StateFailed {
		t.Fatalf("state = %s, want failed after %d consecutive failures", detail.State, model.MaxConsecutiveFailures)
	}
	if detail.FailedAttempts != model.MaxConsecutiveFailures {
		t.Fatalf("FailedAttempts = %d, want %d", detail.FailedAttempts, model.MaxConsecutiveFailures)
	}

	retryAt := baseTime.Add(model.MaxConsecutiveFailures * time.Hour)
	c.Now = func() time.Time { return retryAt }
	if err := c.Retry(ctx, task.ID); err != nil {
		t.Fatal(err)
	}

	detail, err = c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != model.StateQueued {
		t.Fatalf("state = %s, want queued once retried: an approved task with no failures left is dispatchable", detail.State)
	}
	if detail.FailedAttempts != 0 || detail.LastFailureAt != nil || detail.LastFailureReason != "" {
		t.Fatalf("FailedAttempts = %d, LastFailureAt = %v, LastFailureReason = %q, want all zero once retried",
			detail.FailedAttempts, detail.LastFailureAt, detail.LastFailureReason)
	}

	// The failed attempts themselves are still on record -- retrying
	// forgives the streak, it does not rewrite history.
	if len(detail.Attempts) != model.MaxConsecutiveFailures {
		t.Fatalf("attempts = %d, want retrying to leave every past attempt on record", len(detail.Attempts))
	}
}

// TestRetryOnATaskWithNoFailureIsAHarmlessNoOp covers Client.Retry's own
// doc comment: calling it on a task that is not currently failed must not
// error or otherwise disturb its state.
func TestRetryOnATaskWithNoFailureIsAHarmlessNoOp(t *testing.T) {
	c, _, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := c.Retry(ctx, task.ID); err != nil {
		t.Fatalf("Retry on a never-run task: %v", err)
	}
	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != model.StateQueued {
		t.Fatalf("state = %s, want queued: retrying a task with nothing to retry must be a no-op", detail.State)
	}
}

// TestRetryOnAnUnknownTaskIsNotFound matches the same NotFoundError every
// other single-task Client method returns for an id nothing is behind
// (Close, Reopen, AddComment).
func TestRetryOnAnUnknownTaskIsNotFound(t *testing.T) {
	c, _, ctx := testClient(t)
	err := c.Retry(ctx, "nope")
	var nf *ui.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want a NotFoundError", err)
	}
}

// TestGetTaskListsPullRequestEventsOldestFirst covers bwsalmon/agents#493
// -- "show PR events in the task timeline" -- the same projection
// TestGetTaskListsEveryAttemptOldestFirst above already exercises for
// attempts, but for Observation's own PrOpenedAt/PrMergedAt/PrClosedAt.
func TestGetTaskListsPullRequestEventsOldestFirst(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	opened := baseTime
	merged := baseTime.Add(2 * time.Hour)
	if err := store.Observe(ctx, model.Observation{
		TaskID: task.ID, PrOpenedAt: &opened, PrMergedAt: &merged,
	}); err != nil {
		t.Fatal(err)
	}

	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.PullRequestEvents) != 2 {
		t.Fatalf("pull request events = %+v, want 2", detail.PullRequestEvents)
	}
	if got := detail.PullRequestEvents[0]; got.Kind != "opened" || !got.At.Equal(opened) {
		t.Fatalf("events[0] = %+v, want opened at %v", got, opened)
	}
	if got := detail.PullRequestEvents[1]; got.Kind != "merged" || !got.At.Equal(merged) {
		t.Fatalf("events[1] = %+v, want merged at %v", got, merged)
	}
}

// TestGetTaskHasNoPullRequestEventsForATaskWithNoPullRequest is the empty
// case TestGetTaskListsPullRequestEventsOldestFirst's non-nil Observation
// does not cover: a task nobody has linked a pull request to yet (and so
// orchestrator.SyncPullRequests has never observed).
func TestGetTaskHasNoPullRequestEventsForATaskWithNoPullRequest(t *testing.T) {
	c, _, ctx := testClient(t)
	task := create(t, c, ctx)

	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.PullRequestEvents) != 0 {
		t.Fatalf("pull request events = %+v, want none", detail.PullRequestEvents)
	}
}

// AttemptTranscript is a single attempt's own recorded agent transcript,
// fetched on demand rather than carried on every Attempt GetTask already
// lists (bwsalmon/agents#446 -- "show attempt agent logs").
func TestAttemptTranscript(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", baseTime.Add(10*time.Minute), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunTranscript(ctx, "r1", "read the file, then pushed"); err != nil {
		t.Fatal(err)
	}

	got, err := c.AttemptTranscript(ctx, task.ID, 1)
	if err != nil || got != "read the file, then pushed" {
		t.Fatalf("AttemptTranscript = (%q, %v), want the transcript SetRunTranscript recorded", got, err)
	}

	if _, err := c.AttemptTranscript(ctx, task.ID, 2); err == nil {
		t.Fatal("expected an error for an attempt number with no run behind it")
	}
	if _, err := c.AttemptTranscript(ctx, "nonexistent", 1); err == nil {
		t.Fatal("expected an error for a nonexistent task")
	}
}

// fakeLiveTranscript is a ui.LiveTranscript a test can script with a
// canned response (or none at all) per run ID, without a real transcript
// file anywhere on disk.
type fakeLiveTranscript map[string]string

func (f fakeLiveTranscript) Tail(runID string) (string, bool, error) {
	text, ok := f[runID]
	return text, ok, nil
}

// TestAttemptTranscriptPrefersTheLiveTranscriptWhileARunIsStillGoing is
// bwsalmon/agents#467's whole point: an attempt with no FinishedAt yet
// reads its transcript-in-progress from Config.LiveTranscripts rather
// than waiting on Store.RunTranscript, which has nothing until
// orchestrator.RunDispatch's own SetRunTranscript call lands after the
// run is over.
func TestAttemptTranscriptPrefersTheLiveTranscriptWhileARunIsStillGoing(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)
	c.Config.LiveTranscripts = fakeLiveTranscript{"r1": "still working on it"}

	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}

	got, err := c.AttemptTranscript(ctx, task.ID, 1)
	if err != nil || got != "still working on it" {
		t.Fatalf("AttemptTranscript = (%q, %v), want the live transcript", got, err)
	}
}

// TestAttemptTranscriptFallsBackToTheStoreOnceALiveRunFinishes proves the
// other half: once FinishedAt is set, AttemptTranscript reads
// Store.RunTranscript even when Config.LiveTranscripts still has
// something recorded for that run ID -- the store, not a file that
// orchestrator.RunDispatch has by now already deleted, is authoritative
// for a finished attempt.
func TestAttemptTranscriptFallsBackToTheStoreOnceALiveRunFinishes(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)
	c.Config.LiveTranscripts = fakeLiveTranscript{"r1": "stale in-progress text"}

	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", baseTime.Add(time.Minute), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunTranscript(ctx, "r1", "the real, finished story"); err != nil {
		t.Fatal(err)
	}

	got, err := c.AttemptTranscript(ctx, task.ID, 1)
	if err != nil || got != "the real, finished story" {
		t.Fatalf("AttemptTranscript = (%q, %v), want the stored transcript", got, err)
	}
}

// TestAttemptTranscriptFallsBackToTheStoreWhenLiveHasNothingYet covers a
// still-running attempt whose framework has not written anything to its
// live transcript file yet (or was never given one -- a Framework, for
// now): AttemptTranscript should fall back to Store.RunTranscript (empty,
// but not an error) rather than surface that as a failure.
func TestAttemptTranscriptFallsBackToTheStoreWhenLiveHasNothingYet(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)
	c.Config.LiveTranscripts = fakeLiveTranscript{}

	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatal(err)
	}

	got, err := c.AttemptTranscript(ctx, task.ID, 1)
	if err != nil || got != "" {
		t.Fatalf("AttemptTranscript = (%q, %v), want (\"\", nil)", got, err)
	}
}

// Replying to a parked task resumes it. This used to take two separate
// acts -- post a comment AND re-apply the trigger label so the next poll
// would notice -- and forgetting the second left the task parked forever.
func TestReplyingToAParkedTaskResumesIt(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	agent := model.Principal{Kind: model.PrincipalAgent, ID: "run-1"}
	questionID, err := store.AddComment(ctx, model.Comment{
		TaskID: task.ID,
		Author: model.Attribution{
			Actor:      model.Principal{Kind: model.PrincipalAutomation, ID: "grain"},
			OnBehalfOf: &agent,
		},
		Body: "which endpoint?", CreatedAt: baseTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveField(ctx, task.ID, baseTime, func(o *model.Observation) {
		o.PendingQuestionCommentID = &questionID
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Task(ctx, task.ID); got.State != model.StateAwaitingReply {
		t.Fatalf("state before the reply = %q, want awaiting_reply", got.State)
	}

	if err := c.AddComment(ctx, task.ID, "the second one", nil); err != nil {
		t.Fatal(err)
	}

	got, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateQueued {
		t.Fatalf("state after the reply = %q, want queued: replying is what resumes a parked task", got.State)
	}
	// And the relayed question still renders as grain speaking for the
	// agent rather than as grain's own.
	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Comments[0].OnBehalfOf != "run-1" {
		t.Fatalf("relayed question = %+v, want it attributed on behalf of the run", detail.Comments[0])
	}
}

func TestCloseAndReopen(t *testing.T) {
	c, _, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := c.Close(ctx, task.ID, ui.CloseOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Task(ctx, task.ID); got.State != model.StateClosed {
		t.Fatalf("state after close = %q, want closed", got.State)
	}
	if err := c.Reopen(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Task(ctx, task.ID); got.State != model.StateQueued {
		t.Fatalf("state after reopen = %q, want back to queued", got.State)
	}
}

func TestListTasksCarriesEveryTaskWithItsState(t *testing.T) {
	c, _, ctx := testClient(t)
	queued := create(t, c, ctx)
	proposed, err := c.CreateTask(ctx, ui.CreateTaskRequest{Title: "maybe"})
	if err != nil {
		t.Fatal(err)
	}

	tasks, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("listed %d tasks, want 2", len(tasks))
	}
	states := map[string]model.State{}
	for _, task := range tasks {
		states[task.ID] = task.State
	}
	if states[queued.ID] != model.StateQueued || states[proposed.ID] != model.StateProposed {
		t.Fatalf("states = %v, want each task's own", states)
	}
}

// TestListTasksIsDispatchOrder: the list a UI or CLI gets reads
// top-to-bottom in the order grain will work through it (grain/task-201).
// Three tasks created in order, with NewestFirst left at its default, are
// listed in that same order -- each one joined the end of the backlog, so
// the first one filed is both at the top and the next to run. This used
// to be the reverse (newest first), which put the task that would run
// next at the bottom of the list.
func TestListTasksIsDispatchOrder(t *testing.T) {
	c, store, ctx := testClient(t)
	first := create(t, c, ctx)
	second := create(t, c, ctx)
	third := create(t, c, ctx)

	tasks, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{tasks[0].ID, tasks[1].ID, tasks[2].ID}
	want := []string{first.ID, second.ID, third.ID}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListTasks order = %v, want dispatch order %v", got, want)
	}

	// The list is not merely in some fixed order: it is Ready's order,
	// which is the whole claim being made to whoever reads it.
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ready, got) {
		t.Fatalf("Ready = %v, want the listed order %v", ready, got)
	}
}

// TestNewestFirstSettingMovesNewTasksToTheFrontOfTheQueue is
// bwsalmon/agents#476's global switch, now that it is only about where
// new work joins the backlog (grain/task-201): with
// model.Config.NewestFirst set, a task created after two others is
// dispatched before them instead of after -- and, because the list is
// dispatch order, it is at the top of it for exactly that reason.
func TestNewestFirstSettingMovesNewTasksToTheFrontOfTheQueue(t *testing.T) {
	c, store, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}
	newestFirst := true
	if _, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{NewestFirst: &newestFirst}); err != nil {
		t.Fatal(err)
	}

	first := create(t, c, ctx)
	second := create(t, c, ctx)
	third := create(t, c, ctx)

	tasks, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{tasks[0].ID, tasks[1].ID, tasks[2].ID}
	want := []string{third.ID, second.ID, first.ID}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListTasks order under NewestFirst = %v, want newest first %v", got, want)
	}

	// And it is at the top because it runs first, not as a display
	// convention of its own: Ready agrees, task for task.
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ready, want) {
		t.Fatalf("Ready under NewestFirst = %v, want newest dispatched first %v", ready, want)
	}
}

// TestCreateTaskFilesAtTheEndTheRequestNames is grain/task-202's own
// half of the same choice: whoever files a task says which end of the
// backlog it joins on the request itself
// (ui.CreateTaskRequest.AtFront), rather than by going and flipping a
// deployment-wide setting first. The request wins over what is stored --
// here NewestFirst is off and the third task still runs next.
func TestCreateTaskFilesAtTheEndTheRequestNames(t *testing.T) {
	c, store, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	first := create(t, c, ctx)
	second := create(t, c, ctx)
	third := createAt(t, c, ctx, true)

	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{third.ID, first.ID, second.ID}
	if !reflect.DeepEqual(ready, want) {
		t.Fatalf("Ready after filing one at the front = %v, want %v", ready, want)
	}
}

// TestCreateTaskRemembersWhichEndTheLastTaskJoined: the choice is
// sticky, which is the whole of grain/task-202 beyond the field itself.
// A task filed at the front stores that as the deployment's own default
// (model.Config.NewestFirst), so the next task filed with no opinion at
// all -- another form, the CLI, a second person -- joins the same end,
// and the Settings pane and the new-task form both read it back.
func TestCreateTaskRemembersWhichEndTheLastTaskJoined(t *testing.T) {
	c, store, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	first := create(t, c, ctx)
	second := createAt(t, c, ctx, true)
	// No opinion: it inherits what the task before it chose rather than
	// falling back to the end of the backlog.
	third := create(t, c, ctx)

	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{third.ID, second.ID, first.ID}
	if !reflect.DeepEqual(ready, want) {
		t.Fatalf("Ready after remembering the front = %v, want %v", ready, want)
	}

	settings, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.NewestFirst {
		t.Fatalf("Settings.NewestFirst after filing a task at the front = false, want true")
	}

	// And back the other way, so it is a memory of the last filing
	// rather than a one-way switch.
	fourth := createAt(t, c, ctx, false)
	fifth := create(t, c, ctx)
	if ready, err = store.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	want = []string{third.ID, second.ID, first.ID, fourth.ID, fifth.ID}
	if !reflect.DeepEqual(ready, want) {
		t.Fatalf("Ready after remembering the end again = %v, want %v", ready, want)
	}
}

// A task filed with no opinion changes nothing about where the next one
// goes: only a stated choice is remembered, so a scheduled or scripted
// filing cannot quietly reset what a human picked.
func TestCreateTaskWithNoOpinionLeavesTheRememberedEndAlone(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}
	createAt(t, c, ctx, true)
	create(t, c, ctx)

	settings, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.NewestFirst {
		t.Fatalf("Settings.NewestFirst after a filing that stated nothing = false, want the remembered true")
	}
}

// TestReorderMovesATaskInTheBacklog exercises Client.Reorder end to end
// -- the endpoint TaskList.jsx's drag-and-drop drop handler calls -- and
// checks the move is visible through both ListTasks (the display) and
// Ready (the dispatch order), since both read the same OrderKey column.
func TestReorderMovesATaskInTheBacklog(t *testing.T) {
	c, store, ctx := testClient(t)
	first := create(t, c, ctx)
	second := create(t, c, ctx)
	third := create(t, c, ctx)

	// Backlog (dispatch) order today is oldest first: first, second, third.
	// Move third to the very front -- "just before the following job" when
	// dropped at the head of the list, the same wording the issue uses.
	if err := c.Reorder(ctx, ui.ReorderRequest{IDs: []string{third.ID}, BeforeID: first.ID}); err != nil {
		t.Fatal(err)
	}

	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{third.ID, first.ID, second.ID}; !reflect.DeepEqual(ready, want) {
		t.Fatalf("Ready after Reorder = %v, want %v", ready, want)
	}

	// And the list a UI shows is that same order, not its reverse: a task
	// dragged to the head of the list is the one that runs next, which is
	// the whole point of being able to drag it there.
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{tasks[0].ID, tasks[1].ID, tasks[2].ID}
	if want := []string{third.ID, first.ID, second.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListTasks after Reorder = %v, want %v", got, want)
	}
}

func TestReorderValidatesIDsIsNotEmpty(t *testing.T) {
	c, _, ctx := testClient(t)
	err := c.Reorder(ctx, ui.ReorderRequest{})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Reorder with no ids: error = %v, want a ValidationError", err)
	}
}

// AuthorKind is what lets the frontend tell a task somebody filed by
// hand from one grain or a dispatched run filed for itself -- Author
// alone cannot, since a login, a run ID and a deployment name are all
// just strings. state.js's lastBaseForRepo reads it to keep a
// system-generated task's base branch out of the human's own "Base
// branch" default.
func TestAuthorKindCarriesTheFilingPrincipalsKind(t *testing.T) {
	c, store, ctx := testClient(t)
	filed := create(t, c, ctx)
	if filed.AuthorKind != string(model.PrincipalHuman) {
		t.Fatalf("AuthorKind of a task filed through the client = %q, want human", filed.AuthorKind)
	}

	id, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scheduled := model.Task{
		ID:     id,
		Intent: model.IntentImplement,
		Title:  "nightly sweep",
		Body:   "filed by a schedule",
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalAutomation, ID: "grain"}},
			Reason:      model.ReasonSchedule,
		},
		CreatedAt: &baseTime,
	}
	if err := store.PutTask(ctx, scheduled); err != nil {
		t.Fatal(err)
	}

	got, err := c.Task(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthorKind != string(model.PrincipalAutomation) {
		t.Fatalf("AuthorKind of a scheduled task = %q, want automation", got.AuthorKind)
	}
}

// GeneratedFrom is read off the task's own LinkProposedBy link -- the
// same provenance relayProposedTasks (pkg/orchestrator/finish.go) sets
// automatically on every task a propose_task call files, surfaced here
// for the UI rather than left for a human to dig out of the store.
func TestGeneratedFromReadsOffTheProposedByLink(t *testing.T) {
	c, store, ctx := testClient(t)
	source := create(t, c, ctx)

	id, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proposal := model.Task{
		ID:     id,
		Intent: model.IntentImplement,
		Title:  "proposed child",
		Body:   "filed by the parent's own run",
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalAutomation, ID: "grain"}},
			Reason:      model.ReasonDirect,
		},
		Links:     []model.Link{{Kind: model.LinkProposedBy, Target: source.ID}},
		CreatedAt: &baseTime,
	}
	if err := store.PutTask(ctx, proposal); err != nil {
		t.Fatal(err)
	}

	got, err := c.Task(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.GeneratedFrom != source.ID {
		t.Fatalf("GeneratedFrom = %q, want %q", got.GeneratedFrom, source.ID)
	}

	if source.GeneratedFrom != "" {
		t.Fatalf("source task GeneratedFrom = %q, want empty: nothing proposed it", source.GeneratedFrom)
	}
}

// Stacked is true only for a task whose Origin.Reason is model.ReasonFix
// -- the merge queue's own automatic fix for another task's pull
// request (bwsalmon/agents#378) -- and false for an ordinary
// propose_task child, even though both carry a GeneratedFrom link. The
// frontend nests the former under the task named by GeneratedFrom and
// leaves the latter as a task of its own.
func TestStackedIsTrueOnlyForAFixTask(t *testing.T) {
	c, store, ctx := testClient(t)
	source := create(t, c, ctx)

	fixID, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fixTask := model.Task{
		ID:     fixID,
		Intent: model.IntentImplement,
		Title:  "fix",
		Body:   "filed by the merge queue",
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalAutomation, ID: "merge-queue"}},
			Reason:      model.ReasonFix,
		},
		Links:     []model.Link{{Kind: model.LinkProposedBy, Target: source.ID}},
		CreatedAt: &baseTime,
	}
	if err := store.PutTask(ctx, fixTask); err != nil {
		t.Fatal(err)
	}

	proposalID, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proposal := model.Task{
		ID:     proposalID,
		Intent: model.IntentImplement,
		Title:  "proposed child",
		Body:   "filed by the parent's own run",
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalAutomation, ID: "grain"}},
			Reason:      model.ReasonDirect,
		},
		Links:     []model.Link{{Kind: model.LinkProposedBy, Target: source.ID}},
		CreatedAt: &baseTime,
	}
	if err := store.PutTask(ctx, proposal); err != nil {
		t.Fatal(err)
	}

	gotFix, err := c.Task(ctx, fixID)
	if err != nil {
		t.Fatal(err)
	}
	if !gotFix.Stacked {
		t.Fatalf("fix task Stacked = false, want true")
	}

	gotProposal, err := c.Task(ctx, proposalID)
	if err != nil {
		t.Fatal(err)
	}
	if gotProposal.Stacked {
		t.Fatalf("proposed child Stacked = true, want false")
	}
}

// MergeQueueBlockedAt is bwsalmon/agents#494's own signal that a
// completed task's pull request needs a human, not another automatic
// merge-queue attempt -- ListTasks, Task and GetTask each project it
// off model.Observation through a different path (a bulk query, a
// single Store.GetObservation call, and one already fetched for
// Transitions), so all three are worth covering here rather than
// trusting they agree.
func TestTaskSurfacesMergeQueueBlockedAt(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}

	got, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MergeQueueBlockedAt != nil {
		t.Fatalf("MergeQueueBlockedAt = %v, want nil before the merge queue gives up", got.MergeQueueBlockedAt)
	}

	if err := store.Observe(ctx, model.Observation{
		TaskID: task.ID, CompletedAt: &baseTime, MergeQueueBlockedAt: &baseTime,
	}); err != nil {
		t.Fatal(err)
	}

	got, err = c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MergeQueueBlockedAt == nil || !got.MergeQueueBlockedAt.Equal(baseTime) {
		t.Fatalf("Task: MergeQueueBlockedAt = %v, want %v", got.MergeQueueBlockedAt, baseTime)
	}

	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.MergeQueueBlockedAt == nil || !detail.MergeQueueBlockedAt.Equal(baseTime) {
		t.Fatalf("GetTask: MergeQueueBlockedAt = %v, want %v", detail.MergeQueueBlockedAt, baseTime)
	}

	list, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].MergeQueueBlockedAt == nil || !list[0].MergeQueueBlockedAt.Equal(baseTime) {
		t.Fatalf("ListTasks: MergeQueueBlockedAt = %v, want %v", list[0].MergeQueueBlockedAt, baseTime)
	}
}

// Repairing is the same shape of signal for the other thing the merge
// queue does to a task it is driving: sends it back to an agent to repair
// its own pull request branch (model.Observation.MergeQueueRepairAt).
// Such a task reads 'running' or 'queued' like any other attempt, so
// without this a list has no way to tell "still being written" from
// "written, merged nowhere, and now being unstuck" -- which is what the
// frontend colours the running mark on.
//
// The three projections take three different paths to it, exactly as
// above, and the bulk one (Store.MergeQueueRepairing) is a second
// implementation of model.Observation.RepairInFlight in SQL -- so all
// three are worth covering rather than trusting they agree.
func TestTaskSurfacesAMergeQueueRepairInFlight(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)

	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repairing {
		t.Fatal("Repairing = true for a task the merge queue has not repaired")
	}

	// The requeue: what orchestrator.requeueForRepair writes.
	askedAt := baseTime.Add(time.Hour)
	if err := store.Observe(ctx, model.Observation{
		TaskID: task.ID, MergeQueueRepairAt: &askedAt,
	}); err != nil {
		t.Fatal(err)
	}

	got, err = c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Repairing {
		t.Fatal("Task: Repairing = false while a repair is in flight")
	}
	detail, err := c.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Repairing {
		t.Fatal("GetTask: Repairing = false while a repair is in flight")
	}
	list, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Repairing {
		t.Fatalf("ListTasks: Repairing = %v, want true", list)
	}

	// The repair run finishes: the task completes again, and it is an
	// ordinary completed task once more.
	finished := askedAt.Add(time.Hour)
	if err := store.Observe(ctx, model.Observation{
		TaskID: task.ID, MergeQueueRepairAt: &askedAt, CompletedAt: &finished,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = c.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repairing {
		t.Fatal("Task: Repairing = true after the repair completed")
	}
	list, err = c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Repairing {
		t.Fatalf("ListTasks: Repairing = %v after the repair completed, want false", list)
	}
}

func TestTaskNotFound(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.Task(ctx, "404")
	var nf *ui.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error = %v, want a NotFoundError", err)
	}
}

func TestGetSettingsIsUnconfiguredOnAFreshStore(t *testing.T) {
	c, _, ctx := testClient(t)
	got, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Configured {
		t.Fatalf("got %+v, want Configured false before anything has ever been saved", got)
	}
}

func firstSettings() ui.UpdateSettingsRequest {
	pollInterval, maxWorkers, geminiModel, claudeModel, host := "30s", 1, "gemini-2.5-pro", "claude-sonnet-5", "github.com"
	return ui.UpdateSettingsRequest{
		PollInterval: &pollInterval, MaxWorkers: &maxWorkers,
		GeminiModel: &geminiModel, ClaudeModel: &claudeModel, GitHubHost: &host,
	}
}

func TestUpdateSettingsFirstTimeRequiresTheCoreFields(t *testing.T) {
	c, _, ctx := testClient(t)

	full := firstSettings()
	if _, err := c.UpdateSettings(ctx, full); err != nil {
		t.Fatalf("saving with every core field given: %v", err)
	}

	c2, _, ctx2 := testClient(t)
	maxWorkers := 1
	_, err := c2.UpdateSettings(ctx2, ui.UpdateSettingsRequest{MaxWorkers: &maxWorkers})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("saving settings for the first time with pollInterval missing: error = %v, want a ValidationError", err)
	}
}

func TestUpdateSettingsThenGetRoundTrips(t *testing.T) {
	c, store, ctx := testClient(t)

	got, err := c.UpdateSettings(ctx, firstSettings())
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if !got.Configured || got.PollInterval != "30s" || got.GeminiModel != "gemini-2.5-pro" ||
		got.GitHubHost != "github.com" {
		t.Fatalf("UpdateSettings returned %+v", got)
	}

	read, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, got) {
		t.Fatalf("GetSettings = %+v, want the just-written %+v", read, got)
	}

	// And it actually landed in the store UpdateSettings itself reads and
	// writes -- not some copy this package keeps of its own.
	stored, err := store.GetConfig(ctx)
	if err != nil || stored == nil {
		t.Fatalf("store.GetConfig: (%+v, %v)", stored, err)
	}
	if stored.GeminiModel != "gemini-2.5-pro" {
		t.Fatalf("store's own config = %+v", stored)
	}
}

// grain/task-63: a first save that never mentions maxMergers stores
// model.DefaultConfig's own value for it rather than a zero nobody
// chose, and a later save can set it -- including back to 0, which means
// "mergers contend for worker capacity" rather than "unset".
func TestUpdateSettingsDefaultsAndRoundTripsMaxMergers(t *testing.T) {
	c, _, ctx := testClient(t)

	got, err := c.UpdateSettings(ctx, firstSettings())
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if got.MaxMergers != model.DefaultMaxMergers {
		t.Fatalf("maxMergers after a first save that never mentioned it = %d, want DefaultMaxMergers (%d)",
			got.MaxMergers, model.DefaultMaxMergers)
	}

	three := 3
	got, err = c.UpdateSettings(ctx, ui.UpdateSettingsRequest{MaxMergers: &three})
	if err != nil {
		t.Fatalf("setting maxMergers: %v", err)
	}
	if got.MaxMergers != 3 || got.MaxWorkers != 1 {
		t.Fatalf("settings = %+v, want maxMergers 3 with maxWorkers left at 1", got)
	}

	none := 0
	got, err = c.UpdateSettings(ctx, ui.UpdateSettingsRequest{MaxMergers: &none})
	if err != nil {
		t.Fatalf("clearing maxMergers: %v", err)
	}
	if got.MaxMergers != 0 {
		t.Fatalf("maxMergers = %d after being set to 0, want 0 kept as the choice it is", got.MaxMergers)
	}
}

// A later partial update changes only the fields given, leaving
// everything else -- including fields with no UI equivalent yet, like
// GCPProject -- exactly as they were, the same UpdateTaskRequest
// convention.
func TestUpdateSettingsChangesOnlyTheFieldsGiven(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	turns := 12
	got, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{MaxAgentTurns: &turns})
	if err != nil {
		t.Fatalf("partial update: %v", err)
	}
	if got.MaxAgentTurns != 12 {
		t.Fatalf("maxAgentTurns = %d, want 12", got.MaxAgentTurns)
	}
	if got.PollInterval != "30s" || got.GeminiModel != "gemini-2.5-pro" || got.GitHubHost != "github.com" {
		t.Fatalf("partial update changed fields it was not given: %+v", got)
	}
}

// bwsalmon/agents#534, grain/task-41: SandboxCPUs/SandboxMemoryMB/
// SandboxDiskGB (the deployment-wide
// default sandbox shape) round-trip through UpdateSettings/GetSettings
// the same as every other store-backed field, and 0 -- the "unset, use
// grain's own default shape" zero value -- is valid, unlike
// MaxWorkers's own "must be at least 1".
func TestUpdateSettingsSandboxShapeRoundTrips(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	cpus, memoryMB, diskGB := 4, 8192, 40
	got, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{
		SandboxCPUs: &cpus, SandboxMemoryMB: &memoryMB, SandboxDiskGB: &diskGB,
	})
	if err != nil {
		t.Fatalf("setting sandbox shape: %v", err)
	}
	if got.SandboxCPUs != 4 || got.SandboxMemoryMB != 8192 || got.SandboxDiskGB != 40 {
		t.Fatalf("sandboxCpus/sandboxMemoryMb/sandboxDiskGb = %d/%d/%d, want 4/8192/40",
			got.SandboxCPUs, got.SandboxMemoryMB, got.SandboxDiskGB)
	}

	read, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if read.SandboxCPUs != 4 || read.SandboxMemoryMB != 8192 || read.SandboxDiskGB != 40 {
		t.Fatalf("GetSettings sandboxCpus/sandboxMemoryMb/sandboxDiskGb = %d/%d/%d, want 4/8192/40",
			read.SandboxCPUs, read.SandboxMemoryMB, read.SandboxDiskGB)
	}

	zero := 0
	got, err = c.UpdateSettings(ctx, ui.UpdateSettingsRequest{
		SandboxCPUs: &zero, SandboxMemoryMB: &zero, SandboxDiskGB: &zero,
	})
	if err != nil {
		t.Fatalf("clearing sandbox shape: %v", err)
	}
	if got.SandboxCPUs != 0 || got.SandboxMemoryMB != 0 || got.SandboxDiskGB != 0 {
		t.Fatalf("sandboxCpus/sandboxMemoryMb/sandboxDiskGb after clearing = %d/%d/%d, want 0/0/0",
			got.SandboxCPUs, got.SandboxMemoryMB, got.SandboxDiskGB)
	}
}

// Unlike MaxWorkers, an empty TargetRepos is meaningful (unrestricted)
// rather than rejected -- v1's target_repos "leave empty for a
// single-repo deployment."
func TestUpdateSettingsTargetReposRoundTripsIncludingEmpty(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	repos := []string{"acme/widgets", "acme/gadgets"}
	got, err := c.UpdateSettings(ctx, ui.UpdateSettingsRequest{TargetRepos: &repos})
	if err != nil {
		t.Fatalf("setting target repos: %v", err)
	}
	if !reflect.DeepEqual(got.TargetRepos, repos) {
		t.Fatalf("targetRepos = %v, want %v", got.TargetRepos, repos)
	}

	empty := []string{}
	got, err = c.UpdateSettings(ctx, ui.UpdateSettingsRequest{TargetRepos: &empty})
	if err != nil {
		t.Fatalf("clearing target repos: %v", err)
	}
	if len(got.TargetRepos) != 0 {
		t.Fatalf("targetRepos = %v, want cleared", got.TargetRepos)
	}
}

func TestUpdateSettingsValidates(t *testing.T) {
	c, _, ctx := testClient(t)
	if _, err := c.UpdateSettings(ctx, firstSettings()); err != nil {
		t.Fatal(err)
	}

	bad := "not-a-duration"
	empty := ""
	negative := -1
	zeroWorkers := 0
	negativeMergers := -1
	badRepo := []string{"not-owner-slash-name"}
	negativeCPUs := -1
	lowMemory := 64
	negativeDisk := -1
	cases := map[string]ui.UpdateSettingsRequest{
		"unparseable poll interval": {PollInterval: &bad},
		"zero max workers":          {MaxWorkers: &zeroWorkers},
		"negative max mergers":      {MaxMergers: &negativeMergers},
		"blank gemini model":        {GeminiModel: &empty},
		"blank github host":         {GitHubHost: &empty},
		"negative max agent turns":  {MaxAgentTurns: &negative},
		"malformed target repo":     {TargetRepos: &badRepo},
		"negative sandbox cpus":     {SandboxCPUs: &negativeCPUs},
		"sandbox memory below 128":  {SandboxMemoryMB: &lowMemory},
		"negative sandbox disk":     {SandboxDiskGB: &negativeDisk},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.UpdateSettings(ctx, req)
			var ve *ui.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %v, want a ValidationError", err)
			}
		})
	}
}
