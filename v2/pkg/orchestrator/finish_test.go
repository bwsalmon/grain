package orchestrator_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/github/githubsim"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// filedTask puts a queued task directly, standing in for what PollIssues
// would have produced -- these tests are about what ProcessResult does
// with a run's own tool calls, not about intake. It names the same repo
// as both the task repo (ExternalRef) and the target (Target): githubsim.
// Sim answers for exactly one repo (its own doc comment says so), and a
// single-repo deployment -- task and target repo the same -- is a real,
// common shape (docs/next-session.md: "a single-repo deployment ...
// behaves exactly as before"), so this is not a corner these tests invent
// just to dodge Sim's limits.
func filedTask(t *testing.T, ctx context.Context, store *model.Store, id string, repo model.RepoRef, issueNumber int) model.Task {
	t.Helper()
	human := model.Principal{Kind: model.PrincipalHuman, ID: "alice"}
	task := model.Task{
		ID: id, Intent: model.IntentImplement, Title: "fix the thing", Body: "please",
		Origin:      model.Origin{Attribution: model.Attribution{Actor: human}, Reason: model.ReasonDirect},
		Approval:    &model.Attribution{Actor: human},
		Target:      &repo,
		Binding:     model.BindingDirective,
		ExternalRef: fmt.Sprintf("%s#%d", repo, issueNumber),
	}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("filing task: %v", err)
	}
	return task
}

// seedIssue puts a blank issue in sim, standing in for the task-repo issue
// PollIssues would already have created before any run this package's own
// tests care about ever started.
func seedIssue(sim *githubsim.Sim, number int) {
	sim.Issues[number] = &githubsim.Issue{Title: "t", Body: "b", Author: "alice", Labels: map[string]struct{}{}}
}

func toolResult(calls ...agent.ToolCall) *agent.Result {
	return &agent.Result{ToolCalls: calls}
}

func TestProcessResultOpensAPullRequestForAPushedBranch(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedIssue(sim, 1)
	task := filedTask(t, ctx, store, "t1", repo, 1)
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	result := toolResult(agent.ToolCall{Name: "run_command", Text: "pushed"})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	if len(sim.PullRequests) != 1 {
		t.Fatalf("expected one pull request, got %+v", sim.PullRequests)
	}
	if sim.PullRequests[0].Head != model.BranchName(task.ID) || sim.PullRequests[0].Base != "main" {
		t.Fatalf("got %+v", sim.PullRequests[0])
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state = %q, want completed", st)
	}

	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantLink := model.PullRequestRef{Repo: *task.Target, Number: sim.PullRequests[0].Number}.String()
	found := false
	for _, l := range got.Links {
		if l.Kind == model.LinkFixes && l.Target == wantLink {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a LinkFixes to %s, got %+v", wantLink, got.Links)
	}
}

func TestProcessResultReusesAnAlreadyOpenPullRequest(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedIssue(sim, 1)
	task := filedTask(t, ctx, store, "t1", repo, 1)
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	existing, err := client.CreatePullRequest("acme", "widgets", model.BranchName(task.ID), "main", "existing", "")
	if err != nil {
		t.Fatal(err)
	}

	result := toolResult(agent.ToolCall{Name: "run_command", Text: "pushed"})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}
	if len(sim.PullRequests) != 1 {
		t.Fatalf("expected no second PR to be opened, got %+v", sim.PullRequests)
	}
	if sim.PullRequests[0].Number != existing.Number {
		t.Fatalf("got %+v, want the existing PR reused", sim.PullRequests[0])
	}
}

func TestProcessResultRelaysAQuestionAndParksTheTask(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedIssue(sim, 1)
	task := filedTask(t, ctx, store, "t1", repo, 1)

	result := toolResult(agent.ToolCall{
		Name: "ask_question", Arguments: map[string]any{"question": "which config file?"},
	})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateAwaitingReply {
		t.Fatalf("state = %q, want awaiting_reply", st)
	}

	comments, err := client.ListComments("acme", "widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "which config file?" {
		t.Fatalf("got %+v", comments)
	}
}

func TestProcessResultRelaysAClosingCommentWithNoPush(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedIssue(sim, 1)
	task := filedTask(t, ctx, store, "t1", repo, 1)

	result := toolResult(agent.ToolCall{
		Name: "comment_on_issue", Arguments: map[string]any{"comment": "the answer is 4"},
	})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state = %q, want completed", st)
	}
	comments, err := client.ListComments("acme", "widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "the answer is 4" {
		t.Fatalf("got %+v", comments)
	}
}

func TestProcessResultFilesAProposedTaskAsAnUnlabelledIssue(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedIssue(sim, 1)
	task := filedTask(t, ctx, store, "t1", repo, 1)

	result := toolResult(agent.ToolCall{
		Name: "propose_task",
		Arguments: map[string]any{
			"title": "follow-up work", "body": "do more of this",
		},
	})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	// Sim numbers a freshly filed issue at 8001 (its own New() starts
	// nextIssue at 8000 and pre-increments) -- this test's own repo has no
	// other issue filed through CreateIssue before this one.
	issue, err := client.GetIssue("acme", "widgets", 8001)
	if err != nil {
		t.Fatalf("expected the proposed task to have been filed: %v", err)
	}
	if issue.Title != "follow-up work" || issue.HasLabel("grain-agent") {
		t.Fatalf("got %+v", issue)
	}
}
