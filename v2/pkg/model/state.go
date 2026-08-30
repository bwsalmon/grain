package model

import (
	"sort"
	"time"
)

// MaxConsecutiveFailures is how many of a task's most recent runs, in a
// row, may end without succeeding before StateOf stops calling it
// 'queued' and starts calling it 'failed' -- see StateFailed and
// task_streak (schema.go), which apply the same cutoff to
// task_run.outcome so a task cannot be retried forever with no cap and
// no visible signal (bwsalmon/agents#403).
//
// A fixed constant rather than a grain_config knob: the tasks this
// bounds are already the ones producing nothing useful attempt after
// attempt, so getting the exact number right matters far less than
// having a number at all. Fold it into grain_config the day an operator
// actually needs a different one.
const MaxConsecutiveFailures = 5

// StateOf computes a task's state. Never stored, never written.
//
// Order is precedence, not preference: a completed task whose issue was
// then closed reads closed, and a task with a live run reads running
// whatever its approval says.
//
// failureStreak is task_streak's own count (Store.FailureStreak.Count) --
// passed in rather than computed here because it takes a database query
// over task_run to answer, which is exactly what this function's own
// contract ("available to code holding a Task and no database") rules
// out doing itself.
//
// schema.go computes the same thing as a SQL view. The duplication is
// deliberate — a view is what makes the invariant structural in the
// store, and this is what makes it available to code holding a Task and
// no database — and state_test.go holds the two to the same precedence,
// because two implementations of one rule drift silently: the symptom
// would be a wrong state only when two conditions are true at once.
func StateOf(task Task, obs *Observation, activeRun bool, failureStreak int) State {
	if obs != nil {
		switch {
		case obs.ClosedAt != nil:
			return StateClosed
		case obs.CompletedAt != nil:
			return StateCompleted
		case obs.PendingQuestionCommentID != nil:
			return StateAwaitingReply
		}
	}
	if activeRun {
		return StateRunning
	}
	if failureStreak >= MaxConsecutiveFailures {
		return StateFailed
	}
	if task.Approval == nil {
		return StateProposed
	}
	return StateQueued
}

// Transition is one point where a task's derived state changed --
// StateOf's own answer, not a value anything stores.
type Transition struct {
	State State
	At    time.Time
}

// Transitions reconstructs, as far as the record allows, when a task
// passed through each state in State's vocabulary -- the history behind
// StateOf's single snapshot, for a UI that wants to show "-> queued at
// 10:04, -> running at 10:06" rather than only the pill on top.
//
// Like StateOf, this reads what it is handed and touches no database;
// unlike StateOf, it is necessarily incomplete in one place. Observation
// only ever holds the *pending* question (Client.AddComment clears
// PendingQuestionCommentID the moment a human replies rather than
// keeping a record of the one that was just answered), so a past
// awaiting_reply period a task has already moved on from leaves no
// timestamp behind -- askedAt is that comment's own CreatedAt while the
// question is still outstanding, and nil once it is not, the same as
// Observation.PendingQuestionCommentID itself.
//
// runs must be oldest first, the order Store.Runs already returns, and
// streak must be the same one Store.FailureStreak would give this task
// right now. StateFailed's own moment is streak.LastFinishedAt rather
// than a replay of task_streak's reset rules against the run list: sound
// because task_ready never selects a failed task, so the streak that
// tripped StateFailed can never grow past the run that tripped it.
func Transitions(task Task, obs *Observation, runs []Run, streak *FailureStreak, askedAt *time.Time) []Transition {
	var out []Transition
	add := func(s State, at *time.Time) {
		if at != nil {
			out = append(out, Transition{State: s, At: *at})
		}
	}

	add(StateProposed, task.CreatedAt)
	add(StateQueued, task.ApprovedAt)

	activeRun := false
	for i := range runs {
		r := &runs[i]
		add(StateRunning, &r.StartedAt)
		if r.FinishedAt == nil {
			activeRun = true
			continue
		}
		// An attempt that was not the last one proves the task queued
		// again afterward -- nothing else could have dispatched the next
		// one. The last attempt's own finish is only a requeue if nothing
		// terminal claimed it, which StateOf (fed this same run list's
		// tail) already knows how to decide.
		if i < len(runs)-1 {
			add(StateQueued, r.FinishedAt)
		}
	}

	failureCount := 0
	if streak != nil {
		failureCount = streak.Count
	}
	if current := StateOf(task, obs, activeRun, failureCount); current == StateQueued && len(runs) > 0 {
		if last := runs[len(runs)-1].FinishedAt; last != nil {
			add(StateQueued, last)
		}
	}

	add(StateAwaitingReply, askedAt)
	if streak != nil && streak.Count >= MaxConsecutiveFailures {
		lastFinishedAt := streak.LastFinishedAt
		add(StateFailed, &lastFinishedAt)
	}
	if obs != nil {
		add(StateCompleted, obs.CompletedAt)
		add(StateClosed, obs.ClosedAt)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// IsBlocked reports whether any blocking link names a task that has not
// closed.
//
// closed is passed in rather than looked up because whether a dependency
// is still open changes with nothing about this task changing — which is
// why this is re-evaluated every cycle rather than pinned at dispatch,
// the one deliberate exception to "decide once, never re-read".
func IsBlocked(task Task, closed map[string]bool) bool {
	for _, link := range task.Links {
		if link.Kind.Blocks() && !closed[link.Target] {
			return true
		}
	}
	return false
}
