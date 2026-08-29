package orchestrator_test

import (
	"context"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// filedTask puts a queued task in the store, the way the CLI or the UI
// would -- these tests are about what ProcessResult does with a run's own
// tool calls. There is no issue to seed alongside it any more: a task is
// a row, and githubsim.Sim answers only for the target repo's pull
// requests.
func filedTask(t *testing.T, ctx context.Context, store *model.Store, id string, repo model.RepoRef) model.Task {
	t.Helper()
	human := model.Principal{Kind: model.PrincipalHuman, ID: "alice"}
	task := model.Task{
		ID: id, Intent: model.IntentImplement, Title: "fix the thing", Body: "please",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human}, Reason: model.ReasonDirect},
		Approval: &model.Attribution{Actor: human},
		Target:   &repo,
		Binding:  model.BindingDirective,
	}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("filing task: %v", err)
	}
	return task
}

// commentBodies is what a task's conversation now says, in order -- where
// these tests used to read client.ListComments off a GitHub issue.
func commentBodies(t *testing.T, ctx context.Context, store *model.Store, taskID string) []string {
	t.Helper()
	comments, err := store.Comments(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(comments))
	for _, c := range comments {
		out = append(out, c.Body)
	}
	return out
}

func toolResult(calls ...agent.ToolCall) *agent.Result {
	return &agent.Result{ToolCalls: calls}
}

func TestProcessResultOpensAPullRequestForAPushedBranch(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
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
	task := filedTask(t, ctx, store, "t1", repo)
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
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

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

	if got := commentBodies(t, ctx, store, task.ID); len(got) != 1 || got[0] != "which config file?" {
		t.Fatalf("conversation = %q, want the relayed question", got)
	}

	// The question grain is waiting on is named by id, and the relay is
	// attributed as grain speaking for the agent rather than as its own.
	comments, err := store.Comments(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := store.GetObservation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs.PendingQuestionCommentID == nil || *obs.PendingQuestionCommentID != comments[0].ID {
		t.Fatalf("pending question = %+v, want the stored comment's id %d", obs.PendingQuestionCommentID, comments[0].ID)
	}
	if comments[0].Author.Actor.Kind != model.PrincipalAutomation || comments[0].Author.OnBehalfOf == nil {
		t.Fatalf("relayed question attribution = %+v, want automation on behalf of the agent", comments[0].Author)
	}
}

func TestProcessResultRelaysAClosingCommentWithNoPush(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

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
	if got := commentBodies(t, ctx, store, task.ID); len(got) != 1 || got[0] != "the answer is 4" {
		t.Fatalf("conversation = %q, want the relayed closing comment", got)
	}
}

func TestProcessResultFilesAProposedTaskIntoTheStore(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

	result := toolResult(agent.ToolCall{
		Name: "propose_task",
		Arguments: map[string]any{
			"title": "follow-up work", "body": "do more of this",
		},
	})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var proposal *model.Task
	for i := range tasks {
		if tasks[i].ID != task.ID {
			proposal = &tasks[i]
		}
	}
	if proposal == nil {
		t.Fatal("the proposed task was never filed")
	}
	if proposal.Title != "follow-up work" {
		t.Fatalf("title = %q, want the proposed one", proposal.Title)
	}

	// Unapproved is the whole contract: proposeTaskTool's own doc comment
	// says a human must accept it before the agent set ever attempts it,
	// which the state machine enforces now rather than a withheld label.
	state, err := store.State(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateProposed {
		t.Fatalf("state = %q, want proposed", state)
	}
	ready, err := store.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ready {
		if id == proposal.ID {
			t.Fatal("a proposed task is dispatchable")
		}
	}
	// And it records which task proposed it, which the issue version had
	// no way to say.
	if len(proposal.Links) != 1 || proposal.Links[0].Kind != model.LinkProposedBy ||
		proposal.Links[0].Target != task.ID {
		t.Fatalf("links = %+v, want proposed-by the task that asked", proposal.Links)
	}
}

// TestProcessResultProposedTaskInheritsAutoMergeFromItsParent covers
// bwsalmon/agents#345: a task proposed by an auto-merge job should
// itself be an auto-merge job, since propose_task's own input schema has
// no way to request a capability the parent lacked -- see
// model.GrantsSubsetOf.
func TestProcessResultProposedTaskInheritsAutoMergeFromItsParent(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.AutoMerge = true
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("marking t1 auto-merge: %v", err)
	}

	result := toolResult(agent.ToolCall{
		Name: "propose_task",
		Arguments: map[string]any{
			"title": "follow-up work", "body": "do more of this",
		},
	})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var proposal *model.Task
	for i := range tasks {
		if tasks[i].ID != task.ID {
			proposal = &tasks[i]
		}
	}
	if proposal == nil {
		t.Fatal("the proposed task was never filed")
	}
	if !proposal.AutoMerge {
		t.Error("AutoMerge = false, want true -- an auto-merge job's own proposal should inherit it")
	}

	// Inheriting AutoMerge is not auto-approval: the proposal still needs
	// a human before it ever runs.
	state, err := store.State(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateProposed {
		t.Fatalf("state = %q, want proposed -- inheriting AutoMerge must not skip approval", state)
	}
}

// TestProcessResultProposedTaskDoesNotInheritAutoMergeFromANonAutoMergeParent
// is TestProcessResultFilesAProposedTaskIntoTheStore's own default case,
// made explicit: a task proposed by a job that was not itself an
// auto-merge job stays out of the merge queue.
func TestProcessResultProposedTaskDoesNotInheritAutoMergeFromANonAutoMergeParent(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

	result := toolResult(agent.ToolCall{
		Name: "propose_task",
		Arguments: map[string]any{
			"title": "follow-up work", "body": "do more of this",
		},
	})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var proposal *model.Task
	for i := range tasks {
		if tasks[i].ID != task.ID {
			proposal = &tasks[i]
		}
	}
	if proposal == nil {
		t.Fatal("the proposed task was never filed")
	}
	if proposal.AutoMerge {
		t.Error("AutoMerge = true, want false -- nothing here opted this proposal into the merge queue")
	}
}
