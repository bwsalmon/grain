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
// There is no scheduling policy here, deliberately: no priority, no
// fairness, no preemption, no notion of time beyond the now a caller
// passes in. Ordering is whatever task_ready yields (Store.Ready sorts
// by task ID, a stable tiebreak rather than a policy), and Cycle takes
// its prefix. A package that ranked ready tasks against each other
// would be a scheduler; this one drains a queue into free slots.
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

// Cycle is one pass: fill every free slot with the next ready task, in
// task_ready's own order, and start nothing else. It is the entire
// dispatch decision for now — no polling, no completion detection, no
// side effect beyond the store writes StartRun already makes durable.
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
// invariant lives in the view, not here.
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
	for i, slot := range free {
		if i >= len(ready) {
			break
		}
		taskID := ready[i]

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
