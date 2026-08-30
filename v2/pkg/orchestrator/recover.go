package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// RecoverOrphanedRuns finishes every task_run a previous process left
// with no `finished_at` -- bwsalmon/agents#425's gap: task_state
// (schema.go) reads a run with a live row as 'running' regardless of
// whether the process that started it still exists, so a daemon killed
// mid-run otherwise leaves that task stuck there forever, with no route
// back to 'queued' and no way for dispatch.Cycle to ever try it again.
//
// This is a startup pass, not a periodic Reconciler, and deliberately
// so: a daemon calls it exactly once, in cmd/grain/daemon.go's own run(),
// before RunCycle ever runs for the first time. At that specific moment a
// live task_run row cannot belong to this process -- it hasn't dispatched
// anything yet -- so it can only be left over from whatever last held
// this store's own -data-dir, crashed or otherwise stopped mid-run. A
// periodic reconciler re-run on every tick would have no such guarantee:
// the very run reconcileDispatch is in the middle of driving right now
// also has no `finished_at` yet, and there is nothing in the row itself
// that distinguishes "still legitimately running" from "abandoned" once
// a cycle is free to interleave with this pass. Restricting recovery to
// startup sidesteps needing a lease/heartbeat mechanism to tell the two
// apart at all.
//
// Finishing the run (outcome "orphaned") is what actually frees the
// task: task_state stops reading it as 'running' the moment
// `finished_at` is set, which drops it back to 'queued' (or 'failed',
// once enough orphaned attempts accumulate against task_streak the same
// as any other non-'succeeded' outcome would) for dispatch.Cycle to pick
// up on its own next tick. What ProcessResult would have done with that
// run's result -- relaying a comment, opening a pull request -- is gone
// with the crashed process, except for the one part GitHub itself still
// remembers: a branch the run may have already pushed before it died.
// recoverRun checks for exactly that, mirroring ProcessResult's own
// "pushed but not yet turned into a PR" handling, so a run that crashed
// after pushing but before finishWithPullRequest ever got to run does not
// leave a real branch permanently stranded and unopened.
func RecoverOrphanedRuns(ctx context.Context, store *model.Store, client github.Client, now time.Time) error {
	runs, err := store.LiveRuns(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: reading live runs: %w", err)
	}

	var errs []error
	for _, r := range runs {
		if err := recoverRun(ctx, store, client, r, now); err != nil {
			errs = append(errs, fmt.Errorf("orchestrator: recovering orphaned run %s: %w", r.ID, err))
		}
	}
	return errors.Join(errs...)
}

// recoverRun is RecoverOrphanedRuns' per-run decision: finish r as
// "orphaned" first -- so a task_id lookup or a GitHub call failing below
// still leaves the task dispatchable again rather than stuck exactly
// where it started -- then check whether its run had already pushed a
// branch worth turning into a pull request.
func recoverRun(ctx context.Context, store *model.Store, client github.Client, r model.Run, now time.Time) error {
	if err := store.FinishRun(ctx, r.ID, now, "orphaned",
		"the process driving this run exited before it finished; recovered at the next daemon startup"); err != nil {
		return fmt.Errorf("finishing: %w", err)
	}

	task, err := store.GetTask(ctx, r.TaskID)
	if err != nil {
		return fmt.Errorf("reading task %s: %w", r.TaskID, err)
	}
	if task == nil {
		return nil
	}
	_, err = salvagePushedBranch(ctx, store, client, *task, now)
	return err
}
