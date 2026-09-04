// Package dispatch decides which tasks run now: what one controller
// cycle starts, and nothing more. It does not loop — a deployment's own
// timer does, through orchestrator.RunCycle, which calls Cycle once per
// tick.
//
// Cycle calls StartRun and nothing else — no sandbox is created, no
// GitHub is touched, no agent runs. Those are side effects bwsalmon/
// agents#219 deliberately defers: this package exists to pin down that
// the *decisions* — which task runs, when, and how often — are correct
// on their own, against a real store, before anything external is wired
// to react to them. pkg/orchestrator is the side-effecting counterpart
// that grew around it, and nothing here had to change shape when it did,
// since a Dispatch already says everything that side effect needs to
// know.
//
// It used to decide which task took which *slot*, drawing from a fixed
// pool of generated identifiers that a long-lived sandbox was named
// after and reused under. A sandbox is now created for a run and
// destroyed with it, so there is no pool to assign out of and nothing
// for an identifier to name: concurrency is a count of live runs against
// model.Limits, and the run's own ID is the only name anything
// downstream needs.
//
// There is almost no scheduling policy here: no fairness, no preemption.
// Ordering is whatever task_ready yields, and Cycle takes its prefix,
// skipping over (never reordering past its own turn) a task still backing
// off after a recently failed run -- see retryEligible -- or one of a
// kind whose own half of model.Limits is already full, which is the same
// skip for the same reason. The one other exception is Store.Ready's own:
// a fix task the merge queue filed for a repo's stuck head sorts before
// ordinary ready tasks (task ID is still the tiebreak within each group),
// which is a fact about task_ready rather than a policy Cycle itself
// makes -- see Store.Ready's doc comment (bwsalmon/agents#389) for why. A
// package that ranked ready tasks against each other on some richer
// notion of priority would be a scheduler; this one drains a queue into
// whatever headroom there is.
//
// Which headroom, though, is two numbers rather than one since
// grain/task-63: a merge-queue fix task draws on capacity ordinary work
// cannot reach (model.Limits' own doc comment for what that buys and why
// the two are not symmetric). That is a fact about how much of the
// deployment each kind may occupy, not about their order -- the backlog
// still says which task goes first.
//
// One exception to "drains into whatever headroom there is": the
// configuration agent (Task.Configuration, bwsalmon/agents#621) is not
// drawn from that headroom at all -- see dispatchConfiguration.
//
// One exception to "ordering is whatever task_ready yields", beyond the
// backoff above: a caller may pass Busy to have Cycle pass over a task
// it is itself still finishing with. That is not a scheduling policy
// either -- it is the caller supplying the one fact task_ready cannot
// know, that a run whose row is already finished has not yet had its
// result acted on. See Busy.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// Dispatch is one decision a cycle made: task TaskID was started as its
// Attempt'th run. The store write is already durable by the time a
// Dispatch is returned — this is a record of what happened, for a
// side-effecting layer to act on, not a request for one to decide.
//
// It named a Slot until slots stopped existing. Nothing replaced that
// field: the layer that acts on a Dispatch acquires a sandbox for it and
// records the name itself (Store.SetRunSandbox), so there is nothing
// about a sandbox for this package to have an opinion on.
type Dispatch struct {
	TaskID  string
	RunID   string
	Attempt int
}

// RunID names a run deterministically from its task and attempt number,
// the same reasoning as task.go's BranchName: two callers computing a
// run's name must agree without coordinating, and a name that already
// encodes its own attempt is self-describing in a log with nothing to
// look up.
//
// The separator carries no letter of its own. It used to read
// "<task>-r<attempt>", which was a byte better spent: a run id is what a
// kontur VM is named after, and that name has 11 bytes to live in
// (orchestrator.maxVMNameLen), so the "r" cost real headroom -- one
// decimal digit of task id -- to say something the position of the field
// already says. Task ids never contain "-" (Store.NewTaskID hands out
// decimal counters), so the last "-" still splits the two halves
// unambiguously.
func RunID(taskID string, attempt int) string {
	return fmt.Sprintf("%s-%d", taskID, attempt)
}

// baseRetryBackoff and maxRetryBackoff bound how long Cycle waits after a
// task's run ends without succeeding before dispatching it again:
// baseRetryBackoff after the first such run in a row, doubling
// each further one, capped at maxRetryBackoff -- so a task whose agent
// hits a transient failure gets a handful of prompt retries, while one
// that is wrong in a way no retry fixes stops hammering a real API and a
// real sandbox every single poll interval (bwsalmon/agents#403).
//
// baseRetryBackoff sits below the default poll interval's own 30s
// (cmd/grain daemon's own default) on purpose: the very first retry
// after a failure should not itself add a visible delay on top of the
// next tick, since a poll interval already spaces attempts out. It is
// the doubling that does the real work, not the base.
//
// Once a task's own streak reaches model.MaxConsecutiveFailures,
// task_state (schema.go) stops calling it 'queued' at all, so it drops
// out of Ready before Cycle ever gets here -- these constants only ever
// gate the streak counts below that cutoff.
const (
	baseRetryBackoff = 30 * time.Second
	maxRetryBackoff  = 30 * time.Minute
)

// retryBackoff is how long Cycle waits after the streakCount'th run in a
// row to end without succeeding before treating the task as ready again.
// streakCount <= 0 (never failed, or succeeded most recently) needs no
// wait at all.
func retryBackoff(streakCount int) time.Duration {
	if streakCount <= 0 {
		return 0
	}
	backoff := baseRetryBackoff * time.Duration(uint64(1)<<uint(streakCount-1))
	if backoff > maxRetryBackoff || backoff <= 0 {
		return maxRetryBackoff
	}
	return backoff
}

// retryEligible reports whether taskID's own failure history lets it be
// dispatched at now -- true outright for a task that has never finished a
// run, or whose most recent one succeeded, and otherwise true only once
// retryBackoff's own wait has elapsed since that run finished.
func retryEligible(ctx context.Context, store *model.Store, taskID string, now time.Time) (bool, error) {
	streak, err := store.FailureStreak(ctx, taskID)
	if err != nil {
		return false, fmt.Errorf("checking %s's failure streak: %w", taskID, err)
	}
	if streak == nil || streak.Count == 0 {
		return true, nil
	}
	return !now.Before(streak.LastFinishedAt.Add(retryBackoff(streak.Count))), nil
}

// Option is one caller's adjustment to a single Cycle call. Variadic
// rather than a parameter of its own because the overwhelming majority
// of callers -- every test that drives a cycle against a store, and
// every one-shot cycle -- want none of it, and a nil argument at every
// one of those call sites would say less than their absence does.
type Option func(*options)

type options struct {
	busy func(taskID string) bool
}

// Busy tells Cycle to pass over any task busy reports true for, without
// spending capacity on it -- exactly the way a task still backing off
// after a failure is passed over (retryEligible).
//
// It exists for one thing the store cannot answer on its own: a run's
// row is finished (Store.FinishRun, inside RunDispatch) a moment before
// the caller has turned that run's result into the effects it implies --
// the observation that says the task completed, the pull request it
// opened. In between, task_state sees no live run and no completion, so
// the task reads 'queued' and this package would dispatch it a second
// time, running the same work twice over. Nothing was ever wrong with
// the view: what it lacks is knowledge that only the process still
// holding that result has, which is what this option lets that process
// supply (orchestrator.InFlight.Busy).
//
// It closed no gap while a cycle waited for the runs it started -- there
// was no tick to fall in that window -- and it is required now that a
// run outlives its cycle (orchestrator.InFlight's own doc comment).
//
// A caller that passes this is saying only "not yet, not from me": a
// task passed over here is dispatched by a later cycle as soon as busy
// stops saying so, on whatever the store says about it then -- and on
// what the store says *then*, not on what it said at the top of the
// cycle, which is what stillReady is for. The two are one guard: busy
// covers the window while the result is being applied, and the readiness
// re-read covers a cycle that entered that window before it and left it
// after it.
func Busy(busy func(taskID string) bool) Option {
	return func(o *options) { o.busy = busy }
}

func newOptions(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// skip reports whether taskID is one the caller has asked Cycle to pass
// over this time round.
func (o options) skip(taskID string) bool {
	return o.busy != nil && o.busy(taskID)
}

// Cycle is one pass: start the next ready, backed-off tasks in
// task_ready's own order until limits admits no more of either kind, and
// start nothing else. It is the entire dispatch decision for now — no
// polling, no completion detection, no side effect beyond the store
// writes StartRun already makes durable.
//
// limits is the whole limit, not the remaining headroom. Cycle works out
// how much of it is already spent itself, from the store, on every call,
// rather than trusting a caller's idea of it left over from the last one
// — a run can finish and free capacity with nothing about this cycle's
// caller changing, the same "re-read, never pin" discipline IsBlocked's
// docstring argues for and for the same reason.
//
// Those counts are read once, up front, and then spent down as this loop
// starts runs; they are not the thing that enforces the limit. StartRun
// re-checks them inside the transaction that records each run, and
// returns model.ErrAtCapacity if another caller took the last of the
// headroom in between -- which Cycle treats as "no more room this tick"
// and stops on, returning what it did manage to start. See StartRun's own
// doc comment: the check has to happen there to be a check at all, and
// this one exists only to avoid asking for capacity that is obviously not
// there.
//
// Which of the two limits a ready task spends is model.Limits' own
// distinction: a merge-queue fix task is a merger (Store.ReadyMergers),
// everything else a worker. A candidate whose own kind is full is passed
// over rather than stopping the cycle, exactly the way one still backing
// off is, since the other kind may still have room and the ready order is
// not a queue of interchangeable items any more. The cycle stops when
// nothing of either kind could start.
//
// Limits that bound nothing (model.Limits.Unlimited -- the zero value)
// mean "nothing may start" here, the opposite of what StartRun reads them
// as. That asymmetry is deliberate and predates the split: a cycle whose
// deployment has configured no capacity at all has nothing it could
// sensibly start, while a caller reaching StartRun directly with no
// limits is saying it has none to enforce.
//
// A task already running never appears in Ready — task_ready requires
// state = 'queued', and a live run makes the state 'running' — so Cycle
// needs no check of its own to avoid dispatching one twice; that
// invariant lives in the view, not here. A task still backing off after a
// failed run does appear in Ready (task_ready has no notion of time), so
// Cycle itself skips it via retryEligible -- without consuming the
// capacity it would otherwise have taken, so a task further down the
// ready order is not made to wait behind one that is not actually
// eligible yet.
//
// What the view cannot do on its own is stay true for the length of a
// cycle: Ready is read once, up front, and a run of a candidate further
// down that list can end while the cycle is still working through it.
// Each candidate's readiness is therefore re-read immediately before its
// run is recorded (stillReady, whose doc comment has the interleaving),
// so a task the caller finished with mid-cycle is passed over rather
// than started a second time.
func Cycle(ctx context.Context, store *model.Store, limits model.Limits, now time.Time, opts ...Option) ([]Dispatch, error) {
	o := newOptions(opts)
	// The configuration agent (Task.Configuration, bwsalmon/agents#621)
	// dispatches first, and unconditionally -- see dispatchConfiguration's
	// own doc comment for why it cannot wait on the same headroom check
	// below. Doing this before LiveRunCounts is read is what makes the
	// free-capacity math that follows accurate for everything else: a
	// configuration task started here already counts as live by the time
	// this function asks.
	out, err := dispatchConfiguration(ctx, store, now, o)
	if err != nil {
		return nil, err
	}

	if limits.Total() <= 0 {
		return out, nil
	}
	live, err := store.LiveRunCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("dispatch: counting live runs: %w", err)
	}

	ready, err := store.Ready(ctx)
	if err != nil {
		return nil, fmt.Errorf("dispatch: reading ready tasks: %w", err)
	}
	mergerIDs, err := store.ReadyMergers(ctx)
	if err != nil {
		return nil, fmt.Errorf("dispatch: reading ready merge-queue tasks: %w", err)
	}
	mergers := make(map[string]bool, len(mergerIDs))
	for _, id := range mergerIDs {
		mergers[id] = true
	}

	for _, candidate := range ready {
		if !limits.Admits(live, true) && !limits.Admits(live, false) {
			break
		}
		if o.skip(candidate) {
			continue
		}
		merger := mergers[candidate]
		if !limits.Admits(live, merger) {
			continue
		}
		eligible, err := retryEligible(ctx, store, candidate, now)
		if err != nil {
			return nil, fmt.Errorf("dispatch: %w", err)
		}
		if !eligible {
			continue
		}
		still, err := stillReady(ctx, store, candidate)
		if err != nil {
			return nil, err
		}
		if !still {
			continue
		}

		d, err := startTask(ctx, store, candidate, now, limits)
		if err != nil {
			if errors.Is(err, model.ErrAtCapacity) {
				return out, nil
			}
			return nil, fmt.Errorf("dispatch: starting run for %s: %w", candidate, err)
		}
		out = append(out, d)
		live = live.Add(merger)
	}
	return out, nil
}

// dispatchConfiguration starts every configuration-agent task
// (Store.ReadyConfiguration) that is ready and not still backing off
// after a failed run, regardless of how much of this deployment's
// capacity is already spent -- it calls startTask with zero limits, which
// StartRun takes to mean "no limit of mine to enforce" (its own doc
// comment).
//
// The configuration agent is what bwsalmon/agents#621 added for a person
// to reach for when something about this deployment needs a live look --
// a question, a problem, or its own configuration -- and a deployment
// already at its concurrency limit is exactly the kind of "something"
// that might be. Making it wait behind the same headroom check as
// ordinary work would strand it at the one moment it is most likely to
// be needed.
func dispatchConfiguration(ctx context.Context, store *model.Store, now time.Time, o options) ([]Dispatch, error) {
	ready, err := store.ReadyConfiguration(ctx)
	if err != nil {
		return nil, fmt.Errorf("dispatch: reading ready configuration tasks: %w", err)
	}
	var out []Dispatch
	for _, taskID := range ready {
		if o.skip(taskID) {
			continue
		}
		eligible, err := retryEligible(ctx, store, taskID, now)
		if err != nil {
			return nil, fmt.Errorf("dispatch: %w", err)
		}
		if !eligible {
			continue
		}
		still, err := stillReady(ctx, store, taskID)
		if err != nil {
			return nil, err
		}
		if !still {
			continue
		}
		d, err := startTask(ctx, store, taskID, now, model.Limits{})
		if err != nil {
			return nil, fmt.Errorf("dispatch: starting configuration run for %s: %w", taskID, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// stillReady is the last look before a run is recorded: whether taskID
// is *still* what the ready list said it was when this cycle read it
// (Store.IsReady).
//
// A cycle reads task_ready once and then spends real time on each
// candidate -- a busy check, a streak read, a run started for whatever
// came earlier in the order -- and the run of a candidate can end during
// any of it. When it does, the store's answer changes twice in quick
// succession: the run's row is finished first (the task reads 'queued'
// again, which is the window Busy covers), and the completion its result
// implies is recorded a moment later, after which the task is not ready
// at all. A cycle that read the list before the second write and asked
// busy after the caller had finished with the task saw "queued" and "not
// busy" -- both true when they were read, neither true together -- and
// started a second run of a task that was already done.
//
// Ordering is the whole of why this works: busy going false is the
// caller saying it has finished turning that run's result into what it
// implies, so a readiness read taken *after* that (this one, at the end
// of the loop body, rather than the bulk read at the top) sees every
// effect of the run. What is left is the ordinary race any two processes
// share -- a human closing the task in the instant before StartRun's own
// transaction -- which is not this one, and which costs a run nobody
// asked for rather than a duplicate of one already made.
func stillReady(ctx context.Context, store *model.Store, taskID string) (bool, error) {
	ready, err := store.IsReady(ctx, taskID)
	if err != nil {
		return false, fmt.Errorf("dispatch: re-reading whether %s is still ready: %w", taskID, err)
	}
	return ready, nil
}

// startTask records taskID's next run (Store.Attempts + 1) and starts
// it, against limits the same way StartRun itself interprets them --
// limits that bound nothing disable the check entirely. Factored out of
// Cycle's own loop so dispatchConfiguration can share it rather than
// duplicate the attempt bookkeeping.
func startTask(ctx context.Context, store *model.Store, taskID string, now time.Time, limits model.Limits) (Dispatch, error) {
	attempts, err := store.Attempts(ctx, taskID)
	if err != nil {
		return Dispatch{}, fmt.Errorf("counting attempts for %s: %w", taskID, err)
	}
	attempt := attempts + 1
	// Sandbox is deliberately left empty: no sandbox exists for this run
	// yet, and inventing a name here for one nothing has built would be a
	// claim this package is in no position to make. The orchestrator
	// acquires one and records it (Store.SetRunSandbox).
	run := model.Run{
		ID:        RunID(taskID, attempt),
		TaskID:    taskID,
		Attempt:   attempt,
		StartedAt: now,
	}
	if err := store.StartRun(ctx, run, limits); err != nil {
		return Dispatch{}, err
	}
	return Dispatch{TaskID: taskID, RunID: run.ID, Attempt: attempt}, nil
}
