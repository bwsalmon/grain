package orchestrator_test

import (
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/dispatch"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// TestRecoverOrphanedRunsReturnsATaskToQueued is bwsalmon/agents#425's own
// scenario: a task_run row with no `finished_at` -- what a crashed
// daemon leaves behind -- and no branch pushed for it. Recovery must
// finish that run and hand the task back to task_ready, exactly what
// dispatch.Cycle could never do on its own (dispatch_test.go's own
// TestCycleLeavesAnAlreadyRunningTaskAlone; crash_test.go's own
// TestRestartAfterACrashMidRunDoesNotDoubleDispatchOrLoseTheRun proves
// the same run is neither lost nor redispatched before recovery ever
// runs).
func TestRecoverOrphanedRunsReturnsATaskToQueued(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	task := filedTask(t, ctx, store, "t1", model.RepoRef{Owner: "acme", Name: "widgets"})

	d := dispatch.Dispatch{TaskID: task.ID, RunID: "t1-1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)

	if st, err := store.State(ctx, task.ID); err != nil || st != model.StateRunning {
		t.Fatalf("state before recovery = %q (%v), want running", st, err)
	}

	if err := orchestrator.RecoverOrphanedRuns(ctx, store, client, baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("RecoverOrphanedRuns: %v", err)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateQueued {
		t.Fatalf("state after recovery = %q, want queued: an orphaned run must free the task", st)
	}

	occupied, err := store.LiveRunCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if occupied != 0 {
		t.Fatalf("occupied slots after recovery = %v, want none: the orphaned run must be finished", occupied)
	}
}

// TestRecoverOrphanedRunsOpensAPullRequestForAnAlreadyPushedBranch covers
// the issue's own other half: a crashed run may have pushed its branch
// before dying, before ProcessResult ever ran to turn it into a pull
// request. Recovery must not strand that branch -- it mirrors
// finish_test.go's own TestProcessResultOpensAPullRequestForAPushedBranch,
// through RecoverOrphanedRuns instead of ProcessResult.
func TestRecoverOrphanedRunsOpensAPullRequestForAnAlreadyPushedBranch(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	task := filedTask(t, ctx, store, "t1", model.RepoRef{Owner: "acme", Name: "widgets"})
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	d := dispatch.Dispatch{TaskID: task.ID, RunID: "t1-1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)

	if err := orchestrator.RecoverOrphanedRuns(ctx, store, client, baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("RecoverOrphanedRuns: %v", err)
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
	if st != model.StateAwaitingSubmit {
		t.Fatalf("state = %q, want awaiting_submit: recovery opened its pull request and nobody has submitted it", st)
	}
}

// TestRecoverOrphanedRunsLeavesAClosedTasksBranchUnopened is
// ProcessResult's own race -- a close that lands while a run is still
// live -- restated for a run that never got the chance to notice because
// the process driving it crashed instead of finishing normally. Recovery
// must still free the task (it is finished either way), but must not
// open a pull request nobody wants merged.
func TestRecoverOrphanedRunsLeavesAClosedTasksBranchUnopened(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	task := filedTask(t, ctx, store, "t1", model.RepoRef{Owner: "acme", Name: "widgets"})
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	d := dispatch.Dispatch{TaskID: task.ID, RunID: "t1-1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)

	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, ClosedAt: &baseTime}); err != nil {
		t.Fatalf("closing task: %v", err)
	}

	if err := orchestrator.RecoverOrphanedRuns(ctx, store, client, baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("RecoverOrphanedRuns: %v", err)
	}

	if len(sim.PullRequests) != 0 {
		t.Fatalf("expected no pull request for a closed task, got %+v", sim.PullRequests)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed", st)
	}
}
