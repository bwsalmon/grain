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

	"github.com/bwsalmon/grain/v2/pkg/dispatch"
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

	dispatches, err := dispatch.Cycle(w.ctx, w.store, []string{slot}, clock)
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

	// FinishRun alone is a requeue -- this harness builds no github.Client
	// at all (see harness_test.go's own doc comment on why), so nothing
	// marks this done until the GitHub-sync stand-in below observes it the
	// way pkg/orchestrator/finish.go's ProcessResult does for real,
	// against a real github.Client, in pkg/orchestrator/live_test.go.
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

	// GitHub itself merges the PR -- this harness has no real GitHub to
	// merge it on (see harness_test.go's package doc), so the test plays
	// GitHub's part; pkg/orchestrator/live_test.go proves the real
	// close-out (pkg/orchestrator/sync.go's SyncPullRequests) against an
	// actual githubsim-backed merge instead.
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

	first, err := dispatch.Cycle(w.ctx, w.store, []string{slot}, clock)
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
	stillParked, err := dispatch.Cycle(w.ctx, w.store, []string{slot}, clock)
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

	second, err := dispatch.Cycle(w.ctx, w.store, []string{slot}, clock)
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

// TestParentBlockedUntilChildrenClose exercises model.LinkChildOf per
// docs/data-model.md's "A parent is not complete while a child is open"
// rule: "an open child is an unresolved blocking link" -- meaning the
// *parent* holds one ChildOf link per child (mirroring how a blocked
// task holds DependsOn links, per dispatch_test.go's
// TestCycleSkipsBlockedTasksUntilTheirDependencyCloses), not the other
// way around. LinkKind.Blocks (task.go) and IsBlocked (state.go) block
// whoever holds the link, keyed on whether the link's target has closed,
// so a child's own ChildOf link back at its parent would block the
// child on the parent instead -- the opposite of what "a parent is not
// complete while a child is open" needs, and briefly a deadlock if both
// directions were linked at once.
//
// Blocked is deliberately not a State (task.go's docstring: "a blocked
// task is still queued"), and nothing in dispatch or store.Observe
// refuses to mark a blocked task completed or closed outright -- see
// model/state.go's StateOf and IsBlocked, and pkg/model/store.go's
// Ready, none of which cross-check each other. The one real, observable
// effect blocking has today is dispatch eligibility: store.Ready's
// underlying task_ready view (schema.go) excludes a task with any open
// blocking link, so dispatch.Cycle never selects it, however many free
// slots there are, until every blocker closes. That is what this test
// asserts -- not that the parent literally "cannot be closed" (nothing
// stops that directly), but that it can never reach closed through the
// normal dispatch/push/complete/merge/close pipeline while blocked,
// because that pipeline starts with a dispatch it never gets.
func TestParentBlockedUntilChildrenClose(t *testing.T) {
	// A third slot, kept unused until the parent's own dispatch: pushScript
	// clones into a fixed "work" directory under its slot's sandbox root,
	// so reusing slotA or slotB for the parent after a child already
	// cloned there would collide with that leftover directory.
	const slotA, slotB, slotC = "sandbox-bd453be9-4a", "sandbox-bd453be9-4b", "sandbox-bd453be9-4c"
	w := newWorld(t, []string{slotA, slotB, slotC})
	w.newRepo("acme", "widgets")
	w.newRepo("acme", "other")
	widgets := model.RepoRef{Owner: "acme", Name: "widgets"}
	other := model.RepoRef{Owner: "acme", Name: "other"}

	clock := baseTime
	// Children first: task_blocked's join against task_link.target only
	// counts a link once its target row exists, so filing them before the
	// parent is what makes the parent's ChildOf links actually block.
	fileIssue(w, "kid-a", human("dana"), widgets)
	fileIssue(w, "kid-b", human("dana"), other)
	fileIssue(w, "par", human("dana"), widgets,
		model.Link{Kind: model.LinkChildOf, Target: "kid-a"},
		model.Link{Kind: model.LinkChildOf, Target: "kid-b"},
	)
	assertState(w, "par", model.StateQueued, false)

	dispatches, err := dispatch.Cycle(w.ctx, w.store, []string{slotA, slotB}, clock)
	if err != nil {
		t.Fatal(err)
	}
	byTask := map[string]dispatch.Dispatch{}
	for _, d := range dispatches {
		byTask[d.TaskID] = d
	}
	if len(dispatches) != 2 || byTask["kid-a"].TaskID == "" || byTask["kid-b"].TaskID == "" {
		t.Fatalf("Cycle dispatched %+v, want exactly kid-a and kid-b, parent still blocked", dispatches)
	}

	// Drive both children through a real dispatch/push/complete cycle,
	// each against its own repo so their merges below cannot collide on
	// the same file.
	for _, spec := range []struct{ id, owner, name string }{
		{"kid-a", "acme", "widgets"},
		{"kid-b", "acme", "other"},
	} {
		d := byTask[spec.id]
		branch := model.BranchName(spec.id)
		clock = clock.Add(time.Minute)
		result := w.runDispatch(d, pushScript(w.remote(spec.owner, spec.name), branch, spec.id), clock)
		if !pushedOK(result) {
			t.Fatalf("%s: agent run did not push cleanly: %+v", spec.id, result.ToolCalls)
		}
		clock = clock.Add(time.Minute)
		if err := w.store.Observe(w.ctx, model.Observation{TaskID: spec.id, CompletedAt: &clock}); err != nil {
			t.Fatal(err)
		}
		assertState(w, spec.id, model.StateCompleted, false)
	}

	// Completed is not closed -- task_blocked reads closed_at, not
	// completed_at, so the parent must still be excluded from every
	// cycle with both children merely completed.
	stillBlocked, err := dispatch.Cycle(w.ctx, w.store, []string{slotA, slotB}, clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(stillBlocked) != 0 {
		t.Fatalf("Cycle dispatched a still-blocked parent: %+v", stillBlocked)
	}

	// GitHub merges kid-a's PR and its "Closes #N" convention closes it --
	// one of the two blockers gone, one still open.
	w.mergeBranchIntoDefault("acme", "widgets", model.BranchName("kid-a"), "main")
	clock = clock.Add(time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "kid-a", CompletedAt: &clock, ClosedAt: &clock}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "kid-a", model.StateClosed, false)

	oneOpen, err := dispatch.Cycle(w.ctx, w.store, []string{slotA, slotB}, clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(oneOpen) != 0 {
		t.Fatalf("Cycle dispatched the parent with kid-b still open: %+v", oneOpen)
	}

	// Close kid-b, the last blocker.
	w.mergeBranchIntoDefault("acme", "other", model.BranchName("kid-b"), "main")
	clock = clock.Add(time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "kid-b", CompletedAt: &clock, ClosedAt: &clock}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "kid-b", model.StateClosed, false)

	// Both children closed -- the parent is unblocked and dispatches on
	// the very next cycle, with nothing re-applied to it. Only slotC is
	// offered here: slotA and slotB each already hold a child's cloned
	// "work" directory from pushScript above, and the parent's own push
	// below needs a clean slot to clone into.
	freed, err := dispatch.Cycle(w.ctx, w.store, []string{slotC}, clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 1 || freed[0].TaskID != "par" {
		t.Fatalf("Cycle after both children closed = %+v, want exactly par", freed)
	}
	assertState(w, "par", model.StateRunning, true)

	// Carry the parent itself through the same real push/complete/merge/
	// close cycle iss-1 gets in TestIssueCompletesEndToEnd, so the test
	// proves not just that the parent becomes eligible but that it can
	// actually reach closed once it does.
	branch := model.BranchName("par")
	clock = clock.Add(time.Minute)
	result := w.runDispatch(freed[0], pushScript(w.remote("acme", "widgets"), branch, "par"), clock)
	if !pushedOK(result) {
		t.Fatalf("parent's agent run did not push cleanly: %+v", result.ToolCalls)
	}
	clock = clock.Add(time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "par", CompletedAt: &clock}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "par", model.StateCompleted, false)

	w.mergeBranchIntoDefault("acme", "widgets", branch, "main")
	clock = clock.Add(time.Minute)
	if err := w.store.Observe(w.ctx, model.Observation{TaskID: "par", CompletedAt: &clock, ClosedAt: &clock}); err != nil {
		t.Fatal(err)
	}
	assertState(w, "par", model.StateClosed, false)
}

func TestFailedRunReturnsTaskToQueueForRetry(t *testing.T) {
	const slot = "sandbox-bd453be9-3"
	w := newWorld(t, []string{slot})
	w.newRepo("acme", "widgets")
	w.newRepo("acme", "other")

	clock := baseTime
	fileIssue(w, "iss-3", human("carol"), model.RepoRef{Owner: "acme", Name: "widgets"})
	branch := model.BranchName("iss-3")

	first, err := dispatch.Cycle(w.ctx, w.store, []string{slot}, clock)
	if err != nil || len(first) != 1 {
		t.Fatalf("first Cycle: %v, %+v", err, first)
	}

	// The agent's first attempt mistakenly targets a repo the task never
	// named -- the same denial gitproxy/live_test.go proves in isolation,
	// here reached through the whole stack: dispatch chose it, the
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

	second, err := dispatch.Cycle(w.ctx, w.store, []string{slot}, clock)
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
