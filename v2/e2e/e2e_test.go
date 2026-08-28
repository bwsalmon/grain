package e2e

// bwsalmon/agents#233 asked for a test that files issues the way a user
// would and checks that they complete -- branches made, merged, and the
// issue's own state transitioning the way a human watching the tracker
// would expect. The three tests below are that, fixed rather than
// randomized (simulate_test.go is the randomized counterpart): one happy
// path from queued through running, completed and closed with a real
// branch pushed and a real merge landed; one that parks on a question
// and resumes; and one where a first attempt is denied by the proxy and
// a retry succeeds.

import (
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/loop"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// fileIssue puts a task the way a real user or automation filing an
// issue would -- Approval set only when the actor is human, exactly
// model.LandsQueued's own rule, matching model/simulate_test.go's
// fileTask helper.
func fileIssue(w *world, id string, actor model.Principal, target model.RepoRef, links ...model.Link) model.Task {
	w.t.Helper()
	reason := model.ReasonDirect
	if actor.Kind == model.PrincipalAutomation {
		reason = model.ReasonProposal
	}
	tk := model.Task{
		ID: id, Intent: model.IntentImplement, Title: "issue " + id,
		Origin:  model.Origin{Attribution: model.Attribution{Actor: actor}, Reason: reason},
		Binding: model.BindingDirective,
		Target:  &target,
		Links:   links,
	}
	if model.LandsQueued(tk.Origin) {
		tk.Approval = &model.Attribution{Actor: actor}
	}
	if err := w.store.PutTask(w.ctx, tk); err != nil {
		w.t.Fatalf("filing %s: %v", id, err)
	}
	return tk
}

// assertState checks a task's state two ways at once, the same
// discipline model/simulate_test.go's checkModelInvariants runs every
// round: the SQL view (store.State) and the pure Go derivation
// (model.StateOf) must agree, given active exactly as the caller knows
// it -- true right after a dispatch, false once FinishRun has been
// called for it -- since nothing exposed by Store answers "does task X
// have a live run" directly without restating that bookkeeping here.
func assertState(w *world, id string, want model.State, active bool) {
	w.t.Helper()
	got, err := w.store.State(w.ctx, id)
	if err != nil {
		w.t.Fatalf("State(%s): %v", id, err)
	}
	if got != want {
		w.t.Fatalf("%s state = %q, want %q", id, got, want)
	}

	task, err := w.store.GetTask(w.ctx, id)
	if err != nil || task == nil {
		w.t.Fatalf("GetTask(%s): %v (nil=%v)", id, err, task == nil)
	}
	obs, err := w.store.GetObservation(w.ctx, id)
	if err != nil {
		w.t.Fatalf("GetObservation(%s): %v", id, err)
	}
	if derived := model.StateOf(*task, obs, active); derived != want {
		w.t.Fatalf("%s: model.StateOf = %q, disagrees with the view's %q", id, derived, want)
	}
}

func TestIssueCompletesEndToEnd(t *testing.T) {
	const slot = "sandbox-bd453be9-1"
	w := newWorld(t, []string{slot})
	w.newRepo("acme", "widgets")

	clock := baseTime
	fileIssue(w, "iss-1", human("alice"), model.RepoRef{Owner: "acme", Name: "widgets"})
	assertState(w, "iss-1", model.StateQueued, false)

	dispatches, err := loop.Cycle(w.ctx, w.store, []string{slot}, clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatches) != 1 || dispatches[0].TaskID != "iss-1" {
		t.Fatalf("Cycle dispatched %+v, want exactly iss-1", dispatches)
	}
	assertState(w, "iss-1", model.StateRunning, true)

	branch := model.BranchName("iss-1")
	clock = clock.Add(time.Minute)
	result := w.runDispatch(dispatches[0], pushScript(w.remote("acme", "widgets"), branch, "iss-1"), clock)
	if !pushedOK(result) {
		t.Fatalf("agent run did not push cleanly: %+v", result.ToolCalls)
	}

	// FinishRun alone is a requeue -- v2 has no completion detector of
	// its own yet (v2/README.md), so nothing marks this done until the
	// GitHub-sync stand-in below observes it.
	assertState(w, "iss-1", model.StateQueued, false)
	if occ, _ := w.store.OccupiedSlots(w.ctx); len(occ) != 0 {
		t.Fatalf("occupied slots after finish = %v, want none", occ)
	}

	if got := w.log1("acme", "widgets", branch, "%s"); got != "agent commit for iss-1" {
		t.Fatalf("pushed branch tip = %q, want the agent's commit", got)
	}

	clock = clock.Add(time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "iss-1", CompletedAt: &clock}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "iss-1", model.StateCompleted, false)

	// GitHub itself merges the PR -- no code in v2 does this yet (see
	// harness_test.go's package doc), so the test plays GitHub's part.
	w.mergeBranchIntoDefault("acme", "widgets", branch, "main")
	if got := w.log1("acme", "widgets", "main", "%s"); got != "Merge "+branch {
		t.Fatalf("main tip after merge = %q, want the merge commit", got)
	}

	// GitHub's own "Closes #N" convention closes the linked issue the
	// moment the PR merges; grain's sync observes that the same way it
	// observed completion.
	clock = clock.Add(time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "iss-1", CompletedAt: &clock, ClosedAt: &clock}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "iss-1", model.StateClosed, false)
}

func TestAgentQuestionParksTaskThenReplyResumesAndItCompletes(t *testing.T) {
	const slot = "sandbox-bd453be9-2"
	w := newWorld(t, []string{slot})
	w.newRepo("acme", "gadgets")

	clock := baseTime
	fileIssue(w, "iss-2", human("bob"), model.RepoRef{Owner: "acme", Name: "gadgets"})

	first, err := loop.Cycle(w.ctx, w.store, []string{slot}, clock)
	if err != nil || len(first) != 1 {
		t.Fatalf("first Cycle: %v, %+v", err, first)
	}

	clock = clock.Add(time.Minute)
	result := w.runDispatch(first[0], askScript("Should this touch the public API or stay internal?"), clock)
	question, asked := askedQuestion(result)
	if !asked || question == "" {
		t.Fatalf("expected an ask_question call, got tool calls: %+v", result.ToolCalls)
	}

	commentID := int64(555)
	clock = clock.Add(time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{
		TaskID: "iss-2", PendingQuestionCommentID: &commentID, ObservedAt: &clock,
	}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "iss-2", model.StateAwaitingReply, false)

	// Parked means not dispatchable, even with a free slot sitting right
	// there.
	stillParked, err := loop.Cycle(w.ctx, w.store, []string{slot}, clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(stillParked) != 0 {
		t.Fatalf("Cycle dispatched an awaiting-reply task: %+v", stillParked)
	}

	// A human replies. Observe REPLACEs the whole row
	// (model/simulate_test.go's TestGitHubSyncObservationsReplaceTheWholeRowNotJustTheChangedField
	// proves this), so an Observe naming nothing but the task id is what
	// clears the pending flag and hands the task back to the queue.
	clock = clock.Add(time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "iss-2", ObservedAt: &clock}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "iss-2", model.StateQueued, false)

	second, err := loop.Cycle(w.ctx, w.store, []string{slot}, clock)
	if err != nil || len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("second Cycle: %v, %+v, want attempt 2", err, second)
	}

	branch := model.BranchName("iss-2")
	clock = clock.Add(time.Minute)
	result2 := w.runDispatch(second[0], pushScript(w.remote("acme", "gadgets"), branch, "iss-2"), clock)
	if !pushedOK(result2) {
		t.Fatalf("retry did not push cleanly: %+v", result2.ToolCalls)
	}

	clock = clock.Add(time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "iss-2", CompletedAt: &clock}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "iss-2", model.StateCompleted, false)

	if n, err := w.store.Attempts(w.ctx, "iss-2"); err != nil || n != 2 {
		t.Fatalf("Attempts(iss-2) = %d (%v), want 2 (parked, then retried)", n, err)
	}
	if got := w.log1("acme", "gadgets", branch, "%s"); got != "agent commit for iss-2" {
		t.Fatalf("pushed branch tip = %q, want the agent's commit", got)
	}
}

func TestFailedRunReturnsTaskToQueueForRetry(t *testing.T) {
	const slot = "sandbox-bd453be9-3"
	w := newWorld(t, []string{slot})
	w.newRepo("acme", "widgets")
	w.newRepo("acme", "other")

	clock := baseTime
	fileIssue(w, "iss-3", human("carol"), model.RepoRef{Owner: "acme", Name: "widgets"})
	branch := model.BranchName("iss-3")

	first, err := loop.Cycle(w.ctx, w.store, []string{slot}, clock)
	if err != nil || len(first) != 1 {
		t.Fatalf("first Cycle: %v, %+v", err, first)
	}

	// The agent's first attempt mistakenly targets a repo the task never
	// named -- the same denial gitproxy/live_test.go proves in isolation,
	// here reached through the whole stack: loop dispatched it, the
	// script ran, and the push still never lands anywhere.
	clock = clock.Add(time.Minute)
	badResult := w.runDispatch(first[0], pushScript(w.remote("acme", "other"), branch, "iss-3"), clock)
	if pushedOK(badResult) {
		t.Fatalf("expected the out-of-scope push to fail, got: %+v", badResult.ToolCalls)
	}
	if w.branchExists("acme", "widgets", branch) || w.branchExists("acme", "other", branch) {
		t.Fatalf("a denied push must land nowhere")
	}
	assertState(w, "iss-3", model.StateQueued, false)

	second, err := loop.Cycle(w.ctx, w.store, []string{slot}, clock)
	if err != nil || len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("retry Cycle: %v, %+v, want attempt 2", err, second)
	}

	clock = clock.Add(time.Minute)
	goodResult := w.runDispatch(second[0], pushScript(w.remote("acme", "widgets"), branch, "iss-3"), clock)
	if !pushedOK(goodResult) {
		t.Fatalf("retry against the right repo still failed: %+v", goodResult.ToolCalls)
	}

	clock = clock.Add(time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "iss-3", CompletedAt: &clock}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "iss-3", model.StateCompleted, false)

	if n, err := w.store.Attempts(w.ctx, "iss-3"); err != nil || n != 2 {
		t.Fatalf("Attempts(iss-3) = %d (%v), want 2", n, err)
	}
	if got := w.log1("acme", "widgets", branch, "%s"); got != "agent commit for iss-3" {
		t.Fatalf("pushed branch tip = %q, want the agent's commit", got)
	}
}
