package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// scheduler is the automation principal every task a schedule files is
// attributed to -- its own actor, distinct from "grain" (relayed agent
// output) and "merge-queue" (automatic PR repair), so a human reading a
// task's conversation or origin can tell which of grain's own mechanisms
// filed it.
var scheduler = model.Principal{Kind: model.PrincipalAutomation, ID: "schedule"}

// reconcileSchedule fires every schedule whose interval has come due --
// filing a task, or starting a suite run, depending on which of the two
// that schedule names (fireSchedule).
//
// One schedule failing to file (or a store error while checking it)
// does not stop the others -- Reconciler's own doc comment on why each
// one re-reads the store every cycle applies again one level down:
// a schedule this cycle could not fire is one next cycle tries again,
// exactly as it stood before.
func reconcileSchedule(ctx context.Context, deps Deps, now time.Time) error {
	due, err := deps.Store.DueSchedules(ctx, now)
	if err != nil {
		return fmt.Errorf("orchestrator: listing due schedules: %w", err)
	}
	var errs []error
	for _, sched := range due {
		if err := fireSchedule(ctx, deps.Store, sched, now); err != nil {
			errs = append(errs, fmt.Errorf("orchestrator: firing schedule %s: %w", sched.ID, err))
		}
	}
	return errors.Join(errs...)
}

// fireSchedule fires sched, whichever of the two things a schedule can
// fire it names: a suite run (SuiteID set) or a single task. The two
// paths differ only in what a firing *is* -- both check that the
// previous firing has finished first, and both advance the schedule's
// own timing afterwards through advanceSchedule.
func fireSchedule(ctx context.Context, store *model.Store, sched model.Schedule, now time.Time) error {
	if sched.SuiteID != nil {
		return fireSuiteSchedule(ctx, store, sched, now)
	}
	return fireTaskSchedule(ctx, store, sched, now)
}

// firingTag is the idempotency marker every task a schedule files
// carries -- v1's scheduled_jobs.py marker_label, ported onto
// model.Task.Tags per docs/data-model.md ("neither a state nor a
// capability: it is an idempotency tag"). fireTaskSchedule checks it
// before filing again, so a schedule whose previous firing is still
// running (or awaiting a reply, or otherwise unclosed) does not pile up a
// second one on top of it.
func firingTag(scheduleID string) string { return "schedule:" + scheduleID }

// fireTaskSchedule files sched's next task, unless its previous firing
// has not finished yet -- in which case this is a no-op, tried again
// next cycle with NextRunAt left exactly where it was, so the check
// costs nothing and cannot skip a firing outright, only delay it. This
// is the path for a schedule that files a single task;
// fireSuiteSchedule is the one for a schedule that runs a whole suite
// instead.
//
// The filed task lands already approved: docs/data-model.md's "the
// SCHEDULED special case dissolves" is why -- a schedule is itself a
// human's standing approval, written once and editable like any other
// declaration, so a firing needs no approval flag of its own the way
// the merge queue's own automatic repair needs none either.
//
// sched.TemplateID (bwsalmon/agents#516), if set, is resolved here --
// fresh, against the store, not against whatever sched.Title/Body/
// AutoMerge/Reads/Grants already say -- so a template edited since this
// schedule last fired is what actually gets filed, not a stale copy. A
// template deleted out from under a schedule (ui.Client.DeleteTemplate
// tries to prevent this, but nothing stops a row from disappearing some
// other way) fails this one firing with a plain error, retried next
// cycle exactly like any other store error reconcileSchedule's own doc
// comment already tolerates -- not treated as reason to disable the
// schedule outright.
//
// sched.Target and sched.Base are never taken from the template, set or
// not -- a template carries no target of its own (model.Template's own
// doc comment on why), so the schedule's own standing repo and branch
// are what every firing targets, template-backed or not.
func fireTaskSchedule(ctx context.Context, store *model.Store, sched model.Schedule, now time.Time) error {
	tag := firingTag(sched.ID)
	open, err := store.HasOpenTaskWithTag(ctx, tag)
	if err != nil {
		return fmt.Errorf("checking for a previous unfinished firing: %w", err)
	}
	if open {
		return nil
	}

	content := sched
	if sched.TemplateID != nil {
		tmpl, err := store.GetTemplate(ctx, *sched.TemplateID)
		if err != nil {
			return fmt.Errorf("resolving template %s: %w", *sched.TemplateID, err)
		}
		if tmpl == nil {
			return fmt.Errorf("template %s no longer exists", *sched.TemplateID)
		}
		content.Title, content.Body, content.AutoMerge, content.Reads, content.Grants =
			tmpl.Title, tmpl.Body, tmpl.AutoMerge, tmpl.Reads, tmpl.Grants
	}

	id, err := store.NewTaskID(ctx)
	if err != nil {
		return fmt.Errorf("allocating an id for %q: %w", content.Title, err)
	}
	target := content.Target
	task := model.Task{
		ID:     id,
		Intent: model.IntentImplement,
		Title:  content.Title,
		Body:   content.Body,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: scheduler},
			Reason:      model.ReasonSchedule,
		},
		Approval:  &model.Attribution{Actor: scheduler},
		Target:    &target,
		Binding:   model.BindingDirective,
		Base:      content.Base,
		Reads:     content.Reads,
		Grants:    content.Grants,
		AutoMerge: content.AutoMerge,
		Tags:      []string{tag},
		CreatedAt: &now,
	}
	if err := store.PutTask(ctx, task); err != nil {
		return fmt.Errorf("filing task: %w", err)
	}

	return advanceSchedule(ctx, store, sched, now, func(s *model.Schedule) {
		// Keeps the schedule's own display cache in sync with the
		// template it fired from (Schedule.TemplateID's own doc comment)
		// -- a no-op assignment when TemplateID is nil, since content is
		// just sched unchanged in that case.
		if sched.TemplateID != nil {
			s.Title, s.Body, s.AutoMerge, s.Reads, s.Grants =
				content.Title, content.Body, content.AutoMerge, content.Reads, content.Grants
		}
	})
}

// fireSuiteSchedule starts one run of the suite sched names --
// bwsalmon/agents#642's own "run the suite against a repo and branch",
// started by sched's cadence instead of by a human clicking "Run…", and
// otherwise exactly the run ui.Client.CreateSuiteRun would have made:
// the suite decides its own items, mode, passes, approval and
// auto-merge, and sched decides only when it runs and what it runs
// against.
//
// The gate here is a run of this schedule that has not finished yet, not
// an open task: a suite run is a whole sequence of tasks over as many
// passes as its mode asks for, so "has the previous firing finished" is a
// question about the run, which is what Store.HasActiveRunForSchedule
// answers. Same reasoning as the task path's own tag check (and the same
// consequence: a suppressed firing leaves NextRunAt where it was, so it
// is delayed rather than skipped).
//
// A suite deleted out from under a schedule (ui.Client.DeleteSuite tries
// to prevent this, the same way DeleteTemplate does for a template) fails
// this one firing with a plain error, retried next cycle --
// fireTaskSchedule's own missing-template path, unchanged in shape.
func fireSuiteSchedule(ctx context.Context, store *model.Store, sched model.Schedule, now time.Time) error {
	active, err := store.HasActiveRunForSchedule(ctx, sched.ID)
	if err != nil {
		return fmt.Errorf("checking for a previous unfinished firing: %w", err)
	}
	if active {
		return nil
	}

	suite, err := store.GetSuite(ctx, *sched.SuiteID)
	if err != nil {
		return fmt.Errorf("resolving suite %s: %w", *sched.SuiteID, err)
	}
	if suite == nil {
		return fmt.Errorf("suite %s no longer exists", *sched.SuiteID)
	}
	if _, err := store.CreateScheduledSuiteRun(ctx, *suite, sched.Target, sched.Base, sched.ID, now); err != nil {
		return fmt.Errorf("starting a run of suite %s: %w", suite.ID, err)
	}

	return advanceSchedule(ctx, store, sched, now, func(s *model.Schedule) {
		// The suite's own name, as this schedule's display cache -- the
		// same one-firing-behind sync a template-backed schedule keeps
		// (Schedule.SuiteID's own doc comment), so ui.scheduleFrom can
		// name what a schedule runs with no extra lookup.
		s.Title = suite.Name
	})
}

// advanceSchedule records that sched fired at now and moves it on to its
// next occurrence, applying whatever display-cache sync the firing itself
// implies (sync, which may be nil).
//
// Recurrence is validated at creation (ui.Client.CreateSchedule/
// UpdateSchedule), so Next always moves strictly forward -- the loop here
// is what gives a schedule paused (or a daemon down) through several
// missed occurrences exactly one firing on resume, resynced to its normal
// cadence, rather than one firing per missed occurrence or drift against
// wall-clock time (Recurrence.Next's own doc comment).
func advanceSchedule(ctx context.Context, store *model.Store, sched model.Schedule, now time.Time, sync func(*model.Schedule)) error {
	next := sched.NextRunAt
	for !next.After(now) {
		next = sched.Recurrence.Next(next)
	}
	if err := store.UpdateSchedule(ctx, sched.ID, func(s *model.Schedule) error {
		s.LastRunAt = &now
		s.NextRunAt = next
		if sync != nil {
			sync(s)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("advancing next run time: %w", err)
	}
	return nil
}
