package model

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
