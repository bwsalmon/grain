package model

// StateOf computes a task's state. Never stored, never written.
//
// Order is precedence, not preference: a completed task whose issue was
// then closed reads closed, and a task with a live run reads running
// whatever its approval says.
//
// schema.go computes the same thing as a SQL view. The duplication is
// deliberate — a view is what makes the invariant structural in the
// store, and this is what makes it available to code holding a Task and
// no database — and state_test.go holds the two to the same precedence,
// because two implementations of one rule drift silently: the symptom
// would be a wrong state only when two conditions are true at once.
func StateOf(task Task, obs *Observation, activeRun bool) State {
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
