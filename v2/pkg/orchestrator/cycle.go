package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/dispatch"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Deps is everything one RunCycle call needs. Framework is a factory, not
// a shared instance: a real deployment gives every dispatch its own agent
// conversation, the way gemini.Framework.Run's own in-process MCP server
// is scoped to one run's own tools at a time. Sandboxes is HostSandboxes
// for the local-directory stand-in, or KonturSandboxes for a real
// bwsalmon/kontur-managed VM per slot — see the package doc comment.
type Deps struct {
	Store     *model.Store
	Client    github.Client
	Sandboxes Sandboxes
	Framework func() agent.Framework
	Config    Config
	Slots     []string
}

// Reconciler is one independent unit of a cycle: a named function that
// reads current state, does whatever that state implies, and reports
// what went wrong without deciding anything about the rest of the cycle.
//
// Each one is level-triggered and idempotent — it re-reads the store and
// GitHub every time rather than acting on a change it was handed — so
// running one is always safe, and skipping one costs latency rather than
// correctness. That is what makes them independent: a reconciler that
// fails this cycle has done nothing the next cycle cannot redo, so no
// other reconciler has any reason to be held back by it.
type Reconciler struct {
	Name      string
	Reconcile func(ctx context.Context, deps Deps, now time.Time) error
}

// Reconcilers returns the cycle's reconcilers, in the order RunCycle runs
// them.
//
// There used to be a "poll" that listed the task repo's labelled GitHub
// issues and turned each into a task, and it is gone: tasks are created by
// writing them (README, "Input is a model update, not a GitHub issue"),
// so there is no longer an outside source of tasks to reconcile against.
// A task filed by the CLI or the UI is in task_ready the moment it is
// written, rather than on whichever tick happened to poll next.
//
// "schedule" (bwsalmon/agents#376) and "releases" (bwsalmon/agents#398)
// are the additions since. "schedule" turns each schedule whose interval
// has come due into a real task, the same store write CreateTask itself
// makes, and runs first for the same latency reason syncing runs last --
// a task a schedule files this cycle should be dispatchable this same
// tick, not on the next one. "releases" carries out whatever a cut or
// promotion a human just requested through the UI still declares -- a
// fresh row in the store, not an outside event to poll for, the same
// "the store is the record" shape everything else here already holds to.
//
// "qualifications" (bwsalmon/agents#518) is the newest, and runs right
// after "releases" on purpose: a candidate "releases" has just advanced
// to CandidateActive this same cycle gets its qualification run
// instantiated the same tick rather than waiting a cycle to be noticed,
// and a run whose last task "sync" observed completing earlier this same
// cycle is promoted (when its plan's AutoPromote asks for it) without a
// tick's delay either.
//
// The order among the rest is a latency preference, not a dependency:
// syncing pull requests last lets a merge this very cycle just performed
// be picked up without a tick's delay. All five read their own inputs
// from the store, so a different order produces the same state one cycle
// later — which is exactly why one failing does not invalidate another.
func Reconcilers() []Reconciler {
	return []Reconciler{
		{Name: "schedule", Reconcile: reconcileSchedule},
		{Name: "dispatch", Reconcile: reconcileDispatch},
		{Name: "sync", Reconcile: reconcileSync},
		{Name: "releases", Reconcile: reconcileReleases},
		{Name: "qualifications", Reconcile: reconcileQualifications},
	}
}

// RunCycle is v2's whole Orchestrator.run_once equivalent: let
// dispatch.Cycle decide what runs, actually run it, turn each result into
// the effect it implies, and refresh every pull request grain is still
// watching. A deployment's own timer calls this once per tick; nothing
// here loops on its own.
//
// **Every reconciler runs, whatever the ones before it did.** A cycle is
// not a pipeline: a GitHub outage in one reconciler used to return early
// and take the others down with it, so a merge that was ready and a queue
// that needed advancing waited on a failure they had nothing to do with.
// Each Reconcilers() entry runs on its own and its error is collected
// rather than returned, so what a cycle achieves degrades to whichever
// parts of the world are reachable rather than to the first one that
// isn't. The returned error joins everything that failed (errors.Join, so
// errors.Is still answers for any of them); graind logs it and ticks
// again.
//
// Cancellation is the one thing that does stop a cycle early, since a
// cancelled context means the daemon is shutting down rather than that
// one reconciler has a problem the others might not.
func RunCycle(ctx context.Context, deps Deps, now time.Time) error {
	var errs []error
	for _, r := range Reconcilers() {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := r.Reconcile(ctx, deps, now); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// reconcileSync refreshes every pull request grain is still watching.
func reconcileSync(ctx context.Context, deps Deps, now time.Time) error {
	return SyncPullRequests(ctx, deps.Store, deps.Client, now)
}

// reconcileReleases carries out whatever GitHub-side effect a release
// candidate still declares (bwsalmon/agents#398) but has not yet had
// performed -- cutting its own branch, or promoting it.
func reconcileReleases(ctx context.Context, deps Deps, now time.Time) error {
	return SyncReleases(ctx, deps.Store, deps.Client, now)
}

// reconcileQualifications schedules and, once successful, auto-promotes
// each repo's qualification plan (bwsalmon/agents#518) against its own
// active release candidate.
func reconcileQualifications(ctx context.Context, deps Deps, now time.Time) error {
	return SyncQualifications(ctx, deps.Store, now)
}

// reconcileDispatch lets dispatch.Cycle decide what runs, then runs every
// dispatch it decided on concurrently, one goroutine per dispatch.
//
// This is what actually makes -max-concurrent/GRAIN_MAX_CONCURRENT a
// concurrency knob rather than just a scheduling one (bwsalmon/
// agents#435): each Dispatch names a
// distinct, free slot (dispatch.Cycle's own loop draws each one from
// store.OccupiedSlots-filtered "free" without repeats), so two dispatches
// from the same Cycle call never touch the same sandbox, the same
// model.Run row, or the same task -- there is nothing for concurrent
// runOne calls to race over. HostSandboxes and KonturSandboxes both guard
// their own per-slot state with a mutex keyed by slot, and model.Store is
// already built for concurrent callers (pkg/model/sqlite's own doc
// comment: "the single writer for the whole store" -- serialization
// happens inside Store, not by a caller taking turns), so nothing here
// needs to hold a lock of its own.
//
// One dispatch failing does not abandon the others. Every Dispatch
// dispatch.Cycle returns already has its own durable store row (its own
// doc comment: "the store write is already durable by the time a Dispatch
// is returned"), so a slot whose run fails here is a slot whose task is
// recorded as attempted either way — dropping the remaining dispatches on
// the floor would leave their slots idle for a tick without changing
// anything about the one that failed. errs is appended to under errsMu
// since the goroutines below share it; ctx itself (passed to every
// runOne unchanged) is what stops them early on cancellation -- framework.
// Run and the store calls underneath it all already respect it, so there
// is no separate ctx.Err() check to make before launching them the way
// the old sequential loop needed one before each iteration.
func reconcileDispatch(ctx context.Context, deps Deps, now time.Time) error {
	dispatches, err := dispatch.Cycle(ctx, deps.Store, deps.Slots, now)
	if err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}

	var errsMu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, d := range dispatches {
		wg.Add(1)
		go func(d dispatch.Dispatch) {
			defer wg.Done()
			if err := runOne(ctx, deps, d, now); err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
			}
		}(d)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// recreatingSandboxes is implemented by a Sandboxes backend that supports
// resetting a slot's sandbox to a clean state between tasks --
// KonturSandboxes' own Recreate. HostSandboxes does not implement it: the
// local-directory stand-in is deliberately left long-lived, resetting one
// between tasks being "the caller's job" per its own doc comment, the same
// as it always has been; only a real sandbox backend gains the recreate-
// after-each-task boundary bwsalmon/agents#353 asked for.
type recreatingSandboxes interface {
	Recreate(ctx context.Context, slot string) error
}

func runOne(ctx context.Context, deps Deps, d dispatch.Dispatch, now time.Time) error {
	task, err := deps.Store.GetTask(ctx, d.TaskID)
	if err != nil {
		return fmt.Errorf("orchestrator: reading task %s: %w", d.TaskID, err)
	}
	if task == nil {
		return fmt.Errorf("orchestrator: dispatch.Cycle dispatched unknown task %s", d.TaskID)
	}

	tools, err := deps.Sandboxes.ToolsFor(ctx, d.Slot)
	if err != nil {
		return err
	}

	var sandboxRoot string
	if deps.Config.Capabilities != nil && len(task.Grants) > 0 {
		rooted, ok := deps.Sandboxes.(rootedSandboxes)
		if !ok {
			return fmt.Errorf("orchestrator: task %s requests capabilities but slot %s's sandbox has no local directory to place them in", task.ID, d.Slot)
		}
		sandboxRoot, err = rooted.RootFor(d.Slot)
		if err != nil {
			return err
		}
	}

	result, runErr := RunDispatch(ctx, deps.Store, deps.Framework(), deps.Config, *task, d, tools, sandboxRoot, now)

	// The sandbox is recreated once this dispatch is done with it,
	// whether or not it succeeded -- a failed or cancelled run is exactly
	// the kind of run that should not leave anything behind for the next
	// one dispatched onto this slot. A recreate failure is reported
	// alongside whatever else went wrong, but it never itself skips
	// ProcessResult below: what a successful run's result implies for
	// GitHub does not depend on whether cleaning up after it also
	// succeeded.
	var recreateErr error
	if recreater, ok := deps.Sandboxes.(recreatingSandboxes); ok {
		if err := recreater.Recreate(ctx, d.Slot); err != nil {
			recreateErr = fmt.Errorf("orchestrator: recreating slot %s's sandbox after task %s: %w", d.Slot, task.ID, err)
		}
	}

	if runErr != nil {
		// A failed run is not necessarily an empty one. The framework
		// hands back what the agent managed to do before it broke (see
		// agent.Framework), and the one part of that which outlives the
		// run is a branch it already pushed -- the sandbox above has just
		// been recreated, but GitHub still has the commits. Skipping
		// ProcessResult here left such a branch permanently stranded:
		// pushed, with no pull request, no closing comment and no second
		// run able to find it, until a daemon restart happened to run
		// RecoverOrphanedRuns over a row this one had already finished
		// and so was no longer live. An agent that commits, pushes and
		// then runs out of turns did the work; only the ending failed.
		//
		// Only the pushed-branch half, not all of ProcessResult: a
		// question or a proposed task from a run that then failed is a
		// half-formed intention, and the run's outcome stays "failed"
		// with the framework's own diagnosis rather than being overwritten
		// with no_action.
		var salvageErr error
		if result != nil {
			if _, err := salvagePushedBranch(ctx, deps.Store, deps.Client, *task, now); err != nil {
				salvageErr = err
			}
		}
		return errors.Join(runErr, salvageErr, recreateErr)
	}
	return errors.Join(ProcessResult(ctx, deps.Store, deps.Client, *task, result, d.RunID, now), recreateErr)
}
