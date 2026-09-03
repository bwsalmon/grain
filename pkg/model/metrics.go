package model

// The store's read surface for measurement: every moment a task and its
// attempts passed through, in the flattest shape a report can be computed
// from. Nothing here is a new record -- every column read below is
// already written by the paths that own it (Client.CreateTask,
// Store.Approve, Store.StartRun/SetRunAgentStarted/FinishRun,
// orchestrator's own Observe) -- which is the whole point: throughput and
// latency are derivations of what already happened, so measuring them
// adds nothing to store and nothing to keep in sync.
//
// pkg/metrics turns these two slices into a report. The split is
// deliberate: reading rows is this package's job, and deciding what a
// percentile means is not.

import (
	"context"
	"database/sql"
	"time"
)

// TaskTiming is one task's own timeline, flattened to the four moments a
// report measures between: filed, queued, completed, closed. It is not a
// Task -- nothing here needs a title, a target or a grant -- and it is
// deliberately not a state either: StateOf's answer is a snapshot of now,
// where these are the moments that snapshot moved.
//
// CreatedAt and ApprovedAt are both nilable because both genuinely are
// (Task.CreatedAt, Task.ApprovedAt): a task filed before either column
// existed has no such moment to report, which a report has to skip rather
// than guess at.
type TaskTiming struct {
	TaskID      string
	Reason      OriginReason
	CreatedAt   *time.Time
	ApprovedAt  *time.Time
	CompletedAt *time.Time
	ClosedAt    *time.Time
}

// RunTiming is one attempt's own timeline. AgentStartedAt splits it in
// two -- setup before, agent work after -- and is nil for a run still in
// setup and for one that never reached its agent at all (see schema.go on
// task_run.agent_started_at).
type RunTiming struct {
	TaskID         string
	Attempt        int
	StartedAt      time.Time
	AgentStartedAt *time.Time
	FinishedAt     *time.Time
	Outcome        string
}

// TaskTimings is every task's timeline, oldest id first.
//
// It reads the whole table rather than taking a window, and so does
// RunTimings, for a reason that is about correctness before it is about
// size: a report's window bounds which *measurements* it covers, not
// which rows it may look at. A task completed this morning may have been
// filed last month, and the latency worth reporting is exactly that
// distance; a run overlapping the window contributes to its occupancy
// whether or not it started inside it. Filtering rows by the window in
// SQL would drop precisely the samples that make the window interesting.
//
// The cost is one full scan of task and task_run per report, which is
// what a single-operator deployment can afford (Store.ListTasks already
// reads every task for every list) and what a much larger one would have
// to revisit -- a materialised roll-up, or a window widened at the query
// rather than in Go.
func (s *Store) TaskTimings(ctx context.Context) ([]TaskTiming, error) {
	var out []TaskTiming
	err := each(ctx, s.db,
		"SELECT `t`.`id`,`t`.`origin_reason`,`t`.`created_at`,`t`.`approved_at`,"+
			"`o`.`completed_at`,`o`.`closed_at` "+
			"FROM `task` AS `t` "+
			"LEFT JOIN `task_observation` AS `o` ON `o`.`task_id` = `t`.`id` "+
			"ORDER BY `t`.`id`", nil,
		func(rows *sql.Rows) error {
			var t TaskTiming
			var reason string
			var created, approved, completed, closed sql.NullTime
			if err := rows.Scan(&t.TaskID, &reason, &created, &approved, &completed, &closed); err != nil {
				return err
			}
			t.Reason = OriginReason(reason)
			t.CreatedAt, t.ApprovedAt = timePtr(created), timePtr(approved)
			t.CompletedAt, t.ClosedAt = timePtr(completed), timePtr(closed)
			out = append(out, t)
			return nil
		})
	return out, err
}

// RunTimings is every attempt ever recorded, oldest first per task -- see
// TaskTimings on why this is the whole table.
func (s *Store) RunTimings(ctx context.Context) ([]RunTiming, error) {
	var out []RunTiming
	err := each(ctx, s.db,
		"SELECT `task_id`,`attempt`,`started_at`,`agent_started_at`,`finished_at`,`outcome` "+
			"FROM `task_run` ORDER BY `task_id`, `attempt`", nil,
		func(rows *sql.Rows) error {
			var r RunTiming
			var agentStarted, finished sql.NullTime
			var outcome sql.NullString
			if err := rows.Scan(&r.TaskID, &r.Attempt, &r.StartedAt,
				&agentStarted, &finished, &outcome); err != nil {
				return err
			}
			r.AgentStartedAt, r.FinishedAt = timePtr(agentStarted), timePtr(finished)
			r.Outcome = outcome.String
			out = append(out, r)
			return nil
		})
	return out, err
}
