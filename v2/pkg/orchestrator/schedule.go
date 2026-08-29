package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// scheduler is the automation principal every task a schedule files is
// attributed to -- its own actor, distinct from "grain" (relayed agent
// output) and "merge-queue" (automatic PR repair), so a human reading a
// task's conversation or origin can tell which of grain's own mechanisms
// filed it.
var scheduler = model.Principal{Kind: model.PrincipalAutomation, ID: "schedule"}

// reconcileSchedule fires every schedule whose interval has come due.
//
// One schedule failing to file (or a store error while checking it)
// does not stop the others -- Reconciler's own doc comment on why each
// one re-reads the store every cycle applies again one level down:
// a schedule this cycle could not fire is one next cycle tries again,
// exactly as it stood before.
func reconcileSchedule(ctx context.Context, deps Deps, now time.Time) error {
	due, err := deps.Store.DueScheduledTasks(ctx, now)
	if err != nil {
		return fmt.Errorf("orchestrator: listing due scheduled tasks: %w", err)
	}
	var errs []error
	for _, sched := range due {
		if err := fireScheduledTask(ctx, deps.Store, sched, now); err != nil {
			errs = append(errs, fmt.Errorf("orchestrator: firing scheduled task %s: %w", sched.ID, err))
		}
	}
	return errors.Join(errs...)
}

// firingTag is the idempotency marker every task a schedule files
// carries -- v1's scheduled_jobs.py marker_label, ported onto
// model.Task.Tags per docs/data-model.md ("neither a state nor a
// capability: it is an idempotency tag"). fireScheduledTask checks it
// before filing again, so a schedule whose previous firing is still
// running (or awaiting a reply, or otherwise unclosed) does not pile up
// a second one on top of it.
func firingTag(scheduleID string) string { return "schedule:" + scheduleID }

// fireScheduledTask files sched's next task, unless its previous firing
// has not finished yet -- in which case this is a no-op, tried again
// next cycle with NextRunAt left exactly where it was, so the check
// costs nothing and cannot skip a firing outright, only delay it.
//
// The filed task lands already approved: docs/data-model.md's "the
// SCHEDULED special case dissolves" is why -- a schedule is itself a
// human's standing approval, written once and editable like any other
// declaration, so a firing needs no approval flag of its own the way
// fileFixTask's own automatic fix needs none either.
func fireScheduledTask(ctx context.Context, store *model.Store, sched model.ScheduledTask, now time.Time) error {
	tag := firingTag(sched.ID)
	open, err := store.HasOpenTaskWithTag(ctx, tag)
	if err != nil {
		return fmt.Errorf("checking for a previous unfinished firing: %w", err)
	}
	if open {
		return nil
	}

	id, err := store.NewTaskID(ctx)
	if err != nil {
		return fmt.Errorf("allocating an id for %q: %w", sched.Title, err)
	}
	target := sched.Target
	task := model.Task{
		ID:     id,
		Intent: model.IntentImplement,
		Title:  sched.Title,
		Body:   sched.Body,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: scheduler},
			Reason:      model.ReasonSchedule,
		},
		Approval:  &model.Attribution{Actor: scheduler},
		Target:    &target,
		Binding:   model.BindingDirective,
		Base:      sched.Base,
		AutoMerge: sched.AutoMerge,
		Tags:      []string{tag},
		CreatedAt: &now,
	}
	if err := store.PutTask(ctx, task); err != nil {
		return fmt.Errorf("filing task: %w", err)
	}

	// Interval is validated positive at creation (ui.Client.CreateSchedule/
	// UpdateSchedule) and never zero or negative in a row this reads back
	// -- the fallback below only guards against that invariant somehow not
	// holding, since Add-ing a non-positive interval in a loop would never
	// terminate.
	interval := sched.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	next := sched.NextRunAt
	for !next.After(now) {
		next = next.Add(interval)
	}
	if err := store.UpdateScheduledTask(ctx, sched.ID, func(s *model.ScheduledTask) error {
		s.LastRunAt = &now
		s.NextRunAt = next
		return nil
	}); err != nil {
		return fmt.Errorf("advancing next run time: %w", err)
	}
	return nil
}
