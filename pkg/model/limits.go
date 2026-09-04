package model

// Limits is how many agents a deployment may have in flight at once,
// split into the two kinds of work an agent is dispatched for
// (grain/task-63).
//
// A *merger* is a run that repairs a pull request that will not land --
// a task the merge queue has sent back to work on its own branch
// (Observation.MergeQueueRepairAt), or one of the separate fix tasks it
// used to file for that (Origin.Reason == ReasonFix). Store.mergerTaskSQL
// is the one definition. A *worker* is a run of anything else: the
// ordinary backlog, a schedule, a qualification, a suite pass, a task
// somebody just filed.
//
// The split exists because the two compete for the same capacity while
// being worth very different amounts. A merger is the last step of work
// that is already done -- committed, pushed, reviewed -- and the longer
// it waits the more likely something else lands on the branch it repairs
// and the fix has to be filed again (Store.Ready's own doc comment on
// why a fix task sits at the head of the backlog). A deployment whose
// every slot was spent on new work would starve exactly the runs that
// finish the old, so Mergers is capacity kept back for them.
//
// Kept back, not walled off: the two numbers are not symmetric.
//
//   - No more than Workers runs of ordinary work are ever live.
//   - No more than Workers+Mergers runs are ever live at all.
//
// A merger is bounded only by the second rule, so it may take a worker's
// slot when one is free -- Workers 3 and Mergers 2 admits five mergers,
// but never a fourth worker, and never a sixth run of either kind. That
// asymmetry is the whole point: work that is nearly landed may borrow
// capacity from work not yet started, and never the other way round.
//
// The zero value enforces nothing at all (Unlimited), which is what a
// caller with no limit of its own to apply passes -- see StartRun.
type Limits struct {
	// Workers is the ceiling on ordinary runs, and the floor under how
	// much of the total a merger can ever be denied.
	Workers int
	// Mergers is the extra capacity only a merge-queue fix task may
	// reach. 0 -- a meaningful value, not an unset one -- means mergers
	// contend for Workers alongside everything else, which is what every
	// deployment did before this split existed.
	Mergers int
}

// Total is the ceiling on live runs of both kinds together: the number
// neither a worker nor a merger may push past.
func (l Limits) Total() int { return l.Workers + l.Mergers }

// Unlimited reports whether these limits bound nothing -- the zero
// value, or anything else with no positive capacity of either kind.
// StartRun reads it as "the caller has no limit for me to enforce"
// rather than as "admit nothing"; dispatch.Cycle takes the opposite
// reading of the same state, deliberately, since a cycle with no
// capacity configured has nothing it could sensibly start (its own doc
// comment).
func (l Limits) Unlimited() bool { return l.Workers <= 0 && l.Mergers <= 0 }

// Admits reports whether one more run may start with live already in
// flight -- a merger when merger is true, an ordinary worker otherwise.
//
// This is the whole rule these two numbers make, in one place, so that
// the check dispatch.Cycle makes before it asks for capacity and the
// check StartRun makes inside the transaction that records the run
// cannot drift apart. See StartRun on why the second one is the one that
// actually enforces anything.
func (l Limits) Admits(live RunCounts, merger bool) bool {
	if l.Unlimited() {
		return true
	}
	if live.Total() >= l.Total() {
		return false
	}
	if merger {
		return true
	}
	return live.Workers < l.Workers
}

// RunCounts is how many runs are live right now, of each kind Limits
// bounds -- Store.LiveRunCounts' own answer, and the tally
// dispatch.Cycle spends down as it starts runs.
type RunCounts struct {
	Workers int
	Mergers int
}

// Total is how many runs are live regardless of kind -- what
// Store.LiveRunCount returns on its own.
func (c RunCounts) Total() int { return c.Workers + c.Mergers }

// Add counts one more live run of the given kind.
func (c RunCounts) Add(merger bool) RunCounts {
	if merger {
		c.Mergers++
		return c
	}
	c.Workers++
	return c
}

// Merger reports whether a run of a task with this origin reason counts
// against the merger half of Limits by its reason alone: a task the merge
// queue filed to repair another task's branch (ReasonFix) and nothing
// else.
//
// It is no longer the whole of the question. A repair now runs as another
// attempt of the task whose branch is broken (Observation.
// MergeQueueRepairAt), whose reason is whatever it was filed as, so the
// store answers "is this run a merger" with both halves at once
// (mergerTaskSQL) and nothing here reads this. It stays as the reason
// half's own name, for a caller holding an origin and no database.
func (r OriginReason) Merger() bool { return r == ReasonFix }
