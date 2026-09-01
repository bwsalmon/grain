// Four more of bwsalmon/agents#337's ten, all extending
// mergequeue_conflict_test.go's own real-git merge queue coverage past
// the one scenario it proves (a conflict a single automatic fix
// resolves):
//
//   - TestMergeQueueEscalatesAfterAFailedFixAndAdvancesToTheNextQueuedTask
//     is sync.go's other exit from advanceMergeQueueHead --
//     escalateToUser -- which pkg/orchestrator/mergequeue_test.go already
//     proves against a hand-built store/sim state, but which nothing had
//     driven through a real dispatch, a real failed fix and a real second
//     queued task actually taking the freed-up head position before this.
//   - TestMergeQueueFilesAndResolvesAFixForFailingCheckRunsNotJustConflicts
//     is the same fix-and-resolve happy path mergequeue_conflict_test.go
//     already proves, but reached through a failing CI check
//     (healthFrom's ListCheckRuns half) rather than a git conflict
//     (healthFrom's Mergeable half) -- the only e2e coverage of that half
//     before this was auto_merge_test.go's own single already-passing
//     check.
//   - TestClosingATaskWithAnOpenPullRequestDropsItFromTheMergeQueueForGood
//     is a safety property nothing exercised before: a task a human
//     closed while its pull request was still open must never be
//     auto-merged later, however clean that pull request eventually
//     reads.
//   - TestAutoMergeLeavesAPullRequestOfUnknownMergeabilityAloneUntilIt
//     Resolves guards the third of healthFrom's outcomes, PrUnknown: a
//     pull request GitHub has not finished computing mergeability for
//     must neither merge early nor be mistaken for a conflict.
//
// All four reuse mergequeue_conflict_test.go's own worldSandboxes,
// setMergeable and realConflict, and cli_test.go's own scriptedFramework,
// rather than rebuilding any of them.
package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/github/githubsim"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// cleanPushScript is pushScript's own clone/commit/push, but prefixed
// with `rm -rf work` -- harness_test.go's own pushScript never cleans up
// after itself, which every existing caller gets away with because it
// only ever pushes once per slot per test. The tests in this file
// dispatch more than one task into the very same slot in a row, so a
// second dispatch's `git clone ... work` would otherwise fail outright
// against the first dispatch's own leftover "work" directory -- the same
// reason configPushScript and resolveScript (mergequeue_conflict_test.go)
// already clean up first.
func cleanPushScript(remote, branch, taskID string) []*genai.GenerateContentResponse {
	cmd := "rm -rf work && git clone " + remote + " work && cd work && " +
		"git checkout -b " + branch + " && " +
		"echo 'change for " + taskID + "' >> NOTES.md && " +
		"git add NOTES.md && git commit -q -m 'agent commit for " + taskID + "' && " +
		"git push origin " + branch
	return []*genai.GenerateContentResponse{
		toolCall("run_command", map[string]any{"command": cmd}),
		finalText("pushed " + branch),
	}
}

// fixPushScript is a fix task's own scripted turn that builds fixBranch
// on top of baseBranch (fileFixTask's own contract: a fix task's Base is
// the branch it repairs) and pushes a trivial, unrelated file -- for
// tests where the fix's own content does not matter, only that it merges
// back into baseBranch cleanly. Building atop origin/baseBranch, rather
// than cloning and branching from whatever HEAD happens to be, is what
// keeps this from conflicting with whatever the task it repairs already
// pushed to the same file names.
func fixPushScript(remote, baseBranch, fixBranch string) []*genai.GenerateContentResponse {
	cmd := "rm -rf work && git clone " + remote + " work && cd work && " +
		"git checkout -b " + baseBranch + " origin/" + baseBranch + " && " +
		"git checkout -b " + fixBranch + " && " +
		"echo 'fix' > FIX.md && " +
		"git add FIX.md && git commit -q -m 'fix for " + fixBranch + "' && " +
		"git push origin " + fixBranch
	return []*genai.GenerateContentResponse{
		toolCall("run_command", map[string]any{"command": cmd}),
		finalText("pushed a fix to " + fixBranch),
	}
}

// fixDoesNotResolveScript is a fix task's own scripted turn that pushes
// something -- but never merges main into itself, unlike resolveScript's
// own real conflict resolution -- so whatever it repairs stays broken.
// Built off baseBranch (the task it repairs' own branch, per
// fileFixTask's own doc comment on why a fix task's Base is that branch
// rather than main), the same as resolveScript, so its own pull request
// is trivially mergeable back into it.
func fixDoesNotResolveScript(remote, baseBranch, fixBranch string) []*genai.GenerateContentResponse {
	cmd := "rm -rf work && git clone " + remote + " work && cd work && " +
		"git checkout -b " + baseBranch + " origin/" + baseBranch + " && " +
		"git checkout -b " + fixBranch + " && " +
		"echo 'attempted a fix' > ATTEMPT.md && " +
		"git add ATTEMPT.md && git commit -q -m 'attempted a fix, does not touch the real conflict' && " +
		"git push origin " + fixBranch
	return []*genai.GenerateContentResponse{
		toolCall("run_command", map[string]any{"command": cmd}),
		finalText("pushed an attempted fix to " + fixBranch),
	}
}

func TestMergeQueueEscalatesAfterAFailedFixAndAdvancesToTheNextQueuedTask(t *testing.T) {
	const slot = "sandbox-337-mq-1"
	w := newWorld(t, []string{slot})
	const owner, repoName = "acme", "widgets"
	w.newRepo(owner, repoName)
	repo := model.RepoRef{Owner: owner, Name: repoName}

	bare := filepath.Join(w.upstreamDir, owner, repoName+".git")
	sim := githubsim.New(owner, repoName, bare, "main")
	client := github.NewClient(sim, nil)
	deps := orchestrator.Deps{Store: w.store, Client: client, Sandboxes: worldSandboxes{w}, MaxConcurrent: 1}

	task1 := model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "task1",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human("alice")}, Reason: model.ReasonDirect},
		Approval: &model.Attribution{Actor: human("alice")},
		Target:   &repo, Binding: model.BindingDirective, AutoMerge: true, CreatedAt: &baseTime,
	}
	if err := w.store.PutTask(w.ctx, task1); err != nil {
		t.Fatal(err)
	}
	laterCreated := baseTime.Add(time.Second)
	task2 := model.Task{
		ID: "t2", Intent: model.IntentImplement, Title: "task2",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human("bob")}, Reason: model.ReasonDirect},
		Approval: &model.Attribution{Actor: human("bob")},
		Target:   &repo, Binding: model.BindingDirective, AutoMerge: true, CreatedAt: &laterCreated,
	}
	if err := w.store.PutTask(w.ctx, task2); err != nil {
		t.Fatal(err)
	}

	branch1, branch2 := model.BranchName("t1"), model.BranchName("t2")
	clock := baseTime

	// Step 1: both push and open pull requests -- task1 first, so it is
	// the earlier-created (and thus head) entry once both are queue
	// members.
	deps.Framework = scriptedFramework(configPushScript(w.remote(owner, repoName), branch1, "setting: from-task1"))
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (t1 push): %v", err)
	}
	if st, err := w.store.State(w.ctx, "t1"); err != nil || st != model.StateCompleted {
		t.Fatalf("t1 state = %q (%v), want completed", st, err)
	}

	clock = clock.Add(time.Minute)
	deps.Framework = scriptedFramework(cleanPushScript(w.remote(owner, repoName), branch2, "t2"))
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (t2 push): %v", err)
	}
	if st, err := w.store.State(w.ctx, "t2"); err != nil || st != model.StateCompleted {
		t.Fatalf("t2 state = %q (%v), want completed", st, err)
	}
	if len(sim.PullRequests) != 2 {
		t.Fatalf("expected two pull requests, got %+v", sim.PullRequests)
	}

	// Step 2: an unrelated commit lands straight on main and genuinely
	// conflicts with task1's branch only -- task2's own change touches an
	// unrelated file.
	w.pushConflictingCommit(owner, repoName, "main", "CONFIG.md", "setting: from-main", "unrelated change conflicting with t1")
	if !w.realConflict(owner, repoName, "main", branch1) {
		t.Fatal("expected a genuine git conflict between main and task1's branch")
	}
	setMergeable(sim, branch1, false)
	setMergeable(sim, branch2, true)

	// Step 3: sync notices task1 -- the queue's head -- is broken and
	// files a fix. task2 is clean but not yet head, so it must not merge.
	clock = clock.Add(time.Minute)
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (files a fix): %v", err)
	}
	got1, err := w.store.GetTask(w.ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	fixTaskID, hasFix := "", false
	for _, l := range got1.Links {
		if l.Kind == model.LinkFixTask {
			fixTaskID, hasFix = l.Target, true
		}
	}
	if !hasFix {
		t.Fatalf("expected a LinkFixTask on t1, got %+v", got1.Links)
	}
	if st, err := w.store.State(w.ctx, "t2"); err != nil || st != model.StateCompleted {
		t.Fatalf("t2 state = %q (%v), want still completed (not yet its turn)", st, err)
	}
	if sim.PullRequests[1].State != "open" {
		t.Fatalf("t2's own pull request must still be open, got %+v", sim.PullRequests[1])
	}

	// Step 4: the fix is dispatched, but its own attempted fix never
	// actually resolves the real conflict with main.
	fixBranch := model.BranchName(fixTaskID)
	clock = clock.Add(time.Minute)
	deps.Framework = scriptedFramework(fixDoesNotResolveScript(w.remote(owner, repoName), branch1, fixBranch))
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (dispatch the fix): %v", err)
	}
	if st, err := w.store.State(w.ctx, fixTaskID); err != nil || st != model.StateCompleted {
		t.Fatalf("fix task state = %q (%v), want completed", st, err)
	}

	// Step 5: the fix's own pull request -- trivially mergeable into
	// task1's branch, since it never touched main -- auto-merges for
	// real, and the fix task closes.
	setMergeable(sim, fixBranch, true)
	clock = clock.Add(time.Minute)
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (fix's own PR merges): %v", err)
	}
	if st, err := w.store.State(w.ctx, fixTaskID); err != nil || st != model.StateClosed {
		t.Fatalf("fix task state = %q (%v), want closed", st, err)
	}
	if !w.realConflict(owner, repoName, "main", branch1) {
		t.Fatal("expected task1's branch to still genuinely conflict with main -- the fix never addressed it")
	}
	setMergeable(sim, branch1, false)

	// Step 6: further cycles let the merge queue notice the fix didn't
	// take, escalate task1 to a human, and move on to merge task2.
	// Exactly which single cycle notices which is not a contract this
	// test pins down: advanceMergeQueueHead's read of the fix task's own
	// state, and queueHeads' read of task1's blocked flag, are each a
	// snapshot taken once at the start of a cycle, so both can settle in
	// the same cycle or across two depending on what order task_link's
	// rows happen to sort in -- what matters is that a few ticks later,
	// both have settled correctly.
	for i := 0; i < 3; i++ {
		clock = clock.Add(time.Minute)
		if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
			t.Fatalf("RunCycle (settling, tick %d): %v", i, err)
		}
	}

	if st, err := w.store.State(w.ctx, "t1"); err != nil || st != model.StateCompleted {
		t.Fatalf("t1 state = %q (%v), want completed -- still open, waiting on a human, never merged or closed", st, err)
	}
	obs1, err := w.store.GetObservation(w.ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if obs1 == nil || obs1.MergeQueueBlockedAt == nil {
		t.Fatal("expected t1 to be marked blocked on the merge queue after its one fix attempt failed")
	}
	comments1, err := w.store.Comments(w.ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	foundEscalation := false
	for _, c := range comments1 {
		if strings.Contains(c.Body, "needs a person") {
			foundEscalation = true
		}
	}
	if !foundEscalation {
		t.Fatalf("expected an escalation comment on t1, got %+v", comments1)
	}

	// The queue moved on: with t1 excluded (blocked), t2 became head and
	// merged for real.
	if st, err := w.store.State(w.ctx, "t2"); err != nil || st != model.StateClosed {
		t.Fatalf("t2 state = %q (%v), want closed -- the queue should have moved on and merged it once t1 was excluded", st, err)
	}
	if got := w.fileAt(owner, repoName, "main", "NOTES.md"); !strings.Contains(got, "change for t2") {
		t.Fatalf("main:NOTES.md = %q, want t2's own change actually merged", got)
	}
}

func TestMergeQueueFilesAndResolvesAFixForFailingCheckRunsNotJustConflicts(t *testing.T) {
	const slot = "sandbox-337-mq-2"
	w := newWorld(t, []string{slot})
	const owner, repoName = "acme", "checks"
	w.newRepo(owner, repoName)
	repo := model.RepoRef{Owner: owner, Name: repoName}

	bare := filepath.Join(w.upstreamDir, owner, repoName+".git")
	sim := githubsim.New(owner, repoName, bare, "main")
	client := github.NewClient(sim, nil)
	deps := orchestrator.Deps{Store: w.store, Client: client, Sandboxes: worldSandboxes{w}, MaxConcurrent: 1}

	task1 := model.Task{
		ID: "t1", Intent: model.IntentImplement, Title: "task1",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human("alice")}, Reason: model.ReasonDirect},
		Approval: &model.Attribution{Actor: human("alice")},
		Target:   &repo, Binding: model.BindingDirective, AutoMerge: true, CreatedAt: &baseTime,
	}
	if err := w.store.PutTask(w.ctx, task1); err != nil {
		t.Fatal(err)
	}

	branch1 := model.BranchName("t1")
	clock := baseTime
	deps.Framework = scriptedFramework(cleanPushScript(w.remote(owner, repoName), branch1, "t1"))
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (push): %v", err)
	}
	if st, err := w.store.State(w.ctx, "t1"); err != nil || st != model.StateCompleted {
		t.Fatalf("t1 state = %q (%v), want completed", st, err)
	}

	// Genuinely mergeable at the git level throughout -- what breaks this
	// pull request is a failing CI check, not a conflict.
	setMergeable(sim, branch1, true)
	failure := "failure"
	sim.CheckRuns[branch1] = []github.CheckRun{{Name: "ci", Status: "completed", Conclusion: &failure}}

	clock = clock.Add(time.Minute)
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (files a fix for failing checks): %v", err)
	}
	got1, err := w.store.GetTask(w.ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	fixTaskID, hasFix := "", false
	for _, l := range got1.Links {
		if l.Kind == model.LinkFixTask {
			fixTaskID, hasFix = l.Target, true
		}
	}
	if !hasFix {
		t.Fatalf("expected a LinkFixTask on t1 after a failing check, got %+v", got1.Links)
	}
	comments1, err := w.store.Comments(w.ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	foundReason := false
	for _, c := range comments1 {
		if strings.Contains(c.Body, "checks are failing") {
			foundReason = true
		}
	}
	if !foundReason {
		t.Fatalf("expected the fix-filed comment to name failing checks, got %+v", comments1)
	}

	// The fix is dispatched and pushes -- content does not matter here,
	// since this test's own sim.CheckRuns is what stands in for CI.
	fixBranch := model.BranchName(fixTaskID)
	clock = clock.Add(time.Minute)
	deps.Framework = scriptedFramework(fixPushScript(w.remote(owner, repoName), branch1, fixBranch))
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (dispatch the fix): %v", err)
	}
	if st, err := w.store.State(w.ctx, fixTaskID); err != nil || st != model.StateCompleted {
		t.Fatalf("fix task state = %q (%v), want completed", st, err)
	}

	// CI re-runs on the fix and passes, and -- since merging it is what
	// lands its content on task1's own branch -- CI re-running on task1's
	// branch itself goes green in the same moment: both are set before
	// the cycle that merges the fix, not after, so nothing here ever
	// reads task1 as still broken once its fix has actually landed.
	setMergeable(sim, fixBranch, true)
	delete(sim.CheckRuns, branch1)
	clock = clock.Add(time.Minute)
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (fix's own PR merges, then t1 itself): %v", err)
	}
	if st, err := w.store.State(w.ctx, fixTaskID); err != nil || st != model.StateClosed {
		t.Fatalf("fix task state = %q (%v), want closed", st, err)
	}

	// A further tick in case the fix's own close and task1's own re-read
	// landed on either side of this cycle's single pass over the queue.
	clock = clock.Add(time.Minute)
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (settling): %v", err)
	}
	if st, err := w.store.State(w.ctx, "t1"); err != nil || st != model.StateClosed {
		t.Fatalf("t1 state = %q (%v), want closed", st, err)
	}
	obs1, err := w.store.GetObservation(w.ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if obs1 != nil && obs1.MergeQueueBlockedAt != nil {
		t.Fatal("t1 should never have been escalated -- the automatic fix genuinely resolved it before any cycle ever read it as still broken")
	}
}

func TestClosingATaskWithAnOpenPullRequestDropsItFromTheMergeQueueForGood(t *testing.T) {
	const slot = "sandbox-337-mq-3"
	w := newWorld(t, []string{slot})
	const owner, repoName = "acme", "declined"
	w.newRepo(owner, repoName)
	repo := model.RepoRef{Owner: owner, Name: repoName}

	bare := filepath.Join(w.upstreamDir, owner, repoName+".git")
	sim := githubsim.New(owner, repoName, bare, "main")
	client := github.NewClient(sim, nil)

	task := model.Task{
		ID: "t-closed-early", Intent: model.IntentImplement, Title: "closed before merge",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human("gina")}, Reason: model.ReasonDirect},
		Approval: &model.Attribution{Actor: human("gina")},
		Target:   &repo, Binding: model.BindingDirective, AutoMerge: true, CreatedAt: &baseTime,
	}
	if err := w.store.PutTask(w.ctx, task); err != nil {
		t.Fatal(err)
	}

	branch := model.BranchName("t-closed-early")
	deps := orchestrator.Deps{
		Store: w.store, Client: client, Sandboxes: worldSandboxes{w}, MaxConcurrent: 1,
		Framework: scriptedFramework(pushScript(w.remote(owner, repoName), branch, "t-closed-early")),
	}
	clock := baseTime
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (push): %v", err)
	}
	assertState(w, "t-closed-early", model.StateCompleted, false)
	if len(sim.PullRequests) != 1 {
		t.Fatalf("expected one pull request, got %+v", sim.PullRequests)
	}

	// A human closes the task before anyone merges its pull request --
	// the same effect ui.Client.Close (grain close) has on Observation.
	// ClosedAt, applied directly here since this test is about
	// SyncPullRequests' own reaction, not the CLI verb itself
	// (cli_verbs_test.go covers that).
	clock = clock.Add(time.Minute)
	if err := w.store.ObserveField(w.ctx, "t-closed-early", clock, func(o *model.Observation) { o.ClosedAt = &clock }); err != nil {
		t.Fatal(err)
	}
	assertState(w, "t-closed-early", model.StateClosed, false)

	// The pull request would merge cleanly if grain ever looked at it
	// again -- which is exactly the point: it must not.
	setMergeable(sim, branch, true)

	for i := 0; i < 3; i++ {
		clock = clock.Add(time.Minute)
		if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
			t.Fatalf("RunCycle (tick %d after close): %v", i, err)
		}
		assertState(w, "t-closed-early", model.StateClosed, false)
		if sim.PullRequests[0].State != "open" {
			t.Fatalf("tick %d: pull request state = %q, want still open -- a closed task must never auto-merge", i, sim.PullRequests[0].State)
		}
	}
	if w.branchContains(owner, repoName, "main", branch) {
		t.Fatal("main must never have absorbed a task that was closed before its pull request merged")
	}
}

func TestAutoMergeLeavesAPullRequestOfUnknownMergeabilityAloneUntilItResolves(t *testing.T) {
	const slot = "sandbox-337-mq-4"
	w := newWorld(t, []string{slot})
	const owner, repoName = "acme", "pending"
	w.newRepo(owner, repoName)
	repo := model.RepoRef{Owner: owner, Name: repoName}

	bare := filepath.Join(w.upstreamDir, owner, repoName+".git")
	sim := githubsim.New(owner, repoName, bare, "main")
	client := github.NewClient(sim, nil)

	task := model.Task{
		ID: "t-pending", Intent: model.IntentImplement, Title: "still computing",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human("hank")}, Reason: model.ReasonDirect},
		Approval: &model.Attribution{Actor: human("hank")},
		Target:   &repo, Binding: model.BindingDirective, AutoMerge: true, CreatedAt: &baseTime,
	}
	if err := w.store.PutTask(w.ctx, task); err != nil {
		t.Fatal(err)
	}

	branch := model.BranchName("t-pending")
	deps := orchestrator.Deps{
		Store: w.store, Client: client, Sandboxes: worldSandboxes{w}, MaxConcurrent: 1,
		Framework: scriptedFramework(pushScript(w.remote(owner, repoName), branch, "t-pending")),
	}
	clock := baseTime
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (push): %v", err)
	}
	assertState(w, "t-pending", model.StateCompleted, false)
	if len(sim.PullRequests) != 1 {
		t.Fatalf("expected one pull request, got %+v", sim.PullRequests)
	}
	if sim.PullRequests[0].Mergeable != nil {
		t.Fatalf("expected a freshly opened pull request's mergeability to read unknown, got %+v", sim.PullRequests[0].Mergeable)
	}

	// Two more ticks with mergeability still unresolved: neither merges
	// prematurely nor files a spurious fix, unlike a real conflict or a
	// real failing check would.
	for i := 0; i < 2; i++ {
		clock = clock.Add(time.Minute)
		if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
			t.Fatalf("RunCycle (tick %d, still unknown): %v", i, err)
		}
		assertState(w, "t-pending", model.StateCompleted, false)
		if sim.PullRequests[0].State != "open" {
			t.Fatalf("tick %d: pull request state = %q, want still open", i, sim.PullRequests[0].State)
		}
	}
	got, err := w.store.GetTask(w.ctx, "t-pending")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range got.Links {
		if l.Kind == model.LinkFixTask {
			t.Fatalf("expected no fix task filed while mergeability was merely unknown, got link %+v", l)
		}
	}
	comments, err := w.store.Comments(w.ctx, "t-pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 0 {
		t.Fatalf("expected no merge-queue commentary while mergeability was merely unknown, got %+v", comments)
	}

	// GitHub finishes computing it: clean. Now it merges.
	setMergeable(sim, branch, true)
	clock = clock.Add(time.Minute)
	if err := orchestrator.RunCycle(w.ctx, deps, clock); err != nil {
		t.Fatalf("RunCycle (resolves clean): %v", err)
	}
	assertState(w, "t-pending", model.StateClosed, false)
	if sim.PullRequests[0].State != "closed" {
		t.Fatalf("pull request state = %q, want closed (merged)", sim.PullRequests[0].State)
	}
}
