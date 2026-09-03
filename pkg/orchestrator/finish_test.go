package orchestrator_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
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
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
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
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
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
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
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

// TestProcessResultRelaysBothACommentAndAQuestion covers a run that
// called comment_on_issue and then ask_question. ProcessResult used to
// return on the question before it ever looked for a comment, so the
// comment was dropped with no trace anywhere: agent.Result is not
// persisted, and this path never reaches the "nothing to act on" logging
// that would at least have named the call. Both are the run's own words
// and both belong in the conversation -- the comment first, and the
// question is still the one the task parks on.
func TestProcessResultRelaysBothACommentAndAQuestion(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

	result := toolResult(
		agent.ToolCall{
			Name: "comment_on_issue", Arguments: map[string]any{"comment": "both tools record fine"},
		},
		agent.ToolCall{
			Name: "ask_question", Arguments: map[string]any{"question": "which config file?"},
		},
	)
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	got := commentBodies(t, ctx, store, task.ID)
	want := []string{"both tools record fine", "which config file?"}
	if len(got) != len(want) {
		t.Fatalf("conversation = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("conversation = %q, want %q", got, want)
		}
	}

	// The question is what the task waits on, not the comment that
	// preceded it, and asking parks the task rather than completing it.
	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateAwaitingReply {
		t.Fatalf("state = %q, want awaiting_reply", st)
	}
	comments, err := store.Comments(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := store.GetObservation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs.PendingQuestionCommentID == nil || *obs.PendingQuestionCommentID != comments[1].ID {
		t.Fatalf("pending question = %+v, want the question's id %d", obs.PendingQuestionCommentID, comments[1].ID)
	}
}

// TestProcessResultRelaysACommentAlongsideAPushedBranch is the same drop
// on the other ending: a pushed branch returned as soon as its pull
// request was opened, past the comment the same run had left. That
// combination is one comment_on_issue's own description invites -- "if
// you do push commits, a pull request is opened for them regardless of
// whether you also call this" -- so the remark explaining the push has to
// survive it.
func TestProcessResultRelaysACommentAlongsideAPushedBranch(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	result := toolResult(
		agent.ToolCall{Name: "run_command", Text: "pushed"},
		agent.ToolCall{
			Name: "comment_on_issue", Arguments: map[string]any{"comment": "fixed it; here is why"},
		},
	)
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	if len(sim.PullRequests) != 1 {
		t.Fatalf("expected one pull request, got %+v", sim.PullRequests)
	}
	if got := commentBodies(t, ctx, store, task.ID); len(got) != 1 || got[0] != "fixed it; here is why" {
		t.Fatalf("conversation = %q, want the relayed comment", got)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state = %q, want completed", st)
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
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
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

// TestProcessResultCorrectsARunThatProducedNothingToActOn is
// bwsalmon/agents#403: RunDispatch's own outcomeOf calls a run
// "succeeded" the moment the agent makes one harmless tool call, with no
// way to know yet whether that call amounted to a push, a question, or a
// closing comment. A run_command call that touches neither git nor either
// escape hatch is exactly that gap -- ProcessResult must correct
// task_run's own outcome once it has actually ruled all three out, or a
// run that never did anything useful would keep reading "succeeded"
// forever and never count toward FailureStreak's own cap.
func TestProcessResultCorrectsARunThatProducedNothingToActOn(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

	runID := "t1-1"
	if err := store.StartRun(ctx, model.Run{
		ID: runID, TaskID: task.ID, Sandbox: "s1", Attempt: 1, StartedAt: baseTime,
	}, model.Limits{}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := store.FinishRun(ctx, runID, baseTime, "succeeded", ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	result := toolResult(agent.ToolCall{Name: "run_command", Text: "ran something, pushed nothing"})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, runID, baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	streak, err := store.FailureStreak(ctx, task.ID)
	if err != nil {
		t.Fatalf("FailureStreak: %v", err)
	}
	if streak == nil || streak.Count != 1 {
		t.Fatalf("failure streak = %+v, want a single counted failure", streak)
	}
	if streak.LastOutcome != "no_action" {
		t.Fatalf("last outcome = %q, want no_action", streak.LastOutcome)
	}
	if streak.LastDetail == "" {
		t.Fatal("last detail is empty, want an explanation a human could read")
	}

	// Still queued, not completed and not failed outright -- one
	// unproductive run is not the cap.
	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateQueued {
		t.Fatalf("state = %q, want queued", st)
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
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
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
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
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
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
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

// proposalsByTitle collects every task in the store except the one that
// proposed them, keyed by title: ListTasks' own order is the queue's, not
// the order a run made its propose_task calls in, and a title is the only
// thing about a proposal a test knows before it is filed.
func proposalsByTitle(t *testing.T, ctx context.Context, store *model.Store, proposer string) map[string]model.Task {
	t.Helper()
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]model.Task{}
	for _, task := range tasks {
		if task.ID != proposer {
			out[task.Title] = task
		}
	}
	return out
}

// dependsOn is every task id a proposal is blocked on, in link order.
func dependsOn(task model.Task) []string {
	var out []string
	for _, l := range task.Links {
		if l.Kind == model.LinkDependsOn {
			out = append(out, l.Target)
		}
	}
	return out
}

// TestProcessResultResolvesProposedTaskDependencies covers the etiquette
// propose_task's own description asks an agent for: a proposal that has
// to follow other work says so in depends_on, and lands blocked on it
// rather than dispatchable the moment a human approves it.
//
// Both spellings a run has resolve here -- an existing task id (its own,
// the usual case for a piece split out of the work in hand) and the local
// `id` of an earlier propose_task call in the same run -- and a duplicate
// collapses rather than filing the same link twice.
func TestProcessResultResolvesProposedTaskDependencies(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

	result := toolResult(
		agent.ToolCall{
			Name: "propose_task",
			Arguments: map[string]any{
				"id":         "spec",
				"title":      "write the spec",
				"body":       "the shape first",
				"depends_on": []any{"t1"},
			},
		},
		// "#t1" is the v1 issue-number spelling of the same task the
		// entry after it names, and naming it twice is one dependency.
		agent.ToolCall{
			Name: "propose_task",
			Arguments: map[string]any{
				"title":      "build it",
				"body":       "then the thing itself",
				"depends_on": []any{"spec", "#t1", "t1"},
			},
		},
	)
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	proposals := proposalsByTitle(t, ctx, store, task.ID)
	spec, ok := proposals["write the spec"]
	if !ok {
		t.Fatalf("the first proposal was never filed: %v", proposals)
	}
	build, ok := proposals["build it"]
	if !ok {
		t.Fatalf("the second proposal was never filed: %v", proposals)
	}

	if got := dependsOn(spec); len(got) != 1 || got[0] != task.ID {
		t.Errorf("first proposal depends on %v, want just the task that proposed it (%s)", got, task.ID)
	}
	// Two, not three: "#t1" and "t1" are the same dependency. Links come
	// back ordered by target rather than in the order the run named them
	// (Store.hydrate), so this checks membership.
	got := dependsOn(build)
	if len(got) != 2 || !slices.Contains(got, spec.ID) || !slices.Contains(got, task.ID) {
		t.Errorf("second proposal depends on %v, want the first proposal (%s) and %s, once each",
			got, spec.ID, task.ID)
	}

	// A resolved dependency is a real block, not a note: neither
	// proposal is dispatchable while the task that proposed them is
	// still open, approval or no approval.
	if !model.IsBlocked(build, map[string]bool{}) {
		t.Error("the second proposal is not blocked by anything")
	}
}

// An entry that names nothing grain can find is kept where a human will
// see it rather than dropped: a proposal filed silently unblocked is one
// an approver has no reason to look twice at.
func TestProcessResultKeepsAnUnresolvableDependencyInTheProposalBody(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

	result := toolResult(agent.ToolCall{
		Name: "propose_task",
		Arguments: map[string]any{
			"title":      "follow-up work",
			"body":       "do more of this",
			"depends_on": []any{"no-such-task"},
		},
	})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	proposal, ok := proposalsByTitle(t, ctx, store, task.ID)["follow-up work"]
	if !ok {
		t.Fatal("the proposed task was never filed")
	}
	if deps := dependsOn(proposal); len(deps) != 0 {
		t.Errorf("depends on %v, want nothing -- a link to a task that does not exist never unblocks", deps)
	}
	if !strings.Contains(proposal.Body, "no-such-task") {
		t.Errorf("body = %q, want the unresolved dependency named in it", proposal.Body)
	}
}

// TestProcessResultProposedTaskCanDeclineInheritedAutoMerge is the other
// half of bwsalmon/agents#345's inheritance: a run that judges a proposal
// to be separate work rather than a piece of its own task says so with
// auto_merge, and that proposal gets a human's review even though the job
// that proposed it did not need one.
func TestProcessResultProposedTaskCanDeclineInheritedAutoMerge(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.AutoMerge = true
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("marking t1 auto-merge: %v", err)
	}

	result := toolResult(
		agent.ToolCall{
			Name: "propose_task",
			Arguments: map[string]any{
				"title":      "the rest of this task",
				"body":       "a piece of the same work",
				"auto_merge": true,
			},
		},
		agent.ToolCall{
			Name: "propose_task",
			Arguments: map[string]any{
				"title":      "something else entirely",
				"body":       "separate work",
				"auto_merge": false,
			},
		},
	)
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	proposals := proposalsByTitle(t, ctx, store, task.ID)
	piece, ok := proposals["the rest of this task"]
	if !ok {
		t.Fatalf("the first proposal was never filed: %v", proposals)
	}
	separate, ok := proposals["something else entirely"]
	if !ok {
		t.Fatalf("the second proposal was never filed: %v", proposals)
	}
	if !piece.AutoMerge {
		t.Error("AutoMerge = false on a piece of an auto-merge task that asked for it")
	}
	if separate.AutoMerge {
		t.Error("AutoMerge = true on a proposal that asked not to be one")
	}
}

// auto_merge cannot raise a proposal above the job that made it: a run
// whose own task a human left reviewable cannot hand its successor the
// unreviewed merge it was denied.
func TestProcessResultProposedTaskCannotGrantItselfAutoMerge(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

	result := toolResult(agent.ToolCall{
		Name: "propose_task",
		Arguments: map[string]any{
			"title": "follow-up work", "body": "do more of this", "auto_merge": true,
		},
	})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "t1-1", baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	proposal, ok := proposalsByTitle(t, ctx, store, task.ID)["follow-up work"]
	if !ok {
		t.Fatal("the proposed task was never filed")
	}
	if proposal.AutoMerge {
		t.Error("AutoMerge = true, want false -- a run cannot grant its proposal what its own task lacked")
	}
}

// A run that pushed a branch and then failed must still get its pull
// request opened. This is the failure that produced grain/task-1 on
// bwsalmon/grain: the agent edited, committed and pushed, then spent the
// rest of its turn budget and the framework returned an error. RunCycle
// treated the error as "nothing happened", skipped ProcessResult
// entirely, recreated the sandbox, and left a real branch on GitHub with
// no pull request, no comment, and nothing pointing at it -- while the
// task went back to the queue to do the same work again.
func TestRunCycleOpensAPullRequestForABranchAFailedRunAlreadyPushed(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

	// The shape of a real max-turns ending: the work is on GitHub, and
	// the framework reports the failure with what it managed to do first
	// (see agent.Framework's own contract).
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))
	ranOutOfTurns := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return &agent.Result{ToolCalls: []agent.ToolCall{
			{Name: "run_command", Text: "pushed"},
			{Name: "run_command", Text: "ok"},
		}}, errors.New("gemini: exceeded max turns (2) without a final answer")
	})

	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		Framework:     orchestrator.StaticFramework(ranOutOfTurns),
		MaxWorkers: 1,
	}

	err := orchestrator.RunCycle(ctx, deps, baseTime)
	if err == nil {
		t.Fatal("expected RunCycle to report the framework's failure")
	}
	if !strings.Contains(err.Error(), "exceeded max turns") {
		t.Errorf("error = %v, want the framework's own diagnosis preserved", err)
	}

	if len(sim.PullRequests) != 1 {
		t.Fatalf("pull requests = %+v, want one for the branch the run pushed before it failed", sim.PullRequests)
	}
	if sim.PullRequests[0].Head != model.BranchName(task.ID) {
		t.Errorf("pull request = %+v, want head %s", sim.PullRequests[0], model.BranchName(task.ID))
	}

	// The run itself stays failed with the real reason: salvaging the
	// branch does not turn a run that ran out of turns into a success,
	// and the detail now names what it spent them on.
	runs, err := store.Runs(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want one", runs)
	}
	if runs[0].Outcome != "failed" {
		t.Errorf("outcome = %q, want failed", runs[0].Outcome)
	}
	if !strings.Contains(runs[0].Detail, "exceeded max turns") ||
		!strings.Contains(runs[0].Detail, "2 tool call(s)") {
		t.Errorf("detail = %q, want both the failure and what the run did first", runs[0].Detail)
	}
}
