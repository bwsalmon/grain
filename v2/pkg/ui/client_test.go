package ui_test

// pkg/ui against a real embedded SQLite store, the same discipline every
// other package's own tests hold to (model/simulate_test.go: "Nothing
// here is a fake standing in for the store"). These used to run against
// an in-memory github.Client stand-in, because a task was a GitHub issue;
// there is no GitHub in this package at all now.

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
	"github.com/bwsalmon/grain/v2/pkg/ui"
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
		Capabilities:  ui.DefaultCapabilities(),
	}, store)
	client.Now = func() time.Time { return baseTime }
	return client, store, ctx
}

// create is the common setup: an approved task, so it reads queued.
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
	if err := c.AddComment(ctx, task.ID, "widened targetRepos, this can run now"); err != nil {
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
		Title: "needs a key", Approved: true, Capabilities: []string{"gemini-key"},
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

	if err := c.Close(ctx, blocker.ID); err != nil {
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
		"empty title":        {Title: "  "},
		"unknown capability": {Title: "t", Capabilities: []string{"nope"}},
		"unparseable repo":   {Title: "t", Repo: "not-a-repo"},
		"unknown dependency": {Title: "t", DependsOn: []string{"404"}},
		"unparseable read":   {Title: "t", Reads: []string{"not-a-repo"}},
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
	for name, req := range map[string]ui.UpdateTaskRequest{
		"empty title": {Title: &blank},
		// Clearing the target is rejected rather than allowed: a task with
		// no target cannot be dispatched, and it is a real column now
		// rather than an optional directive line that could just be absent.
		"empty repo":       {Repo: &blank},
		"unparseable repo": {Repo: &bad},
		"unparseable read": {Reads: &[]string{"not-a-repo"}},
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

	if err := c.AddComment(ctx, task.ID, "any progress?"); err != nil {
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
	if err := c.AddComment(ctx, task.ID, "   "); err == nil {
		t.Fatal("an empty comment was accepted")
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
		ID: "r1", TaskID: task.ID, Slot: "s1", Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, "r1", baseTime.Add(10*time.Minute), "failed", "build error"); err != nil {
		t.Fatal(err)
	}
	if err := store.StartRun(ctx, model.Run{
		ID: "r2", TaskID: task.ID, Slot: "s1", Sandbox: "s1",
		Attempt: 2, StartedAt: baseTime.Add(time.Hour),
	}); err != nil {
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
		ID: "r1", TaskID: task.ID, Slot: "s1", Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}); err != nil {
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
			ID: id, TaskID: task.ID, Slot: "s1", Sandbox: "s1",
			Attempt: i + 1, StartedAt: started,
		}); err != nil {
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
		ID: "r1", TaskID: task.ID, Slot: "s1", Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}); err != nil {
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
		ID: "r1", TaskID: task.ID, Slot: "s1", Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}); err != nil {
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
		ID: "r1", TaskID: task.ID, Slot: "s1", Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}); err != nil {
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
// live transcript file yet (or was never given one -- agent/gemini, for
// now): AttemptTranscript should fall back to Store.RunTranscript (empty,
// but not an error) rather than surface that as a failure.
func TestAttemptTranscriptFallsBackToTheStoreWhenLiveHasNothingYet(t *testing.T) {
	c, store, ctx := testClient(t)
	task := create(t, c, ctx)
	c.Config.LiveTranscripts = fakeLiveTranscript{}

	if err := store.StartRun(ctx, model.Run{
		ID: "r1", TaskID: task.ID, Slot: "s1", Sandbox: "s1",
		Attempt: 1, StartedAt: baseTime,
	}); err != nil {
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

	if err := c.AddComment(ctx, task.ID, "the second one"); err != nil {
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

	if err := c.Close(ctx, task.ID); err != nil {
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

// TestListTasksDefaultsToNewestFirst is grain's original shape
// (bwsalmon/agents#476), unchanged by OrderKey's arrival: three tasks
// created in order end up listed in the reverse of that order, even
// though Store.ListTasks itself now hands back the opposite (ascending
// OrderKey, dispatch order) and this package's own ListTasks reverses it.
func TestListTasksDefaultsToNewestFirst(t *testing.T) {
	c, _, ctx := testClient(t)
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
		t.Fatalf("ListTasks order = %v, want newest first %v", got, want)
	}
}

// TestNewestFirstSettingMovesNewTasksToTheFrontOfTheQueue is
// bwsalmon/agents#476's global switch: with model.Config.NewestFirst set,
// a task created after two others still shows up first in the task list
// (same as the default), but Store.Ready now dispatches it before them
// too, instead of after -- the whole point of the setting.
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
		t.Fatalf("ListTasks order under NewestFirst = %v, want still newest first %v", got, want)
	}

	// Dispatch order, not just display order, now runs newest to oldest.
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ready, want) {
		t.Fatalf("Ready under NewestFirst = %v, want newest dispatched first %v", ready, want)
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

	// And the list a UI shows reflects the same move, oldest-insertion
	// convention aside: it is still the reverse of Ready, since NewestFirst
	// was never switched on in this test.
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{tasks[0].ID, tasks[1].ID, tasks[2].ID}
	if want := []string{second.ID, first.ID, third.ID}; !reflect.DeepEqual(got, want) {
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
	pollInterval, maxConcurrent, geminiModel, host := "30s", 1, "gemini-2.5-pro", "github.com"
	return ui.UpdateSettingsRequest{
		PollInterval: &pollInterval, MaxConcurrent: &maxConcurrent, GeminiModel: &geminiModel, GitHubHost: &host,
	}
}

func TestUpdateSettingsFirstTimeRequiresTheCoreFields(t *testing.T) {
	c, _, ctx := testClient(t)

	full := firstSettings()
	if _, err := c.UpdateSettings(ctx, full); err != nil {
		t.Fatalf("saving with every core field given: %v", err)
	}

	c2, _, ctx2 := testClient(t)
	maxConcurrent := 1
	_, err := c2.UpdateSettings(ctx2, ui.UpdateSettingsRequest{MaxConcurrent: &maxConcurrent})
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

// Unlike MaxConcurrent, an empty TargetRepos is meaningful (unrestricted)
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
	zeroConcurrent := 0
	badRepo := []string{"not-owner-slash-name"}
	cases := map[string]ui.UpdateSettingsRequest{
		"unparseable poll interval": {PollInterval: &bad},
		"zero max concurrent":       {MaxConcurrent: &zeroConcurrent},
		"blank gemini model":        {GeminiModel: &empty},
		"blank github host":         {GitHubHost: &empty},
		"negative max agent turns":  {MaxAgentTurns: &negative},
		"malformed target repo":     {TargetRepos: &badRepo},
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
