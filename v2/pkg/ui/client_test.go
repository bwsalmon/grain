package ui_test

// pkg/ui against a real embedded Dolt store, the same discipline every
// other package's own tests hold to (model/simulate_test.go: "Nothing
// here is a fake standing in for the store"). These used to run against
// an in-memory github.Client stand-in, because a task was a GitHub issue;
// there is no GitHub in this package at all now.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

var baseTime = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func testClient(t *testing.T) (*ui.Client, *model.Store, context.Context) {
	t.Helper()
	db, err := dolt.Open(dolt.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded dolt: %v", err)
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
	if stored.ExternalRef != "" {
		t.Fatalf("external ref = %q, want empty: nothing files an issue any more", stored.ExternalRef)
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

func TestCreateTaskValidates(t *testing.T) {
	c, _, ctx := testClient(t)

	for name, req := range map[string]ui.CreateTaskRequest{
		"empty title":        {Title: "  "},
		"unknown capability": {Title: "t", Capabilities: []string{"nope"}},
		"unparseable repo":   {Title: "t", Repo: "not-a-repo"},
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
	got, err := c.UpdateTask(ctx, task.ID, ui.UpdateTaskRequest{
		Repo: &repo, Base: &base, AutoMerge: &autoMerge,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != "other/repo" || got.Base != "release" || !got.AutoMerge {
		t.Fatalf("task = %+v, want the edited fields", got)
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

func TestTaskNotFound(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.Task(ctx, "404")
	var nf *ui.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error = %v, want a NotFoundError", err)
	}
}
