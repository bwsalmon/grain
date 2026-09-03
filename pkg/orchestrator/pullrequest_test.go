package orchestrator_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// linksTo counts the LinkFixes rows on taskID pointing at ref -- a count,
// not a bool, because "opened it twice and linked it twice" is exactly
// the failure these tests are here to rule out.
func linksTo(t *testing.T, ctx context.Context, store *model.Store, taskID, ref string) int {
	t.Helper()
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range task.Links {
		if l.Kind == model.LinkFixes && l.Target == ref {
			n++
		}
	}
	return n
}

// A run that opens its own pull request mid-flight gets a real one, and
// the task it belongs to is *not* ended by it: the run is still going,
// and a task marked completed here would be handed straight to
// SyncPullRequests' merge queue -- which could merge a branch the agent
// is still pushing to.
func TestOpenPullRequestForTaskOpensOneWithoutEndingTheTask(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	branch := model.BranchName(task.ID)
	pushBranch(t, sim.BareRepo, branch)

	status, err := orchestrator.OpenPullRequestForTask(ctx, store, client, task)
	if err != nil {
		t.Fatalf("OpenPullRequestForTask: %v", err)
	}
	if len(sim.PullRequests) != 1 {
		t.Fatalf("expected one pull request, got %+v", sim.PullRequests)
	}
	if sim.PullRequests[0].Head != branch || sim.PullRequests[0].Base != "main" {
		t.Fatalf("got %+v", sim.PullRequests[0])
	}
	if status.PullRequest.Number != sim.PullRequests[0].Number {
		t.Fatalf("status = %+v, want the pull request just opened", status)
	}

	ref := model.PullRequestRef{Repo: repo, Number: status.PullRequest.Number}.String()
	if n := linksTo(t, ctx, store, task.ID, ref); n != 1 {
		t.Fatalf("links to %s = %d, want 1", ref, n)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st == model.StateCompleted {
		t.Fatal("the task was completed by opening its pull request -- " +
			"the run is still live, and a completed task is a merge queue member")
	}
}

// The finish path adopts the pull request the run already opened rather
// than opening a second one for the same head (which GitHub refuses) or
// linking it twice -- and it is the finish, not the early open, that ends
// the task.
func TestOpenPullRequestForTaskIsAdoptedByTheFinishPath(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	first, err := orchestrator.OpenPullRequestForTask(ctx, store, client, task)
	if err != nil {
		t.Fatalf("OpenPullRequestForTask: %v", err)
	}
	// Called twice, the way an agent polling its own checks would.
	second, err := orchestrator.OpenPullRequestForTask(ctx, store, client, task)
	if err != nil {
		t.Fatalf("OpenPullRequestForTask again: %v", err)
	}
	if second.PullRequest.Number != first.PullRequest.Number {
		t.Fatalf("second call opened %d, want the same pull request as the first (%d)",
			second.PullRequest.Number, first.PullRequest.Number)
	}

	result := toolResult(agent.ToolCall{Name: "run_command", Text: "pushed"})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	if len(sim.PullRequests) != 1 {
		t.Fatalf("expected exactly one pull request across all three calls, got %+v", sim.PullRequests)
	}
	ref := model.PullRequestRef{Repo: repo, Number: first.PullRequest.Number}.String()
	if n := linksTo(t, ctx, store, task.ID, ref); n != 1 {
		t.Fatalf("links to %s = %d, want 1", ref, n)
	}
	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state = %q, want completed once the run itself finished", st)
	}
}

// An agent that asks before it pushes is told to push, rather than
// getting GitHub's own 422 about a head that does not exist -- and
// nothing is opened.
func TestOpenPullRequestForTaskRefusesAnUnpushedBranch(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

	_, err := orchestrator.OpenPullRequestForTask(ctx, store, client, task)
	if err == nil {
		t.Fatal("expected an error for a branch that was never pushed")
	}
	if !strings.Contains(err.Error(), model.BranchName(task.ID)) {
		t.Errorf("error = %q, want it to name the branch that is missing", err)
	}
	if len(sim.PullRequests) != 0 {
		t.Fatalf("expected no pull request, got %+v", sim.PullRequests)
	}
}

// The checks are the whole reason a run opens its pull request early:
// what comes back has to say what CI is doing right now, including a
// check that has not finished.
func TestOpenPullRequestForTaskReportsChecks(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	branch := model.BranchName(task.ID)
	pushBranch(t, sim.BareRepo, branch)
	sim.CheckRuns[branch] = []github.CheckRun{
		{Name: "lint", Status: "completed", Conclusion: strPtr("failure")},
		{Name: "tests", Status: "in_progress"},
	}

	status, err := orchestrator.OpenPullRequestForTask(ctx, store, client, task)
	if err != nil {
		t.Fatalf("OpenPullRequestForTask: %v", err)
	}
	if status.ChecksError != "" {
		t.Fatalf("ChecksError = %q, want none", status.ChecksError)
	}
	if !status.ChecksKnown {
		t.Fatal("ChecksKnown = false, want true -- this client can read check runs")
	}
	if len(status.Checks) != 2 {
		t.Fatalf("Checks = %+v, want the two seeded check runs", status.Checks)
	}
	if status.Checks[0].Name != "lint" || status.Checks[0].Conclusion == nil ||
		*status.Checks[0].Conclusion != "failure" {
		t.Errorf("Checks[0] = %+v, want lint/failure", status.Checks[0])
	}
	if status.Checks[1].Status != "in_progress" {
		t.Errorf("Checks[1] = %+v, want the still-running check reported as such", status.Checks[1])
	}
}

// A human closing the task while its run is still going is answered the
// same way the finish path answers it: the branch stays pushed and no
// pull request is opened for work nobody asked to keep.
func TestOpenPullRequestForTaskRefusesAClosedTask(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))
	closedAt := baseTime
	if err := store.ObserveField(ctx, task.ID, closedAt, func(o *model.Observation) {
		o.ClosedAt = &closedAt
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := orchestrator.OpenPullRequestForTask(ctx, store, client, task); err == nil {
		t.Fatal("expected an error for a task closed while its run was live")
	}
	if len(sim.PullRequests) != 0 {
		t.Fatalf("expected no pull request, got %+v", sim.PullRequests)
	}
}

// A task with no repo has no branch, so there is nothing to open -- and
// saying so beats a nil dereference on task.Target.
func TestOpenPullRequestForTaskRefusesATaskWithNoRepo(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	human := model.Principal{Kind: model.PrincipalHuman, ID: "alice"}
	task := model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "think about it", Body: "no repo",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human}, Reason: model.ReasonDirect},
		Approval: &model.Attribution{Actor: human},
		Binding:  model.BindingDirective,
	}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	if _, err := orchestrator.OpenPullRequestForTask(ctx, store, client, task); err == nil {
		t.Fatal("expected an error for a task with no repo attached")
	}
}
