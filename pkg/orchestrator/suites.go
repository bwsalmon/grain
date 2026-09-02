package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// SyncTaskSuites advances every active task suite run by exactly the
// step it is missing (bwsalmon/agents#642): if its current pass has not
// finished yet, nothing -- next cycle checks again; if it has, either
// stop the run (TaskSuiteRunSucceeded or TaskSuiteRunFailed) or fire the
// next pass, whichever model.OutcomeOfPass and the run's own Mode/
// Count/MaxPasses say to do.
//
// Level-triggered like every other reconciler here: ActiveTaskSuiteRuns
// only ever names a run this cycle still has something to check, so a
// run already Succeeded or Failed is never read again, and one run's own
// store error does not stop this cycle from advancing the others.
func SyncTaskSuites(ctx context.Context, store *model.Store, now time.Time) error {
	runs, err := store.ActiveTaskSuiteRuns(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: listing active task suite runs: %w", err)
	}
	var errs []error
	for _, run := range runs {
		if err := syncTaskSuiteRun(ctx, store, run, now); err != nil {
			errs = append(errs, fmt.Errorf("orchestrator: advancing task suite run %d: %w", run.ID, err))
		}
	}
	return errors.Join(errs...)
}

// syncTaskSuiteRun decides run's own next step from its current pass's
// outcome:
//
//   - PassPending: the pass has not finished (or, for a
//     RequireApproval run, a task in it is still waiting on a human's
//     approval) -- do nothing this cycle.
//   - PassFailed: at least one task in the pass failed or closed without
//     completing -- the run stops here, TaskSuiteRunFailed.
//   - PassClean or PassChanged: every task in the pass completed. A
//     TaskSuiteCount run fires another pass until it has run Count of
//     them, then stops, succeeded, whatever any pass produced.
//     A TaskSuiteUntilClean run stops the moment a pass is
//     PassClean (bwsalmon/agents#642's own "run the tasks until they
//     generate no issues or repo changes"), or fires another pass, up to
//     MaxPasses -- reaching MaxPasses without ever seeing a clean pass
//     is reported as TaskSuiteRunFailed, not silently accepted, so a
//     suite that never converges is visible rather than looking the same
//     as one that succeeded on its very last allowed pass.
func syncTaskSuiteRun(ctx context.Context, store *model.Store, run model.TaskSuiteRun, now time.Time) error {
	pass := run.CurrentPass()
	outcome := model.OutcomeOfPass(run.PassTasks(pass))

	switch outcome {
	case model.PassPending:
		return nil
	case model.PassFailed:
		return store.CompleteTaskSuiteRun(ctx, run.ID, model.TaskSuiteRunFailed,
			fmt.Sprintf("pass %d had a task that failed, or closed without completing", pass), now)
	}

	switch run.Mode {
	case model.TaskSuiteCount:
		if pass >= run.Count {
			return store.CompleteTaskSuiteRun(ctx, run.ID, model.TaskSuiteRunSucceeded, "", now)
		}
	case model.TaskSuiteUntilClean:
		if outcome == model.PassClean {
			return store.CompleteTaskSuiteRun(ctx, run.ID, model.TaskSuiteRunSucceeded, "", now)
		}
		if pass >= run.MaxPasses {
			return store.CompleteTaskSuiteRun(ctx, run.ID, model.TaskSuiteRunFailed,
				fmt.Sprintf("reached %d passes without one that produced no repo change or follow-up task", run.MaxPasses), now)
		}
	default:
		return fmt.Errorf("task suite run %d has an unknown mode %q", run.ID, run.Mode)
	}

	_, err := store.FireNextPass(ctx, run, now)
	return err
}
