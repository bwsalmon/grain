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

// PausedOutcome is the task_run.outcome of a run grain itself stopped
// because the agent's own usage limit was reached -- the credential it
// runs as having no budget left in its current window
// (agent.UsageLimitError, orchestrator.Pause). One word, named here
// rather than in the package that writes it, because two readers have to
// agree on it: task_streak (schema.go) and Store.FailureStreak, both of
// which skip it.
//
// Skipping is the whole reason it is not simply "cancelled". Every other
// non-"succeeded" ending is evidence about the task -- its agent failed,
// its sandbox would not build, its result could not be turned into a
// pull request -- and counting those toward MaxConsecutiveFailures is
// what stops a task being retried forever. A usage limit is evidence
// about the deployment: it says nothing whatsoever about the task, it
// arrives at whatever tasks happen to be running when the window runs
// out, and it would otherwise spend the retry budget of every one of
// them on an outage none of them caused. The same argument
// orchestrator.outcomeOf makes for no longer reading an errored tool
// call as a failed run.
const PausedOutcome = "paused"

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
		case obs.CompletedAt != nil && AwaitsSubmit(task):
			return StateAwaitingSubmit
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

// AwaitsSubmit reports whether a task that has finished running is
// waiting on a human's Submit click rather than on the merge queue --
// the condition that separates StateAwaitingSubmit from StateCompleted.
// It says nothing about whether the run is over: StateOf is what pairs
// it with Observation.CompletedAt, and this deliberately never reads an
// Observation so a caller holding a Task alone can still ask.
//
// Two halves, both necessary. AutoMerge is what Submit sets
// (ui.Client.Submit reuses it rather than adding a second field), so an
// already-submitted task is on the queue and waiting on nobody. And a
// task with no LinkFixes pull request has nothing to submit at all --
// an analyze task that answered in a comment is finished, not parked,
// and offering "Submit" as its next step would name a button it will
// never grow.
//
// Nothing here asks whether the pull request is still open. A merged or
// closed one closes the task itself (orchestrator.recordPullRequestEvents
// sets ClosedAt), and StateClosed already outranks this in StateOf.
func AwaitsSubmit(task Task) bool {
	if task.AutoMerge {
		return false
	}
	for _, l := range task.Links {
		if l.Kind == LinkFixes {
			return true
		}
	}
	return false
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
	// Guarded the same way StateOf itself guards it: obs.CompletedAt and
	// obs.ClosedAt outrank a failure streak there, so once either is set
	// the task can never again read 'failed' no matter what task_streak
	// says. Transitions has to apply the same precedence rather than
	// adding StateFailed off streak.Count alone, because a task can reach
	// StateCompleted without task_streak ever resetting: a run salvaged
	// into a pull request after erroring keeps its own outcome "failed"
	// forever (orchestrator.salvagePushedBranch never corrects it -- see
	// orchestrator.RunCycle's "only the ending failed"), so streak.Count
	// can sit at or above MaxConsecutiveFailures permanently for a task
	// that has, in every other respect, completed. Left unguarded, every
	// later read of this task's timeline showed a bogus "Failed" entry
	// immediately before "Completed", forever, even though the task
	// plainly succeeded (bwsalmon/agents#502).
	if obs == nil || (obs.CompletedAt == nil && obs.ClosedAt == nil) {
		if streak != nil && streak.Count >= MaxConsecutiveFailures {
			lastFinishedAt := streak.LastFinishedAt
			add(StateFailed, &lastFinishedAt)
		}
	}
	if obs != nil {
		// One moment, one entry, under whichever of the two names StateOf
		// would give it right now. The record holds no separate "and then
		// somebody submitted it" timestamp -- AutoMerge is a field on the
		// declaration with no history of its own -- so a task submitted
		// after the fact shows its completion as 'completed' and one still
		// parked shows the same moment as 'awaiting_submit', rather than
		// the timeline ending on a state the badge above it disagrees with.
		completedState := StateCompleted
		if AwaitsSubmit(task) {
			completedState = StateAwaitingSubmit
		}
		add(completedState, obs.CompletedAt)
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
