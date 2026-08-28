package ui

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
)

// The store-backed half of this package's tests: against a real embedded
// Dolt database, the same way pkg/model/dolt/store_test.go proves the
// store itself, since a memClient alone has nothing to back the
// model.Store seam these tests exercise. Every task seeded here is
// "tracked" -- filed into the store, its trigger label already removed --
// mirroring what pkg/orchestrator.PollIssues would have left behind by
// the time a UI request ever sees it.

var testRepo = model.RepoRef{Owner: "acme", Name: "tasks"}

func openTestStore(t *testing.T) *model.Store {
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
	return store
}

func testClientWithStore(t *testing.T) (*Client, *memClient, *model.Store) {
	store := openTestStore(t)
	client := newMemClient()
	cfg := Config{
		TaskRepo:     testRepo,
		Labels:       DefaultLabels(),
		Capabilities: DefaultCapabilities(),
	}
	return NewClient(cfg, client, store), client, store
}

// seedTracked files number as a tracked task -- a model.Task row in the
// store, plus the GitHub issue pollIssue would have left behind (open,
// trigger label already removed) -- the same two-sided state
// pkg/orchestrator.PollIssues produces.
func seedTracked(t *testing.T, ctx context.Context, gh *memClient, store *model.Store, number int, title, body string, target *model.RepoRef) model.Task {
	t.Helper()
	gh.seed(number, title, body)
	task := model.Task{
		ID:     storeTaskID(testRepo, number),
		Intent: model.IntentImplement,
		Title:  title,
		Body:   body,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "filer"}},
			Reason:      model.ReasonDirect,
		},
		Approval:    &model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "filer"}},
		Target:      target,
		Binding:     model.BindingDirective,
		ExternalRef: model.ExternalRef(testRepo, number),
	}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("filing task #%d: %v", number, err)
	}
	return task
}

func TestTrackedTaskReadsStateFromTheStore(t *testing.T) {
	ctx := context.Background()
	c, gh, store := testClientWithStore(t)
	target := &model.RepoRef{Owner: "acme", Name: "widget"}
	seedTracked(t, ctx, gh, store, 10, "tracked task", "do the thing\n\n/repo acme/widget", target)

	task, err := c.Task(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != StateQueued {
		t.Errorf("state = %q, want queued", task.State)
	}
	if task.Repo != "acme/widget" {
		t.Errorf("repo = %q, want acme/widget", task.Repo)
	}
	if task.Description != "do the thing" {
		t.Errorf("description = %q, want directives stripped", task.Description)
	}
	if task.HTMLURL != "https://github.com/acme/tasks/issues/10" {
		t.Errorf("htmlUrl = %q, built with no GitHub call expected", task.HTMLURL)
	}

	// A run recorded on the store shows up as run history off GetTask.
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.StartRun(ctx, model.Run{
		ID: "run-1", TaskID: storeTaskID(testRepo, 10), Slot: "local", Sandbox: "local",
		Attempt: 1, StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	detail, err := c.GetTask(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Runs) != 1 || detail.Runs[0].ID != "run-1" {
		t.Fatalf("runs = %+v, want one run-1", detail.Runs)
	}
}

func TestTrackedTaskRunningState(t *testing.T) {
	ctx := context.Background()
	c, gh, store := testClientWithStore(t)
	seedTracked(t, ctx, gh, store, 11, "running task", "body", nil)
	if err := store.StartRun(ctx, model.Run{
		ID: "run-11", TaskID: storeTaskID(testRepo, 11), Slot: "local", Sandbox: "local",
		Attempt: 1, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	task, err := c.Task(ctx, 11)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != StateRunning {
		t.Errorf("state = %q, want running", task.State)
	}
}

func TestSetCapabilityOnTrackedTaskWritesTheStoreNotGitHub(t *testing.T) {
	ctx := context.Background()
	c, gh, store := testClientWithStore(t)
	seedTracked(t, ctx, gh, store, 12, "task", "body", nil)

	if err := c.SetCapability(ctx, 12, "self-debug", true); err != nil {
		t.Fatal(err)
	}
	task, err := c.Task(ctx, 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Capabilities) != 1 || task.Capabilities[0] != "self-debug" {
		t.Fatalf("capabilities = %v, want [self-debug]", task.Capabilities)
	}
	issue, _ := gh.GetIssue("acme", "tasks", 12)
	if issue.HasLabel("grain-self-debug") {
		t.Errorf("a tracked task's capability must not be written as a GitHub label")
	}

	if err := c.SetCapability(ctx, 12, "self-debug", false); err != nil {
		t.Fatal(err)
	}
	task, err = c.Task(ctx, 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Capabilities) != 0 {
		t.Fatalf("capabilities after detach = %v, want none", task.Capabilities)
	}
}

func TestCloseAndReopenTrackedTask(t *testing.T) {
	ctx := context.Background()
	c, gh, store := testClientWithStore(t)
	seedTracked(t, ctx, gh, store, 13, "task", "body", nil)

	if err := c.Close(ctx, 13); err != nil {
		t.Fatal(err)
	}
	task, err := c.Task(ctx, 13)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != StateClosed {
		t.Errorf("state = %q, want closed", task.State)
	}
	if task.GitHubState != "closed" {
		t.Errorf("githubState = %q, want closed", task.GitHubState)
	}
	issue, _ := gh.GetIssue("acme", "tasks", 13)
	if issue.State != "closed" {
		t.Errorf("the GitHub issue itself must still be closed too")
	}

	if err := c.Reopen(ctx, 13); err != nil {
		t.Fatal(err)
	}
	task, err = c.Task(ctx, 13)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != StateQueued {
		t.Errorf("state after reopen = %q, want queued", task.State)
	}
	if task.GitHubState != "open" {
		t.Errorf("githubState after reopen = %q, want open", task.GitHubState)
	}
}

func TestUpdateTrackedTaskWritesTheStoreAndMirrorsGitHub(t *testing.T) {
	ctx := context.Background()
	c, gh, store := testClientWithStore(t)
	seedTracked(t, ctx, gh, store, 14, "old title", "old body", nil)

	newTitle := "new title"
	newBase := "release"
	task, err := c.UpdateTask(ctx, 14, UpdateTaskRequest{Title: &newTitle, Base: &newBase})
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "new title" || task.Base != "release" {
		t.Fatalf("task = %+v, want updated title/base", task)
	}

	mt, err := store.GetTask(ctx, storeTaskID(testRepo, 14))
	if err != nil {
		t.Fatal(err)
	}
	if mt == nil || mt.Title != "new title" || mt.Base != "release" {
		t.Fatalf("store row = %+v, want updated title/base", mt)
	}

	issue, _ := gh.GetIssue("acme", "tasks", 14)
	if issue.Title != "new title" {
		t.Errorf("issue title = %q, want mirrored to new title", issue.Title)
	}
}

func TestListTasksPrefersTrackedTaskOverItsOwnIssue(t *testing.T) {
	ctx := context.Background()
	c, gh, store := testClientWithStore(t)
	seedTracked(t, ctx, gh, store, 15, "tracked", "body", nil)
	// The trigger label itself is gone by the time a task is tracked (see
	// seedTracked/pollIssue), so nothing about this untracked issue #16
	// should collide with #15 -- included here to prove ListTasks reads
	// both without erroring, not to exercise the dedup path itself
	// (already covered by TestListTasksMergesUntrackedLabelsAndDeduplicates).
	gh.seed(16, "still just an issue", "body", DefaultLabels().Trigger)

	tasks, err := c.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byNumber := map[int]Task{}
	for _, task := range tasks {
		byNumber[task.Number] = task
	}
	if byNumber[15].State != StateQueued {
		t.Errorf("tracked task state = %q, want queued", byNumber[15].State)
	}
	if byNumber[16].State != StateQueued {
		t.Errorf("untracked (trigger-labelled) task state = %q, want queued", byNumber[16].State)
	}
}

func TestNilStoreFallsBackToGitHubEntirely(t *testing.T) {
	ctx := context.Background()
	c, gh := testClient() // Store is nil.
	l := DefaultLabels()
	gh.seed(20, "task", "body", l.Trigger)

	task, err := c.Task(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != StateQueued {
		t.Errorf("state = %q, want queued", task.State)
	}

	if err := c.Close(ctx, 20); err != nil {
		t.Fatal(err)
	}
	issue, _ := gh.GetIssue("acme", "tasks", 20)
	if issue.State != "closed" {
		t.Errorf("Close with a nil store must still close the GitHub issue")
	}
}
