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

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

func filedSchedule(t *testing.T, store *model.Store, id string, nextRunAt time.Time) model.ScheduledTask {
	t.Helper()
	sched := model.ScheduledTask{
		ID:         id,
		Title:      "Nightly dependency bump",
		Body:       "Bump every dependency to its latest patch release.",
		Target:     model.RepoRef{Owner: "acme", Name: "widgets"},
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24},
		Enabled:    true,
		NextRunAt:  nextRunAt,
		CreatedAt:  baseTime,
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
	sched.Reads = []model.RepoRef{{Owner: "acme", Name: "shared-lib"}}
	sched.Grants = []model.Grant{{Capability: "web-search", Via: model.GrantByLabel}}
	if err := store.PutScheduledTask(t.Context(), sched); err != nil {
		t.Fatal(err)
	}

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
	if len(got.Reads) != 1 || got.Reads[0] != sched.Reads[0] {
		t.Errorf("reads = %+v, want %+v", got.Reads, sched.Reads)
	}
	if len(got.Grants) != 1 || got.Grants[0].Capability != "web-search" {
		t.Errorf("grants = %+v, want web-search", got.Grants)
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

// TestReconcileScheduleFiresFromATemplateResolvedFresh is
// bwsalmon/agents#516's central guarantee: fireScheduledTask reads a
// template-backed schedule's content off the template at firing time, not
// off whatever the schedule's own row last cached, so a template edited
// between two firings changes what the second one files -- and the
// schedule's own display cache (Title/Body/...) is kept in sync with it
// too. The schedule's own Target is never touched: a template carries no
// target of its own (model.TaskTemplate's own doc comment on why), so it
// is the schedule's own standing repo that every firing targets.
func TestReconcileScheduleFiresFromATemplateResolvedFresh(t *testing.T) {
	store, ctx := openStore(t)
	tmpl := model.TaskTemplate{
		ID:     "template-1",
		Name:   "Dependency bump",
		Title:  "Bump dependencies",
		Body:   "Bump every dependency to its latest patch release.",
		Reads:  []model.RepoRef{{Owner: "acme", Name: "shared-lib"}},
		Grants: []model.Grant{{Capability: "web-search", Via: model.GrantByLabel}},
	}
	if err := store.PutTaskTemplate(ctx, tmpl); err != nil {
		t.Fatal(err)
	}
	templateID := tmpl.ID
	sched := model.ScheduledTask{
		ID:         "sched-1",
		TemplateID: &templateID,
		Target:     model.RepoRef{Owner: "acme", Name: "widgets"},
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24},
		Enabled:    true,
		NextRunAt:  baseTime.Add(-time.Minute),
		CreatedAt:  baseTime,
	}
	if err := store.PutScheduledTask(ctx, sched); err != nil {
		t.Fatal(err)
	}

	if err := runScheduleOnly(t, store, baseTime); err != nil {
		t.Fatalf("first RunCycle: %v", err)
	}
	first := scheduledTasksTagged(t, store, "schedule:"+sched.ID)
	if len(first) != 1 {
		t.Fatalf("filed %d tasks on the first cycle, want 1", len(first))
	}
	if first[0].Title != tmpl.Title || first[0].Body != tmpl.Body {
		t.Errorf("first firing title/body = %q/%q, want the template's own", first[0].Title, first[0].Body)
	}
	if first[0].Target == nil || *first[0].Target != sched.Target {
		t.Errorf("first firing target = %+v, want the schedule's own %+v", first[0].Target, sched.Target)
	}
	if len(first[0].Reads) != 1 || first[0].Reads[0] != tmpl.Reads[0] {
		t.Errorf("first firing reads = %+v, want %+v", first[0].Reads, tmpl.Reads)
	}

	got, err := store.GetScheduledTask(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != tmpl.Title {
		t.Errorf("schedule's own display cache = %q after firing, want %q synced from the template", got.Title, tmpl.Title)
	}

	// Edit the template, close out the first firing, and force the
	// schedule due again -- the second firing should reflect the edit,
	// not the schedule's own now-stale cache.
	if err := store.UpdateTaskTemplate(ctx, templateID, func(t *model.TaskTemplate) error {
		t.Title = "Bump dependencies (patch only)"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	later := baseTime.Add(time.Hour)
	if err := store.Observe(ctx, model.Observation{TaskID: first[0].ID, ClosedAt: &later}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateScheduledTask(ctx, sched.ID, func(s *model.ScheduledTask) error {
		s.NextRunAt = baseTime
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := runScheduleOnly(t, store, later); err != nil {
		t.Fatalf("second RunCycle: %v", err)
	}
	filed := scheduledTasksTagged(t, store, "schedule:"+sched.ID)
	if len(filed) != 2 {
		t.Fatalf("filed %d tasks after the second cycle, want 2", len(filed))
	}
	var second model.Task
	for _, tk := range filed {
		if tk.ID != first[0].ID {
			second = tk
		}
	}
	if second.Title != "Bump dependencies (patch only)" {
		t.Errorf("second firing title = %q, want the freshly-edited template title", second.Title)
	}
}

// TestReconcileScheduleFailsToFireWhenItsTemplateIsMissing checks
// fireScheduledTask's own defensive path: a schedule whose TemplateID no
// longer resolves (ui.Client.DeleteTemplate tries to prevent this, but
// nothing in the store itself enforces it) fails that one firing rather
// than filing a task with no content, and does not advance NextRunAt --
// reconcileSchedule's own doc comment: one schedule failing to file does
// not stop the others, and is tried again next cycle exactly as it stood.
func TestReconcileScheduleFailsToFireWhenItsTemplateIsMissing(t *testing.T) {
	store, ctx := openStore(t)
	missing := "template-does-not-exist"
	sched := model.ScheduledTask{
		ID:         "sched-1",
		TemplateID: &missing,
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24},
		Enabled:    true,
		NextRunAt:  baseTime.Add(-time.Minute),
		CreatedAt:  baseTime,
	}
	if err := store.PutScheduledTask(ctx, sched); err != nil {
		t.Fatal(err)
	}

	if err := runScheduleOnly(t, store, baseTime); err == nil {
		t.Fatal("want RunCycle to report an error for a schedule whose template is missing")
	}
	if filed := scheduledTasksTagged(t, store, "schedule:"+sched.ID); len(filed) != 0 {
		t.Fatalf("filed %d tasks for a schedule with a missing template, want 0", len(filed))
	}
	got, err := store.GetScheduledTask(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NextRunAt.Equal(sched.NextRunAt) {
		t.Errorf("nextRunAt = %v, want it left untouched at %v after a failed firing", got.NextRunAt, sched.NextRunAt)
	}
}
