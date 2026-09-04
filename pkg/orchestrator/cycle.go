package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/dispatch"
	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/model"
)

// Deps is everything one RunCycle call needs. Framework is a factory, not
// a shared instance: a real deployment gives every dispatch its own agent
// conversation, the way a run's own MCP server
// is scoped to one run's own tools at a time. Sandboxes is HostSandboxes
// for the local-directory stand-in, or KonturSandboxes for a real
// bwsalmon/kontur-managed VM — see the package doc comment.
//
// MaxWorkers/MaxMergers are how many runs may be in flight at once, of
// each kind model.Limits distinguishes. They were one []string of slot
// identifiers until slots stopped existing, then one count; dispatch.
// Cycle counts live runs against them instead of differencing a pool.
type Deps struct {
	Store     *model.Store
	Client    github.Client
	Sandboxes Sandboxes
	// Framework builds the agent.Framework one dispatch is driven by.
	// The string is the task's own model.Task.AgentFramework -- empty
	// meaning "this deployment's default" (model.Config.AgentFramework),
	// which only the caller knows, so resolving that is the factory's
	// job and not this package's (cmd/grain's own agentFrameworks).
	//
	// It returns an error rather than a Framework for the case a
	// deployment can now genuinely be in: the credential that framework
	// runs as (a Gemini API key, a Claude Code OAuth token) is set from
	// the UI, so it may simply not be there yet when a task naming that
	// framework comes up for dispatch. runOne reports that as a
	// setup-failed run naming what is missing, rather than the daemon
	// refusing to start at all the way a startup-time construction had
	// to.
	Framework func(ctx context.Context, framework string) (agent.Framework, error)
	Config    Config
	// MintSandboxToken returns the git-proxy bearer token identifying a
	// sandbox -- gitproxy.SandboxTokenStore.EnsureToken, in a deployment;
	// nil in a test with no proxy, which skips configuring credentials
	// altogether.
	//
	// A token is minted here, per run, rather than once per slot at
	// daemon startup as it was while a sandbox outlived the runs
	// dispatched onto it. That is the security property docs/
	// data-model.md predicted would fall out of a sandbox per task: a
	// proxy token now dies with the run that used it, so one that leaks
	// cannot be replayed by, or on behalf of, anything dispatched after
	// it.
	MintSandboxToken func(sandbox string) (string, error)
	// RevokeSandboxToken drops the token MintSandboxToken made, once the
	// sandbox it identified has been released -- gitproxy.
	// SandboxTokenStore.Revoke, in a deployment; nil wherever
	// MintSandboxToken is.
	//
	// It is the other half of minting per run. One token per slot was a
	// fixed set that a deployment carried for its whole life; one per run
	// is a new entry every dispatch, in a file every mint reads and
	// rewrites whole. See that method's own doc comment: this is upkeep,
	// not authorization, which Store.GitScope already handles by
	// resolving a sandbox through the live run on it.
	RevokeSandboxToken func(sandbox string) error
	// MaxWorkers is the starting value for a deployment -- cmd/grain
	// seeds it from cfg.maxWorkers -- but RunCycle overwrites its own
	// copy every cycle from deps.Store's grain_config row, so changing it
	// through the store takes effect on the next tick rather than
	// requiring a restart. See RunCycle's own doc comment.
	MaxWorkers int
	// MaxMergers is MaxWorkers' counterpart for the merge queue's own fix
	// tasks (grain/task-63): capacity on top of MaxWorkers that only they
	// may reach, refreshed from the store every cycle the same way. The
	// pair of them is what reconcileDispatch hands dispatch.Cycle as one
	// model.Limits -- see that type for the rule they make together.
	MaxMergers int
	// Runs, when non-nil, is where a cycle parks the runs it starts:
	// reconcileDispatch gives each dispatch a goroutine tracked there and
	// returns without waiting for it, so the cycle -- and the tick after
	// it -- is over in the time the dispatch *decisions* take rather than
	// in the time the agents take. That is what makes the configured
	// concurrency reachable by a deployment whose tasks arrive one at a
	// time: see InFlight's own doc comment for what waiting used to
	// cost, and cmd/grain's drainInFlight for the other half, draining it at
	// shutdown so a run still gets to release its sandbox.
	//
	// Nil means a cycle waits for every run it started before returning,
	// and joins their errors into its own. That is the shape this package
	// had before there was an InFlight at all, and it is the right one for
	// a caller that has no loop to tick again -- a test that dispatches
	// and then asserts on what the run did, or a one-shot cycle -- since
	// such a caller has nothing to hold the process open while a detached
	// goroutine works. A deployment always sets one.
	Runs *InFlight
	// CycleTimes, when non-nil, is where each RunCycle call records how
	// long it took and where inside itself that time went -- see
	// CycleTimes' own doc comment for why the record is a ring in this
	// process rather than a row per tick.
	//
	// nil means the cycle measures nothing, which is what every caller
	// with no report to serve wants (a test, a one-shot cycle). A
	// deployment sets one; cmd/grain hands the same ring to its UI/API
	// server, which serves it as GET /api/metrics' own "cycles" section.
	CycleTimes *CycleTimes
}

// runCleanupTimeout bounds each of the two things runOne does after a run
// is over -- releasing its sandbox and finishing its row -- on a context
// detached from the caller's own cancellation.
//
// Detaching is what makes them happen at all when the daemon is shutting
// down or a task was closed mid-run (their own comments in runOne).
// Bounding is what keeps that from being worse than not detaching: both
// reach something that can hang -- `konturctl vm delete` against a wedged
// docker, a store write behind a busy database -- and an unbounded
// detached context would pin the dispatch goroutine, and the run row it
// has not finished yet, for the life of the process. Generous rather than
// tight, since a VM teardown is genuinely slow and the alternative to
// waiting is leaving one running.
const runCleanupTimeout = 2 * time.Minute

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

// dispatchReconciler names the one reconciler in the cycle that decides
// what runs now -- the one a queued task is actually waiting for, which
// is why CycleTiming.DispatchWait singles its start out from the rest.
const dispatchReconciler = "dispatch"

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
// "qualifications" (bwsalmon/agents#518) runs right after "releases" on
// purpose: a candidate "releases" has just advanced to CandidateActive
// this same cycle gets its qualification run instantiated the same tick
// rather than waiting a cycle to be noticed, and a run whose last task
// "sync" observed completing earlier this same cycle is promoted (when
// its plan's AutoPromote asks for it) without a tick's delay either.
//
// "branches" (bwsalmon/agents#638) is the newest: it creates whatever
// branch a human just requested from the repo page still declares but has
// not yet had created on GitHub -- the same "a fresh row in the store,
// not an outside event to poll for" shape "releases" already holds to,
// just without a candidate or qualification sequence of its own to chain
// off of, so its place in the order carries no latency preference the way
// "releases" before "qualifications" does.
//
// "reviews" (grain/task-284) files the review a finished task declares,
// from the template it names, and runs before "dispatch" and "sync" for
// the latency reason "schedule" runs first: a review filed this cycle is
// dispatchable this same tick, and the merge queue sees it the same tick
// too. That placement is a preference and not a dependency, though it
// looks like one -- syncEntry holds a reviewed task's own merge on the
// declaration (Task.ReviewTemplateID) rather than on the review task
// having been filed yet, precisely so that whether a change merges
// before its review exists does not come down to this ordering.
//
// The order among the rest is a latency preference, not a dependency:
// syncing pull requests last lets a merge this very cycle just performed
// be picked up without a tick's delay. All of them read their own inputs
// from the store, so a different order produces the same state one cycle
// later — which is exactly why one failing does not invalidate another.
func Reconcilers() []Reconciler {
	return []Reconciler{
		{Name: "schedule", Reconcile: reconcileSchedule},
		{Name: "reviews", Reconcile: reconcileReviews},
		{Name: dispatchReconciler, Reconcile: reconcileDispatch},
		{Name: "sync", Reconcile: reconcileSync},
		{Name: "releases", Reconcile: reconcileReleases},
		{Name: "branches", Reconcile: reconcileBranches},
		{Name: "qualifications", Reconcile: reconcileQualifications},
		{Name: "suites", Reconcile: reconcileSuites},
	}
}

// StaticFramework adapts one already-built agent.Framework to what
// Deps.Framework wants -- for a caller with exactly one framework and no
// per-task choice to make (every test in this repo, and any embedder
// driving a single framework of its own). A real deployment does not use
// it: cmd/grain builds the framework a run needs when that run is
// dispatched, from the credential the UI has by then, which is the whole
// point of the factory taking a name and returning an error.
func StaticFramework(f agent.Framework) func(context.Context, string) (agent.Framework, error) {
	return func(context.Context, string) (agent.Framework, error) { return f, nil }
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
//
// deps.MaxWorkers, deps.MaxMergers, deps.Config.MaxAgentTurns and
// deps.Config.PromptExtension are
// refreshed from deps.Store's own grain_config row (if any) before any
// reconciler runs, so a change made through the store -- `grain
// settings`, or the UI's Settings page -- takes effect on this cycle
// rather than at the next
// restart. They are the settings this package itself is the consumer
// of; the rest of model.Config is re-applied by whoever owns the piece
// it configures (cmd/grain/daemon.go's liveConfig, once per tick, for
// everything a running daemon can adopt at all). deps is taken by value,
// so this only ever affects the cycle it is refreshing and the dispatch
// goroutines that cycle starts -- never a caller's own Deps.
//
// A store with no row yet -- deps.Store is nil in tests that build Deps
// by hand, or GetConfig itself returns nil -- leaves every one of them
// exactly as the caller set it.
//
// A cycle times itself when deps.CycleTimes is set: the whole tick, each
// reconciler's own share of it, and how far in the dispatch decision was
// reached. That last number is the one a deployment cannot otherwise
// get -- see CycleTiming.DispatchWait -- since a queue wait looks
// identical whether the deployment was full or the tick was simply slow
// to get to dispatch.
func RunCycle(ctx context.Context, deps Deps, now time.Time) error {
	// The tick times itself from here, before the stored-configuration
	// read below: that read is a store call like any other, on the
	// critical path of every dispatch decision, so a cycle that measured
	// only its reconcilers would under-report exactly the kind of
	// slowness this is here to catch.
	//
	// timing is filled in as the cycle goes and handed over in one write
	// at the end, so nothing on this path takes the ring's lock more than
	// once per tick. The deferred Observe declines a cycle cut short by
	// cancellation: that is the shutdown tick, whose truncated duration
	// describes the daemon stopping rather than the daemon being slow,
	// and there is at most one of them per process.
	started := time.Now()
	timing := CycleTiming{Start: now}
	if deps.CycleTimes != nil {
		defer func() {
			if ctx.Err() != nil {
				return
			}
			timing.Duration = time.Since(started)
			deps.CycleTimes.Observe(timing)
		}()
	}

	if deps.Store != nil {
		if mc, err := deps.Store.GetConfig(ctx); err != nil {
			log.Printf("orchestrator: reading stored configuration: %v", err)
		} else if mc != nil {
			deps.MaxWorkers, deps.MaxMergers = mc.MaxWorkers, mc.MaxMergers
			deps.Config.MaxAgentTurns = mc.MaxAgentTurns
			deps.Config.PromptExtension = mc.PromptExtension
		}
	}
	var errs []error
	reconcilers := Reconcilers()
	if deps.CycleTimes != nil {
		timing.Reconcilers = make([]ReconcilerTiming, 0, len(reconcilers))
	}
	for _, r := range reconcilers {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		wait := time.Since(started)
		began := time.Now()
		err := recoverReconcile(ctx, r, deps, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
		}
		if deps.CycleTimes == nil {
			continue
		}
		timing.Reconcilers = append(timing.Reconcilers, ReconcilerTiming{
			Name:     r.Name,
			Wait:     wait,
			Duration: time.Since(began),
			Failed:   err != nil,
		})
		if r.Name == dispatchReconciler {
			timing.DispatchWait = wait
		}
	}
	return errors.Join(errs...)
}

// recoverReconcile calls r.Reconcile and turns a panic into an error the
// same way this func's own caller already treats any other error one
// reconciler returns -- see RunCycle's own "one reconciler having a
// problem shouldn't take the others down": a panic is such a problem too,
// and cmd/grain's own reconcile loop, which calls RunCycle once per tick,
// has nothing of its own left to recover it with by the time it has
// already unwound past this call (bwsalmon/agents#550 -- an unrecovered
// panic here would otherwise crash the whole grain daemon process,
// including the UI/API server it now also serves alongside RunCycle,
// bwsalmon/agents#363).
func recoverReconcile(ctx context.Context, r Reconciler, deps Deps, now time.Time) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v\n%s", rec, debug.Stack())
		}
	}()
	return r.Reconcile(ctx, deps, now)
}

// reconcileReviews files the review each finished task declares
// (grain/task-284), from the template it names, onto the branch that
// task's own run pushed.
func reconcileReviews(ctx context.Context, deps Deps, now time.Time) error {
	return SyncReviews(ctx, deps.Store, now)
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

// reconcileBranches carries out whatever GitHub-side create a requested
// branch (bwsalmon/agents#638) still declares but has not yet had
// performed.
func reconcileBranches(ctx context.Context, deps Deps, now time.Time) error {
	return SyncBranches(ctx, deps.Store, deps.Client, now)
}

// reconcileQualifications schedules and, once successful, auto-promotes
// each repo's qualification plan (bwsalmon/agents#518) against its own
// active release candidate.
func reconcileQualifications(ctx context.Context, deps Deps, now time.Time) error {
	return SyncQualifications(ctx, deps.Store, now)
}

// reconcileSuites advances every suite run still in flight
// (bwsalmon/agents#642), firing its next pass or stopping it once its
// own Mode says to. Runs last, after "qualifications", for the same
// latency reason that one runs right after "releases": nothing else
// here produces or consumes a suite run, so its own position among the
// rest is a preference, not a dependency.
func reconcileSuites(ctx context.Context, deps Deps, now time.Time) error {
	return SyncSuites(ctx, deps.Store, now)
}

// reconcileDispatch lets dispatch.Cycle decide what runs, then runs every
// dispatch it decided on concurrently, one goroutine per dispatch --
// handed to deps.Runs, and so outliving this cycle, wherever a caller has
// given it somewhere to park them (Deps.Runs).
//
// This is what actually makes -max-workers/GRAIN_MAX_WORKERS a
// concurrency knob rather than just a scheduling one (bwsalmon/
// agents#435): each Dispatch names a distinct task and a distinct run,
// and each run acquires a sandbox of its own, so two dispatches from the
// same Cycle call never touch the same sandbox, the same model.Run row,
// or the same task -- there is nothing for concurrent runOne calls to
// race over. That used to rest on dispatch.Cycle handing out distinct
// slots from a pool; it now rests on the simpler fact that a sandbox
// belongs to one run and is built for it. model.Store is already built
// for concurrent callers (pkg/model/sqlite's own doc comment: "the
// single writer for the whole store" -- serialization happens inside
// Store, not by a caller taking turns), so nothing here needs to hold a
// lock of its own.
//
// One dispatch failing does not abandon the others. Every Dispatch
// dispatch.Cycle returns already has its own durable store row (its own
// doc comment: "the store write is already durable by the time a Dispatch
// is returned"), so a run that fails here is one whose task is recorded
// as attempted either way — dropping the remaining dispatches on the
// floor would leave that capacity idle for a tick without changing
// anything about the one that failed. errs is appended to under errsMu
// since the goroutines below share it; ctx itself (passed to every
// runOne unchanged) is what stops them early on cancellation -- framework.
// Run and the store calls underneath it all already respect it, so there
// is no separate ctx.Err() check to make before launching them the way
// the old sequential loop needed one before each iteration.
func reconcileDispatch(ctx context.Context, deps Deps, now time.Time) error {
	// Nothing is dispatched while the deployment's agent has no budget
	// left: a task started now would spend a sandbox and an attempt
	// reaching the same refusal the run that closed this gate already
	// reached (Config.Pause). Only this reconciler stops -- syncing pull
	// requests, advancing the merge queue and cutting releases cost no
	// agent tokens and are exactly what should keep working while the
	// window is shut.
	//
	// Asked once per tick, and level-triggered like every other
	// reconciler: the pause lifts because a later tick found its instant
	// passed, not because anything here holds a timer.
	//
	// Silent per tick on purpose: Pause logs the one line worth having
	// when it closes and the one worth having when it opens again, and a
	// line per tick for however many hours a provider's window lasts
	// would bury both.
	if _, _, blocked := deps.Config.Pause.Blocked(now); blocked {
		return nil
	}

	// dispatch.Busy is what keeps a task this process is still finishing
	// with out of this cycle's reach: a run's row is finished before
	// runOne has turned its result into the effects it implies, and a
	// tick landing in that window would otherwise dispatch the same task
	// again. There is no such window without deps.Runs -- a cycle that
	// waits for its own runs has no next tick to fall into it -- which is
	// why the option goes with it.
	var opts []dispatch.Option
	if deps.Runs != nil {
		opts = append(opts, dispatch.Busy(deps.Runs.Busy))
	}
	limits := model.Limits{Workers: deps.MaxWorkers, Mergers: deps.MaxMergers}
	dispatches, err := dispatch.Cycle(ctx, deps.Store, limits, now, opts...)
	if err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}

	// With somewhere to park them, a run outlives the cycle that started
	// it: the goroutines below are handed to deps.Runs and this function
	// returns as soon as they are launched. See Deps.Runs and InFlight
	// for why -- in short, waiting here made a cycle as slow as its
	// slowest agent, and cmd/grain's reconcile loop does not tick again
	// until a cycle returns, so a single running task stopped every other
	// task from starting however much of the deployment's capacity was
	// free.
	//
	// An error has nowhere to be returned to once that happens -- the
	// cycle it belonged to is long over -- so it is logged, the same way
	// cmd/grain's own loop logs whatever a cycle returns. Nothing else is
	// lost by that: the run's own outcome and diagnosis are already in
	// the store, on all three of the paths there are to get them there --
	// RunDispatch finishes the row for anything the agent's own run did,
	// runOne's setup guard finishes one whose sandbox never came up, and
	// noteFinishFailure records what went wrong after the run, once
	// RunDispatch has already finished the row. That is where a UI or a
	// retry reads them from, rather than from a cycle's error.
	if deps.Runs != nil {
		for _, d := range dispatches {
			deps.Runs.Go(d.RunID, d.TaskID, func() {
				if err := recoverRunOne(ctx, deps, d, now); err != nil {
					log.Printf("orchestrator: run %s (task %s): %v", d.RunID, d.TaskID, err)
				}
			})
		}
		return nil
	}

	var errsMu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, d := range dispatches {
		wg.Add(1)
		go func(d dispatch.Dispatch) {
			defer wg.Done()
			if err := recoverRunOne(ctx, deps, d, now); err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
			}
		}(d)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// recoverRunOne wraps runOne with the same panic-to-error conversion
// recoverReconcile (cycle.go, above) gives every Reconciler, for the same
// "one dispatch failing does not abandon the others" reason this func's
// own caller already documents -- and for a reason unique to running in
// its own goroutine, one per concurrent dispatch: recover only ever
// catches a panic on the same goroutine it was deferred on, so
// RunCycle's/recoverReconcile's own recover, one level up, could never
// have caught a panic here anyway. Left unrecovered, it would crash the
// whole grain daemon process -- UI/API server included
// (bwsalmon/agents#550) -- over what should only ever cost this one
// dispatch its run.
func recoverRunOne(ctx context.Context, deps Deps, d dispatch.Dispatch, now time.Time) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic running dispatch %s (task %s): %v\n%s", d.RunID, d.TaskID, rec, debug.Stack())
		}
	}()
	return runOne(ctx, deps, d, now)
}

func runOne(ctx context.Context, deps Deps, d dispatch.Dispatch, now time.Time) (err error) {
	task, err := deps.Store.GetTask(ctx, d.TaskID)
	if err != nil {
		return fmt.Errorf("orchestrator: reading task %s: %w", d.TaskID, err)
	}
	if task == nil {
		return fmt.Errorf("orchestrator: dispatch.Cycle dispatched unknown task %s", d.TaskID)
	}

	// Everything between here and RunDispatch is this run's setup, and any
	// of it can fail: a kontur VM has to boot, a token has to be minted, a
	// guest has to accept a git config. dispatch.Cycle has already made
	// this run durable (its own doc comment), and RunDispatch is what
	// finishes it on every path once it takes over -- so a setup failure
	// returning straight out of here would leave the row live with nothing
	// left to finish it.
	//
	// That is not merely untidy. task_state reads a live run as 'running',
	// so the task never returns to 'queued' and task_ready never offers it
	// again; LiveRunCount still counts the row, so the deployment
	// permanently loses one unit of -max-workers; and retryEligible
	// reads *finished* runs, so the backoff that is supposed to retry a
	// transient failure never sees one to retry. Nothing sweeps it either:
	// Config.MaxRunRuntime is enforced inside RunDispatch, and
	// RecoverOrphanedRuns is a startup pass, so the wedge lasts until the
	// daemon is restarted by hand.
	//
	// It mattered less when a slot's VM was built once at startup and
	// ToolsFor was a cheap lookup after that. A sandbox per run puts a VM
	// boot -- with a ReadyTimeout measured in minutes -- on the setup path
	// of every single dispatch, so this is now the ordinary way a run
	// fails rather than a first-boot curiosity.
	//
	// Finishing it here is what makes bwsalmon/agents#576's successor true:
	// a transient failure costs one run, and dispatch's own backoff
	// (retryBackoff, and MaxConsecutiveFailures behind it) retries the
	// task rather than a human noticing a log line. The outcome is its own
	// word rather than "failed" because no agent ever ran: nothing was
	// attempted that could have gone wrong on its own terms.
	//
	// Detached from ctx's cancellation for the same reason the release
	// below is: a daemon shutting down mid-Acquire is exactly when this
	// row would otherwise be stranded.
	//
	// Whatever setup phrase was standing when it broke is left standing on
	// the finished row, deliberately: nothing renders a finished run's
	// synopsis as current (Store.TaskActivity reads live runs only, and
	// state.js's runActivity checks the task is still running), so it
	// contradicts nothing, and where the run got to before it failed is
	// exactly the context the detail beside it does not carry -- "this
	// run's sandbox could not be prepared: ..." plus "cloning
	// acme/widgets" is a better record than the detail alone.
	ranAgent := false
	defer func() {
		if ranAgent || err == nil {
			return
		}
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runCleanupTimeout)
		defer cancel()
		if ferr := deps.Store.FinishRun(finishCtx, d.RunID, now, "setup-failed",
			"this run's sandbox could not be prepared: "+err.Error()); ferr != nil {
			err = errors.Join(err, fmt.Errorf("orchestrator: finishing run %s after its setup failed: %w", d.RunID, ferr))
		}
	}()

	// What grain itself is doing, on the task's own row, for the stretch
	// before there is an agent to say it -- see setupNotes. Every call
	// below is best-effort and none of them can fail this run.
	notes := setupNotes{store: deps.Store, taskID: d.TaskID, now: deps.Config.now}

	// The sandbox this run gets is named after the run itself. Nothing
	// else is in a position to name it: it is built for this run and torn
	// down with it, so there is no longer-lived thing (a slot, a pool
	// entry) whose name it could inherit, and the run's own ID is already
	// unique, already durable, and already what a log line or a
	// `konturctl vm list` would most usefully show.
	//
	// Recording it in the store is what lets the git proxy resolve a
	// caller back to this run's task (Store.GitScope), so it has to
	// happen before anything inside the sandbox can reach the proxy --
	// which the ordering here gives: the checkout RunDispatch performs is
	// the run's own first use of it.
	sandboxName := d.RunID
	if err := deps.Store.SetRunSandbox(ctx, d.RunID, sandboxName); err != nil {
		return fmt.Errorf("orchestrator: recording run %s's sandbox: %w", d.RunID, err)
	}

	// A task's own SandboxCPUs/SandboxMemoryMB/SandboxDiskGB
	// (bwsalmon/agents#534, grain/task-41) is a create-time argument now
	// rather than something applied to an already-built sandbox: this one
	// is built for this run, so its size is decided once, here, and goes
	// away with it.
	shape := Shape{CPUs: task.SandboxCPUs, MemoryMB: task.SandboxMemoryMB, DiskGB: task.SandboxDiskGB}
	notes.say(ctx, buildingSandboxNote)
	sandbox, err := deps.Sandboxes.Acquire(ctx, sandboxName, shape)
	if err != nil {
		return fmt.Errorf("orchestrator: acquiring a sandbox for run %s: %w", d.RunID, err)
	}

	// The sandbox is released once this dispatch is done with it, whether
	// or not the run succeeded -- a failed or cancelled run is exactly the
	// kind that should leave nothing behind. Deferred rather than run at
	// the end of the happy path: every early return below is a run that
	// has already got a sandbox, and leaking one would hold real resources
	// (a kontur VM, its containers) for the life of the process.
	//
	// A release failure is joined onto whatever else went wrong rather
	// than replacing it, and never skips the rest of this function: what a
	// successful run's result implies for GitHub does not depend on
	// whether cleaning up after it also succeeded. That is also why this
	// needs the named return -- an error produced in a deferred call has
	// nowhere else to go.
	//
	// The release runs on a context detached from ctx's cancellation
	// (though not its values): ctx is cancelled when the daemon is
	// shutting down or the task was closed mid-run, which are exactly the
	// cases where a VM would otherwise be left running with nothing left
	// to release it.
	//
	// Detached, but not unbounded. Release execs `konturctl vm delete`
	// against a host that may be wedged, and a context with no deadline
	// would let that hang hold this goroutine -- and this run's row, which
	// only gets finished below it -- open forever, which is the failure
	// the detachment is supposed to prevent rather than cause. A VM that
	// outlives the timeout is ReapOrphans' problem at the next startup,
	// which is what that pass is for.
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runCleanupTimeout)
		defer cancel()
		if rerr := sandbox.Release(releaseCtx); rerr != nil {
			err = errors.Join(err, fmt.Errorf("orchestrator: releasing run %s's sandbox: %w", d.RunID, rerr))
		}
		// The token identified this sandbox, and the sandbox is gone, so
		// the entry is too -- see SandboxTokenStore.Revoke on why the file
		// is worth pruning now that it grows by one per run rather than
		// holding one entry per slot forever. Logged rather than joined:
		// a leftover entry authorizes nothing on its own (Store.GitScope
		// resolves through the live run, and this one has none), so it is
		// not worth turning a run's own outcome into an error over.
		if deps.RevokeSandboxToken != nil {
			if rerr := deps.RevokeSandboxToken(sandboxName); rerr != nil {
				log.Printf("orchestrator: revoking run %s's sandbox token: %v", d.RunID, rerr)
			}
		}
	}()

	// Mint this sandbox's own proxy token and point its git at the proxy,
	// before the checkout RunDispatch performs needs either. Both used to
	// happen once per slot in cmd/grain's own runDaemon, at startup,
	// which is where they belonged while a slot's sandbox was long-lived
	// and shared across every run that landed on it. Neither is a
	// deployment-wide setup step any more: there is nothing to configure
	// until a run has a sandbox, and what is configured is destroyed with
	// it.
	if deps.MintSandboxToken != nil {
		notes.say(ctx, sandboxCredentialsNote)
		token, err := deps.MintSandboxToken(sandboxName)
		if err != nil {
			return fmt.Errorf("orchestrator: minting run %s's sandbox token: %w", d.RunID, err)
		}
		if err := sandbox.ConfigureGitCredentials(ctx,
			deps.Config.GitRemoteBase+"/placeholder/placeholder.git", token); err != nil {
			return fmt.Errorf("orchestrator: configuring git credentials for run %s: %w", d.RunID, err)
		}
	}

	tools, err := sandbox.Tools(ctx)
	if err != nil {
		return err
	}

	// Config.GrantTools' own doc comment explains why this is gated on
	// Interactive and keyed by raw Grants rather than a resolved
	// GrantResolution: every capability it is wired for today is a GRANT
	// capability (model.ProvisionGrant) whose Resolve always honours, so
	// there is no "granted but refused" case to miss by not waiting for
	// prepareCapabilities below.
	if deps.Config.GrantTools != nil && task.Interactive {
		for _, g := range task.Grants {
			if build, ok := deps.Config.GrantTools[g.Capability]; ok {
				tools = append(tools, build(deps.Store, task.ID)...)
			}
		}
	}

	// sandboxRoot and konturVM are the two ways a Sandbox can describe
	// itself to a Framework with no in-process route to it (agent/claude,
	// via agent.RunConfig.SandboxRoot/.KonturVM -- see that struct's own
	// doc comment): a local directory for a rootedSandbox (HostSandboxes'
	// own), or a named VM for a vmNamedSandbox (KonturSandboxes' own).
	// Computed unconditionally, not just when the task has capabilities to
	// place, since Root()/VMName() are always available where they exist
	// at all and cost nothing to read -- a claude-selected run needs one
	// of them regardless of whether this task granted anything.
	//
	// Whether a missing sandboxRoot actually matters is decided later, by
	// applyPlacements: not every grant materializes a SideSandbox
	// placement (self-debug and self-repair materialize none at all --
	// they only add tools), so erroring here on len(task.Grants) > 0
	// alone used to fail every task holding only those, dispatched onto
	// a non-rooted sandbox (bwsalmon/agents#643), whether or not it had
	// anything to place.
	//
	// placer is the third way, and the one a kontur VM answers with for
	// the grants that DO place something: a route into a sandbox with no
	// local directory, which applyPlacements prefers over sandboxRoot
	// whenever a sandbox offers one. Without it a deployment running
	// -kontur-sandboxes could never receive a gcp-key/gemini-key/
	// github-sandbox placement at all -- every such task failed during
	// preparation instead.
	var sandboxRoot, konturVM string
	var placer SandboxPlacer
	if rooted, ok := sandbox.(rootedSandbox); ok {
		sandboxRoot, err = rooted.Root()
		if err != nil {
			return err
		}
	}
	if named, ok := sandbox.(vmNamedSandbox); ok {
		konturVM = named.VMName()
	}
	if placing, ok := sandbox.(SandboxPlacer); ok {
		placer = placing
	}

	// From here this run can be asked to throw its sandbox away and start
	// again in a fresh one (Config.SandboxRecreations, and pkg/mcp's
	// recreate_sandbox behind it) -- an agent that has wedged its own
	// sandbox, or landed on a guest that stopped answering, otherwise has
	// no move left but to spend the rest of its turns failing.
	//
	// Registered after everything a rebuild has to put back is in hand
	// (the sandbox itself, its tools, and the two routes a placement can
	// take into it) and deferred *after* the release above, so that
	// deferred calls run in the order this needs: unregistering first --
	// which waits out a rebuild already in flight -- and only then
	// destroying the VM that rebuild was building.
	stopRecreating := deps.Config.SandboxRecreations.register(d.TaskID, &sandboxRecreation{
		store:       deps.Store,
		cfg:         deps.Config,
		task:        *task,
		sandbox:     sandbox,
		tools:       tools,
		sandboxRoot: sandboxRoot,
		placer:      placer,
		mint:        deps.MintSandboxToken,
	})
	defer stopRecreating()

	// Built here, after the sandbox and before RunDispatch takes over
	// finishing this run, so a task naming a framework whose credential
	// is not configured yet ends as a setup-failed run saying exactly
	// that (the guard above) rather than as a failed agent run.
	// task.AgentFramework is empty for a task that made no choice of its
	// own; Deps.Framework is what turns that into the deployment's
	// default.
	framework, err := deps.Framework(ctx, task.AgentFramework)
	if err != nil {
		return fmt.Errorf("orchestrator: building the agent framework for run %s: %w", d.RunID, err)
	}

	// From here on RunDispatch owns finishing this run, on every path it
	// can take -- so the setup guard above must not also finish it.
	ranAgent = true
	result, runErr := RunDispatch(ctx, deps.Store, framework, deps.Config, *task, d, tools, sandboxRoot, konturVM, placer, now)

	if runErr != nil {
		// A failed run is not necessarily an empty one. The framework
		// hands back what the agent managed to do before it broke (see
		// agent.Framework), and the one part of that which outlives the
		// run is a branch it already pushed -- the sandbox above is about
		// to be released, but GitHub still has the commits. Skipping
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
			// An empty branch is excepted: salvagePushedBranch has
			// already explained it on the task and parked it
			// (noteEmptyBranch), and this run's row keeps runErr's own
			// diagnosis, which is the more informative of the two -- a
			// run that broke before it had committed anything failed
			// first and pushed nothing second.
			if _, err := salvagePushedBranch(ctx, deps.Store, deps.Client, *task, now); err != nil {
				if _, empty := emptyBranch(err); !empty {
					salvageErr = err
				}
			}
		}
		if salvageErr != nil {
			// Joined, so the overwrite noteFinishFailure is about to make
			// carries the framework's own diagnosis too: a run that ran
			// out of turns *and* could not have its branch salvaged has
			// two things wrong with it, and the row has one detail.
			return noteFinishFailure(ctx, deps.Store, d.RunID, errors.Join(runErr, salvageErr))
		}
		return runErr
	}
	return noteFinishFailure(ctx, deps.Store, d.RunID,
		ProcessResult(ctx, deps.Store, deps.Client, *task, result, d.RunID, now))
}

// noteFinishFailure records, on the run's own row, that turning a
// finished agent run into the effects it implies is what failed -- then
// hands err back unchanged for its caller to return.
//
// It closes the one hole in reconcileDispatch's claim that "the run's own
// outcome and diagnosis are already in the store". They are for
// everything RunDispatch owns: it finishes the row on every path it can
// take. They are not for anything after it. ProcessResult runs once
// FinishRun has already written this run's outcome, and every way it can
// fail is a GitHub call -- reading whether the branch is there
// (branchExistsSettled), finding or opening its pull request
// (EnsurePullRequest) -- so an error here means the agent's work is on
// the remote and nothing points at it. The task is left un-completed and
// dispatch retries it, which is the right thing to do about a transient
// GitHub failure and the wrong thing to do about a persistent one; either
// way the row still says whatever outcomeOf guessed, the error reaches
// only the daemon's journal, and a human reading the task sees an attempt
// whose detail describes a run that went fine, repeated, with no reason
// given anywhere for why it is being repeated. That is precisely the
// question `grain get` exists to answer.
//
// "finish-failed" rather than a correction of the outcome already there:
// the run itself is not what failed, the same distinction runOne's own
// setup guard draws with "setup-failed" at the other end. The vocabulary
// is open (metrics.Runs.Outcomes' own doc comment), and task_streak
// counts it the way it counts any other non-"succeeded" ending, so the
// retry this failure earns still backs off rather than spinning.
//
// The salvage half of a run that had already failed on its own goes
// through here too, with both errors joined, so overwriting the row does
// not throw the framework's own diagnosis away. A salvage that worked
// never reaches this at all, which is what leaves a run that ran out of
// turns after pushing reading "failed" with the reason it failed --
// salvaging its branch was never a claim that the run went well.
//
// A failure to record the failure is joined on rather than replacing it:
// the original error is the one worth reporting, and a store that cannot
// take this write is its own separate problem.
func noteFinishFailure(ctx context.Context, store *model.Store, runID string, err error) error {
	if err == nil {
		return nil
	}
	// Detached from ctx's own cancellation, and bounded, for the same
	// reason the sandbox release above is: a daemon shutting down or a
	// task closed mid-run is exactly when this row would otherwise keep
	// an outcome that does not explain itself.
	noteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runCleanupTimeout)
	defer cancel()
	if serr := store.SetRunOutcome(noteCtx, runID, "finish-failed",
		"this run's result could not be turned into a pull request or a comment: "+
			err.Error()); serr != nil {
		return errors.Join(err, fmt.Errorf("orchestrator: recording run %s's finishing failure: %w", runID, serr))
	}
	return err
}
