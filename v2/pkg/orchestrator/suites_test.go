package orchestrator_test

// SyncTaskSuites' own tests (bwsalmon/agents#642). Like qualifications_
// test.go, nothing here talks to (simulated) GitHub: a pull request
// having been opened is simulated directly as the LinkFixes link
// finishWithPullRequest itself would have written, and a follow-up task
// having been proposed is simulated as relayProposedTasks itself would
// have written, so what is under test stays SyncTaskSuites' own
// pass-to-pass decision, not the machinery that would normally produce
// those links.

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

func suiteSmokeTemplate(repo model.RepoRef) model.TaskTemplate {
	return model.TaskTemplate{
		ID: "template-suite-smoke", Name: "Smoke test", Title: "Smoke test", Body: "run the smoke suite",
		Target: repo, CreatedAt: baseTime,
	}
}

func openPullRequest(t *testing.T, ctx context.Context, store *model.Store, taskID string) {
	t.Helper()
	if err := store.UpdateTask(ctx, taskID, func(task *model.Task) error {
		task.Links = append(task.Links, model.Link{Kind: model.LinkFixes, Target: "acme/widgets#1"})
		return nil
	}); err != nil {
		t.Fatalf("recording a pull request for %s: %v", taskID, err)
	}
}

func completeTask(t *testing.T, ctx context.Context, store *model.Store, taskID string, at time.Time) {
	t.Helper()
	if err := store.Observe(ctx, model.Observation{TaskID: taskID, CompletedAt: &at}); err != nil {
		t.Fatalf("completing %s: %v", taskID, err)
	}
}

// onlyTask returns run's own single pass-1 task id -- every test below
// uses a one-item suite, so a pass is always exactly one task.
func onlyTask(t *testing.T, run model.TaskSuiteRun, pass int) string {
	t.Helper()
	tasks := run.PassTasks(pass)
	if len(tasks) != 1 {
		t.Fatalf("pass %d has %d tasks, want 1", pass, len(tasks))
	}
	return tasks[0].TaskID
}

func TestSyncTaskSuitesDoesNothingWhileThePassIsStillPending(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	if err := store.PutTaskTemplate(ctx, suiteSmokeTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	suite := model.TaskSuite{
		Name: "smoke", Items: []model.TaskSuiteItem{{TemplateID: "template-suite-smoke"}},
		Mode: model.TaskSuiteCount, Count: 2, AutoMerge: true,
	}
	run, err := store.CreateTaskSuiteRun(ctx, suite, repo, "main", baseTime)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := orchestrator.SyncTaskSuites(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncTaskSuites: %v", err)
	}
	updated, err := store.GetTaskSuiteRun(ctx, run.ID)
	if err != nil || updated == nil {
		t.Fatalf("run: (%+v, %v)", updated, err)
	}
	if updated.Status != model.TaskSuiteRunActive {
		t.Fatalf("got status %q while pass 1 is still queued, want active", updated.Status)
	}
	if len(updated.Tasks) != 1 {
		t.Fatalf("got %d tasks while pass 1 is still pending, want exactly the 1 already filed", len(updated.Tasks))
	}
}

func TestSyncTaskSuitesInCountModeFiresEveryPassThenSucceeds(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	if err := store.PutTaskTemplate(ctx, suiteSmokeTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	suite := model.TaskSuite{
		Name: "smoke", Items: []model.TaskSuiteItem{{TemplateID: "template-suite-smoke"}},
		Mode: model.TaskSuiteCount, Count: 2, AutoMerge: true,
	}
	run, err := store.CreateTaskSuiteRun(ctx, suite, repo, "main", baseTime)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Pass 1 completes with no change at all -- TaskSuiteCount fires the
	// second pass anyway, unlike TaskSuiteUntilClean.
	completeTask(t, ctx, store, onlyTask(t, run, 1), baseTime)
	if err := orchestrator.SyncTaskSuites(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncTaskSuites after pass 1: %v", err)
	}
	updated, err := store.GetTaskSuiteRun(ctx, run.ID)
	if err != nil || updated == nil {
		t.Fatalf("run: (%+v, %v)", updated, err)
	}
	run = *updated
	if run.Status != model.TaskSuiteRunActive {
		t.Fatalf("got status %q after pass 1 of 2, want still active", run.Status)
	}
	if run.CurrentPass() != 2 {
		t.Fatalf("got current pass %d after pass 1 completed, want 2", run.CurrentPass())
	}

	completeTask(t, ctx, store, onlyTask(t, run, 2), baseTime)
	if err := orchestrator.SyncTaskSuites(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncTaskSuites after pass 2: %v", err)
	}
	updated, err = store.GetTaskSuiteRun(ctx, run.ID)
	if err != nil || updated == nil {
		t.Fatalf("run: (%+v, %v)", updated, err)
	}
	run = *updated
	if run.Status != model.TaskSuiteRunSucceeded {
		t.Fatalf("got status %q after both passes completed, want succeeded", run.Status)
	}
	if run.CompletedAt == nil {
		t.Fatal("want CompletedAt set once succeeded")
	}
}

func TestSyncTaskSuitesInUntilCleanModeStopsAtTheFirstCleanPass(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	if err := store.PutTaskTemplate(ctx, suiteSmokeTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	suite := model.TaskSuite{
		Name: "bugfinder", Items: []model.TaskSuiteItem{{TemplateID: "template-suite-smoke"}},
		Mode: model.TaskSuiteUntilClean, MaxPasses: 5, AutoMerge: true,
	}
	run, err := store.CreateTaskSuiteRun(ctx, suite, repo, "main", baseTime)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Pass 1 opens a pull request -- a real change, so the run must not
	// stop yet.
	first := onlyTask(t, run, 1)
	openPullRequest(t, ctx, store, first)
	completeTask(t, ctx, store, first, baseTime)
	if err := orchestrator.SyncTaskSuites(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncTaskSuites after pass 1: %v", err)
	}
	updated, err := store.GetTaskSuiteRun(ctx, run.ID)
	if err != nil || updated == nil {
		t.Fatalf("run: (%+v, %v)", updated, err)
	}
	run = *updated
	if run.Status != model.TaskSuiteRunActive {
		t.Fatalf("got status %q after a pass that opened a pull request, want still active", run.Status)
	}
	if run.CurrentPass() != 2 {
		t.Fatalf("got current pass %d, want 2", run.CurrentPass())
	}

	// Pass 2 completes clean -- no pull request, no proposed follow-up --
	// so the run stops here, succeeded, without ever reaching MaxPasses.
	completeTask(t, ctx, store, onlyTask(t, run, 2), baseTime)
	if err := orchestrator.SyncTaskSuites(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncTaskSuites after pass 2: %v", err)
	}
	updated, err = store.GetTaskSuiteRun(ctx, run.ID)
	if err != nil || updated == nil {
		t.Fatalf("run: (%+v, %v)", updated, err)
	}
	run = *updated
	if run.Status != model.TaskSuiteRunSucceeded {
		t.Fatalf("got status %q after a clean pass, want succeeded", run.Status)
	}
	if run.CurrentPass() != 2 {
		t.Fatalf("got current pass %d, want exactly 2 (no third pass fired)", run.CurrentPass())
	}
}

func TestSyncTaskSuitesInUntilCleanModeFailsAfterMaxPassesWithNoCleanPass(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	if err := store.PutTaskTemplate(ctx, suiteSmokeTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	suite := model.TaskSuite{
		Name: "bugfinder", Items: []model.TaskSuiteItem{{TemplateID: "template-suite-smoke"}},
		Mode: model.TaskSuiteUntilClean, MaxPasses: 2, AutoMerge: true,
	}
	run, err := store.CreateTaskSuiteRun(ctx, suite, repo, "main", baseTime)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	for pass := 1; pass <= 2; pass++ {
		task := onlyTask(t, run, pass)
		openPullRequest(t, ctx, store, task)
		completeTask(t, ctx, store, task, baseTime)
		if err := orchestrator.SyncTaskSuites(ctx, store, baseTime); err != nil {
			t.Fatalf("SyncTaskSuites after pass %d: %v", pass, err)
		}
		run2, err := store.GetTaskSuiteRun(ctx, run.ID)
		if err != nil || run2 == nil {
			t.Fatalf("run: (%+v, %v)", run2, err)
		}
		run = *run2
	}

	if run.Status != model.TaskSuiteRunFailed {
		t.Fatalf("got status %q after MaxPasses with no clean pass, want failed", run.Status)
	}
	if run.LastError == "" {
		t.Fatal("want a LastError explaining why the run failed")
	}
	if run.CurrentPass() != 2 {
		t.Fatalf("got current pass %d, want exactly MaxPasses (2), no further pass fired", run.CurrentPass())
	}
}

func TestSyncTaskSuitesFailsTheRunWhenATaskClosesWithoutCompleting(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	if err := store.PutTaskTemplate(ctx, suiteSmokeTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	suite := model.TaskSuite{
		Name: "smoke", Items: []model.TaskSuiteItem{{TemplateID: "template-suite-smoke"}},
		Mode: model.TaskSuiteCount, Count: 3, AutoMerge: true,
	}
	run, err := store.CreateTaskSuiteRun(ctx, suite, repo, "main", baseTime)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	closedAt := baseTime
	if err := store.Observe(ctx, model.Observation{TaskID: onlyTask(t, run, 1), ClosedAt: &closedAt}); err != nil {
		t.Fatalf("observe closed: %v", err)
	}
	if err := orchestrator.SyncTaskSuites(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncTaskSuites: %v", err)
	}
	updated, err := store.GetTaskSuiteRun(ctx, run.ID)
	if err != nil || updated == nil {
		t.Fatalf("run: (%+v, %v)", updated, err)
	}
	if updated.Status != model.TaskSuiteRunFailed {
		t.Fatalf("got status %q after a task closed without completing, want failed", updated.Status)
	}
	if updated.CurrentPass() != 1 {
		t.Fatalf("got current pass %d, want 1 (no further pass fired after a failure)", updated.CurrentPass())
	}
}

func TestSyncTaskSuitesLeavesARequireApprovalRunActiveUntilApproved(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	if err := store.PutTaskTemplate(ctx, suiteSmokeTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	suite := model.TaskSuite{
		Name: "smoke", Items: []model.TaskSuiteItem{{TemplateID: "template-suite-smoke"}},
		Mode: model.TaskSuiteCount, Count: 1, RequireApproval: true,
	}
	run, err := store.CreateTaskSuiteRun(ctx, suite, repo, "main", baseTime)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.Tasks[0].Approved {
		t.Fatal("a RequireApproval suite's tasks must land unapproved")
	}

	if err := orchestrator.SyncTaskSuites(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncTaskSuites before approval: %v", err)
	}
	updated, err := store.GetTaskSuiteRun(ctx, run.ID)
	if err != nil || updated == nil {
		t.Fatalf("run: (%+v, %v)", updated, err)
	}
	if updated.Status != model.TaskSuiteRunActive {
		t.Fatalf("got status %q before approval, want still active", updated.Status)
	}

	if err := store.Approve(ctx, run.Tasks[0].TaskID, model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "grace"}}, baseTime); err != nil {
		t.Fatalf("approve: %v", err)
	}
	completeTask(t, ctx, store, run.Tasks[0].TaskID, baseTime)
	if err := orchestrator.SyncTaskSuites(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncTaskSuites after approval and completion: %v", err)
	}
	updated, err = store.GetTaskSuiteRun(ctx, run.ID)
	if err != nil || updated == nil {
		t.Fatalf("run: (%+v, %v)", updated, err)
	}
	if updated.Status != model.TaskSuiteRunSucceeded {
		t.Fatalf("got status %q after approval and completion, want succeeded", updated.Status)
	}
}
