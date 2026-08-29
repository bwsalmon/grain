package orchestrator_test

// reconcileSchedule's behaviour, reached only through the exported
// RunCycle -- this package's tests never call an unexported reconciler
// directly (isolation_test.go's own tests already hold to that), so a
// cycle here needs Deps enough to run every reconciler safely: no slots
// to dispatch onto and no open pull request links means dispatch and
// sync both no-op without a real github.Client, Sandboxes or Framework.

import (
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

func filedSchedule(t *testing.T, store *model.Store, id string, nextRunAt time.Time) model.ScheduledTask {
	t.Helper()
	sched := model.ScheduledTask{
		ID:        id,
		Title:     "Nightly dependency bump",
		Body:      "Bump every dependency to its latest patch release.",
		Target:    model.RepoRef{Owner: "acme", Name: "widgets"},
		Interval:  24 * time.Hour,
		Enabled:   true,
		NextRunAt: nextRunAt,
		CreatedAt: baseTime,
	}
	if err := store.PutScheduledTask(t.Context(), sched); err != nil {
		t.Fatalf("filing schedule: %v", err)
	}
	return sched
}

func runScheduleOnly(t *testing.T, store *model.Store, now time.Time) error {
	t.Helper()
	deps := orchestrator.Deps{Store: store}
	return orchestrator.RunCycle(t.Context(), deps, now)
}

func scheduledTasksTagged(t *testing.T, store *model.Store, tag string) []model.Task {
	t.Helper()
	all, err := store.ListTasks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []model.Task
	for _, tk := range all {
		for _, got := range tk.Tags {
			if got == tag {
				out = append(out, tk)
			}
		}
	}
	return out
}

func TestReconcileScheduleFilesAnAlreadyApprovedTask(t *testing.T) {
	store, ctx := openStore(t)
	sched := filedSchedule(t, store, "sched-1", baseTime.Add(-time.Minute))

	if err := runScheduleOnly(t, store, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	filed := scheduledTasksTagged(t, store, "schedule:"+sched.ID)
	if len(filed) != 1 {
		t.Fatalf("filed %d tasks tagged %s, want 1", len(filed), "schedule:"+sched.ID)
	}
	got := filed[0]
	if got.Origin.Reason != model.ReasonSchedule {
		t.Errorf("origin reason = %q, want schedule", got.Origin.Reason)
	}
	if got.Origin.Attribution.Actor.Kind != model.PrincipalAutomation {
		t.Errorf("origin actor = %+v, want automation", got.Origin.Attribution.Actor)
	}
	// docs/data-model.md: creating a schedule is itself the standing
	// human approval, so every firing lands already approved -- queued,
	// not proposed, with no separate flag to have set.
	if got.Approval == nil {
		t.Fatal("want the filed task already approved")
	}
	if got.Target == nil || *got.Target != sched.Target {
		t.Errorf("target = %+v, want %+v", got.Target, sched.Target)
	}
	if got.Title != sched.Title || got.Body != sched.Body {
		t.Errorf("title/body did not carry over: %+v", got)
	}

	updated, err := store.GetScheduledTask(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.NextRunAt.After(baseTime) {
		t.Errorf("nextRunAt = %v, want it advanced past %v", updated.NextRunAt, baseTime)
	}
	if updated.LastRunAt == nil || !updated.LastRunAt.Equal(baseTime) {
		t.Errorf("lastRunAt = %v, want %v", updated.LastRunAt, baseTime)
	}
}

func TestReconcileScheduleSkipsOneNotYetDue(t *testing.T) {
	store, _ := openStore(t)
	sched := filedSchedule(t, store, "sched-1", baseTime.Add(time.Hour))

	if err := runScheduleOnly(t, store, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	if filed := scheduledTasksTagged(t, store, "schedule:"+sched.ID); len(filed) != 0 {
		t.Fatalf("filed %d tasks for a schedule not yet due, want 0", len(filed))
	}
}

func TestReconcileScheduleSkipsOneThatsPaused(t *testing.T) {
	store, _ := openStore(t)
	sched := filedSchedule(t, store, "sched-1", baseTime.Add(-time.Minute))
	if err := store.UpdateScheduledTask(t.Context(), sched.ID, func(s *model.ScheduledTask) error {
		s.Enabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := runScheduleOnly(t, store, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	if filed := scheduledTasksTagged(t, store, "schedule:"+sched.ID); len(filed) != 0 {
		t.Fatalf("filed %d tasks for a paused schedule, want 0", len(filed))
	}
}

// A schedule whose previous firing has not finished yet does not pile up
// a second one on top of it -- v1's scheduled_jobs.py marker_label
// idempotency check, ported onto model.Task.Tags (docs/data-model.md).
func TestReconcileScheduleWaitsForThePreviousFiringToFinish(t *testing.T) {
	store, ctx := openStore(t)
	sched := filedSchedule(t, store, "sched-1", baseTime.Add(-time.Minute))

	if err := runScheduleOnly(t, store, baseTime); err != nil {
		t.Fatalf("first RunCycle: %v", err)
	}
	firstRun := scheduledTasksTagged(t, store, "schedule:"+sched.ID)
	if len(firstRun) != 1 {
		t.Fatalf("filed %d tasks on the first cycle, want 1", len(firstRun))
	}

	// Force the schedule due again immediately, the way a very short
	// interval would in practice, while its first firing is still open.
	later := baseTime.Add(time.Hour)
	if err := store.UpdateScheduledTask(ctx, sched.ID, func(s *model.ScheduledTask) error {
		s.NextRunAt = baseTime
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := runScheduleOnly(t, store, later); err != nil {
		t.Fatalf("second RunCycle: %v", err)
	}
	if filed := scheduledTasksTagged(t, store, "schedule:"+sched.ID); len(filed) != 1 {
		t.Fatalf("filed %d tasks while the first was still open, want still 1", len(filed))
	}

	// Once the first firing closes out, the next cycle is free to file
	// another.
	if err := store.Observe(ctx, model.Observation{TaskID: firstRun[0].ID, ClosedAt: &later}); err != nil {
		t.Fatal(err)
	}
	if err := runScheduleOnly(t, store, later); err != nil {
		t.Fatalf("third RunCycle: %v", err)
	}
	if filed := scheduledTasksTagged(t, store, "schedule:"+sched.ID); len(filed) != 2 {
		t.Fatalf("filed %d tasks once the first closed, want 2", len(filed))
	}
}
