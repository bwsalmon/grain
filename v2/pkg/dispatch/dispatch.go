// Package dispatch decides which task takes which slot: what one
// controller cycle assigns, and nothing more. It does not loop — a
// deployment's own timer does, through orchestrator.RunCycle, which
// calls Cycle once per tick.
//
// Cycle calls StartRun and nothing else — no sandbox is created, no
// GitHub is touched, no agent runs. Those are side effects bwsalmon/
// agents#219 deliberately defers: this package exists to pin down that
// the *decisions* — which task takes which slot, when, and how often —
// are correct on their own, against a real store, before anything
// external is wired to react to them. pkg/orchestrator is the
// side-effecting counterpart that grew around it, and nothing here had
// to change shape when it did, since a Dispatch already says everything
// that side effect needs to know.
//
// There is almost no scheduling policy here: no fairness, no preemption.
// Ordering is whatever task_ready yields, and Cycle takes its prefix,
// skipping over (never reordering past its own turn) a task still backing
// off after a recently failed run -- see retryEligible. The one other
// exception is Store.Ready's own: a fix task the merge queue filed for a
// repo's stuck head sorts before ordinary ready tasks (task ID is still
// the tiebreak within each group), which is a fact about task_ready
// rather than a policy Cycle itself makes -- see Store.Ready's doc
// comment (bwsalmon/agents#389) for why. A package that ranked ready
// tasks against each other on some richer notion of priority would be a
// scheduler; this one drains a queue into free slots.
package dispatch

import (
	"context"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Dispatch is one decision a cycle made: task TaskID was started in slot
// Slot, as its Attempt'th run. The store write is already durable by the
// time a Dispatch is returned — this is a record of what happened, for a
// side-effecting layer to act on, not a request for one to decide.
type Dispatch struct {
	TaskID  string
	Slot    string
	RunID   string
	Attempt int
}

// RunID names a run deterministically from its task and attempt number,
// the same reasoning as task.go's BranchName: two callers computing a
// run's name must agree without coordinating, and a name that already
// encodes its own attempt is self-describing in a log with nothing to
// look up.
func RunID(taskID string, attempt int) string {
	return fmt.Sprintf("%s-r%d", taskID, attempt)
}

// baseRetryBackoff and maxRetryBackoff bound how long Cycle waits after a
// task's run ends without succeeding before offering it a free slot
// again: baseRetryBackoff after the first such run in a row, doubling
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

// Cycle is one pass: fill every free slot with the next ready, backed-off
// task, in task_ready's own order, and start nothing else. It is the
// entire dispatch decision for now — no polling, no completion detection,
// no side effect beyond the store writes StartRun already makes durable.
//
// slots is the whole concurrency pool, not just the free ones. Cycle
// works out what is occupied itself, from the store, on every call,
// rather than trusting a caller's idea of it left over from the last
// one — a run can finish and free a slot with nothing about this cycle's
// caller changing, the same "re-read, never pin" discipline IsBlocked's
// docstring argues for and for the same reason.
//
// A task already running never appears in Ready — task_ready requires
// state = 'queued', and a live run makes the state 'running' — so Cycle
// needs no check of its own to avoid dispatching one twice; that
// invariant lives in the view, not here. A task still backing off after a
// failed run does appear in Ready (task_ready has no notion of time), so
// Cycle itself skips it via retryEligible -- without consuming the free
// slot it would otherwise have taken, so a task further down the ready
// order is not made to wait behind one that is not actually eligible yet.
func Cycle(ctx context.Context, store *model.Store, slots []string, now time.Time) ([]Dispatch, error) {
	occupied, err := store.OccupiedSlots(ctx)
	if err != nil {
		return nil, fmt.Errorf("dispatch: reading occupied slots: %w", err)
	}
	busy := make(map[string]bool, len(occupied))
	for _, s := range occupied {
		busy[s] = true
	}
	var free []string
	for _, s := range slots {
		if !busy[s] {
			free = append(free, s)
		}
	}
	if len(free) == 0 {
		return nil, nil
	}

	ready, err := store.Ready(ctx)
	if err != nil {
		return nil, fmt.Errorf("dispatch: reading ready tasks: %w", err)
	}

	var out []Dispatch
	readyIdx := 0
	for _, slot := range free {
		var taskID string
		for {
			if readyIdx >= len(ready) {
				return out, nil
			}
			candidate := ready[readyIdx]
			readyIdx++
			eligible, err := retryEligible(ctx, store, candidate, now)
			if err != nil {
				return nil, fmt.Errorf("dispatch: %w", err)
			}
			if eligible {
				taskID = candidate
				break
			}
		}

		attempts, err := store.Attempts(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("dispatch: counting attempts for %s: %w", taskID, err)
		}
		attempt := attempts + 1
		run := model.Run{
			ID:        RunID(taskID, attempt),
			TaskID:    taskID,
			Slot:      slot,
			Sandbox:   slot,
			Attempt:   attempt,
			StartedAt: now,
		}
		if err := store.StartRun(ctx, run); err != nil {
			return nil, fmt.Errorf("dispatch: starting run for %s: %w", taskID, err)
		}
		out = append(out, Dispatch{TaskID: taskID, Slot: slot, RunID: run.ID, Attempt: attempt})
	}
	return out, nil
}
