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

func filedSchedule(t *testing.T, store *model.Store, id string, nextRunAt time.Time) model.Schedule {
	t.Helper()
	sched := model.Schedule{
		ID:         id,
		Title:      "Nightly dependency bump",
		Body:       "Bump every dependency to its latest patch release.",
		Target:     model.RepoRef{Owner: "acme", Name: "widgets"},
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24},
		Enabled:    true,
		NextRunAt:  nextRunAt,
		CreatedAt:  baseTime,
	}
	if err := store.PutSchedule(t.Context(), sched); err != nil {
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
	if err := store.PutSchedule(t.Context(), sched); err != nil {
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

	updated, err := store.GetSchedule(ctx, sched.ID)
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
	if err := store.UpdateSchedule(t.Context(), sched.ID, func(s *model.Schedule) error {
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
	if err := store.UpdateSchedule(ctx, sched.ID, func(s *model.Schedule) error {
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
// bwsalmon/agents#516's central guarantee: fireTaskSchedule reads a
// template-backed schedule's content off the template at firing time,
// not off whatever the schedule's own row last cached, so a template
// edited between two firings changes what the second one files -- and
// the schedule's own display cache (Title/Body/...) is kept in sync
// with it too. The schedule's own Target is left alone here because
// this template is unbound, which is the ordinary case; the bound one
// is TestReconcileScheduleFiresABoundTemplateAtItsOwnRepo below.
func TestReconcileScheduleFiresFromATemplateResolvedFresh(t *testing.T) {
	store, ctx := openStore(t)
	tmpl := model.Template{
		ID:     "template-1",
		Name:   "Dependency bump",
		Title:  "Bump dependencies",
		Body:   "Bump every dependency to its latest patch release.",
		Reads:  []model.RepoRef{{Owner: "acme", Name: "shared-lib"}},
		Grants: []model.Grant{{Capability: "web-search", Via: model.GrantByLabel}},
	}
	if err := store.PutTemplate(ctx, tmpl); err != nil {
		t.Fatal(err)
	}
	templateID := tmpl.ID
	sched := model.Schedule{
		ID:         "sched-1",
		TemplateID: &templateID,
		Target:     model.RepoRef{Owner: "acme", Name: "widgets"},
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24},
		Enabled:    true,
		NextRunAt:  baseTime.Add(-time.Minute),
		CreatedAt:  baseTime,
	}
	if err := store.PutSchedule(ctx, sched); err != nil {
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

	got, err := store.GetSchedule(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != tmpl.Title {
		t.Errorf("schedule's own display cache = %q after firing, want %q synced from the template", got.Title, tmpl.Title)
	}

	// Edit the template, close out the first firing, and force the
	// schedule due again -- the second firing should reflect the edit,
	// not the schedule's own now-stale cache.
	if err := store.UpdateTemplate(ctx, templateID, func(t *model.Template) error {
		t.Title = "Bump dependencies (patch only)"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	later := baseTime.Add(time.Hour)
	if err := store.Observe(ctx, model.Observation{TaskID: first[0].ID, ClosedAt: &later}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSchedule(ctx, sched.ID, func(s *model.Schedule) error {
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

// TestReconcileScheduleFiresABoundTemplateAtItsOwnRepo is the other
// half of grain/task-285: a template bound to a repo and branch decides
// what its firings target, over the schedule's own standing repo -- and
// the schedule's row is resynced to match, so what it says it fires and
// what it actually fires stay the same thing.
func TestReconcileScheduleFiresABoundTemplateAtItsOwnRepo(t *testing.T) {
	store, ctx := openStore(t)
	bound := model.RepoRef{Owner: "acme", Name: "gadgets"}
	tmpl := model.Template{
		ID: "template-1", Name: "Dependency bump", Title: "Bump dependencies",
		Target: &bound, Base: "release",
	}
	if err := store.PutTemplate(ctx, tmpl); err != nil {
		t.Fatal(err)
	}
	templateID := tmpl.ID
	sched := model.Schedule{
		ID:         "sched-1",
		TemplateID: &templateID,
		Target:     model.RepoRef{Owner: "acme", Name: "widgets"},
		Base:       "main",
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24},
		Enabled:    true,
		NextRunAt:  baseTime.Add(-time.Minute),
		CreatedAt:  baseTime,
	}
	if err := store.PutSchedule(ctx, sched); err != nil {
		t.Fatal(err)
	}

	if err := runScheduleOnly(t, store, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	filed := scheduledTasksTagged(t, store, "schedule:"+sched.ID)
	if len(filed) != 1 {
		t.Fatalf("filed %d tasks, want 1", len(filed))
	}
	if filed[0].Target == nil || *filed[0].Target != bound || filed[0].Base != "release" {
		t.Errorf("firing target/base = %+v/%q, want the template's binding %+v on release",
			filed[0].Target, filed[0].Base, bound)
	}

	got, err := store.GetSchedule(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != bound || got.Base != "release" {
		t.Errorf("schedule row = %+v/%q after firing, want it resynced to the binding", got.Target, got.Base)
	}
}

// TestReconcileScheduleFailsToFireWhenItsTemplateIsMissing checks
// fireTaskSchedule's own defensive path: a schedule whose TemplateID no
// longer resolves (ui.Client.DeleteTemplate tries to prevent this, but
// nothing in the store itself enforces it) fails that one firing rather
// than filing a task with no content, and does not advance NextRunAt --
// reconcileSchedule's own doc comment: one schedule failing to file does
// not stop the others, and is tried again next cycle exactly as it stood.
func TestReconcileScheduleFailsToFireWhenItsTemplateIsMissing(t *testing.T) {
	store, ctx := openStore(t)
	missing := "template-does-not-exist"
	sched := model.Schedule{
		ID:         "sched-1",
		TemplateID: &missing,
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24},
		Enabled:    true,
		NextRunAt:  baseTime.Add(-time.Minute),
		CreatedAt:  baseTime,
	}
	if err := store.PutSchedule(ctx, sched); err != nil {
		t.Fatal(err)
	}

	if err := runScheduleOnly(t, store, baseTime); err == nil {
		t.Fatal("want RunCycle to report an error for a schedule whose template is missing")
	}
	if filed := scheduledTasksTagged(t, store, "schedule:"+sched.ID); len(filed) != 0 {
		t.Fatalf("filed %d tasks for a schedule with a missing template, want 0", len(filed))
	}
	got, err := store.GetSchedule(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NextRunAt.Equal(sched.NextRunAt) {
		t.Errorf("nextRunAt = %v, want it left untouched at %v after a failed firing", got.NextRunAt, sched.NextRunAt)
	}
}

// --- schedules that run a suite -------------------------------------

// filedSuiteSchedule is filedSchedule's counterpart for the other thing
// a schedule can fire: a one-item suite, saved in the store the way
// ui.Client.CreateSuite would save it, and a schedule pointing at it.
func filedSuiteSchedule(t *testing.T, store *model.Store, id string, nextRunAt time.Time) (model.Suite, model.Schedule) {
	t.Helper()
	ctx := t.Context()
	if err := store.PutTemplate(ctx, suiteSmokeTemplate()); err != nil {
		t.Fatalf("put template: %v", err)
	}
	suite := model.Suite{
		ID: "suite-1", Name: "nightly smoke",
		Items:     []model.SuiteItem{{TemplateID: suiteSmokeTemplate().ID}},
		Mode:      model.SuiteCount,
		Count:     2,
		AutoMerge: true,
		CreatedAt: baseTime,
	}
	if err := store.PutSuite(ctx, suite); err != nil {
		t.Fatalf("put suite: %v", err)
	}
	suiteID := suite.ID
	sched := model.Schedule{
		ID:         id,
		Title:      suite.Name,
		SuiteID:    &suiteID,
		Target:     model.RepoRef{Owner: "acme", Name: "widgets"},
		Base:       "main",
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24},
		Enabled:    true,
		NextRunAt:  nextRunAt,
		CreatedAt:  baseTime,
	}
	if err := store.PutSchedule(ctx, sched); err != nil {
		t.Fatalf("filing schedule: %v", err)
	}
	return suite, sched
}

func runsOfSchedule(t *testing.T, store *model.Store, scheduleID string) []model.SuiteRun {
	t.Helper()
	all, err := store.ListSuiteRuns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []model.SuiteRun
	for _, r := range all {
		if r.ScheduleID == scheduleID {
			out = append(out, r)
		}
	}
	return out
}

// A due suite-backed schedule starts exactly the run ui.Client.
// CreateSuiteRun would have started by hand -- against the schedule's own
// repo and branch, with the suite's own items and settings -- and
// advances its own timing afterwards, the same as a schedule that files a
// task.
func TestReconcileScheduleStartsASuiteRun(t *testing.T) {
	store, ctx := openStore(t)
	suite, sched := filedSuiteSchedule(t, store, "sched-1", baseTime.Add(-time.Minute))

	if err := runScheduleOnly(t, store, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	runs := runsOfSchedule(t, store, sched.ID)
	if len(runs) != 1 {
		t.Fatalf("started %d runs, want 1", len(runs))
	}
	run := runs[0]
	if run.SuiteID != suite.ID || run.SuiteName != suite.Name {
		t.Errorf("run = %s/%q, want the schedule's own suite %s/%q", run.SuiteID, run.SuiteName, suite.ID, suite.Name)
	}
	if run.Target != sched.Target || run.Base != sched.Base {
		t.Errorf("run targets %+v@%s, want the schedule's own %+v@%s", run.Target, run.Base, sched.Target, sched.Base)
	}
	if run.Status != model.SuiteRunActive {
		t.Errorf("status = %q, want active", run.Status)
	}
	if len(run.PassTasks(1)) != 1 {
		t.Fatalf("pass 1 filed %d tasks, want one per suite item", len(run.PassTasks(1)))
	}

	// The tasks a run files are the suite's own, filed by the suite's own
	// principal -- a schedule decides only when the run happens, never
	// what a firing's tasks look like.
	task, err := store.GetTask(ctx, run.PassTasks(1)[0].TaskID)
	if err != nil || task == nil {
		t.Fatalf("get filed task: (%v, %v)", task, err)
	}
	if task.Origin.Reason != model.ReasonSuite {
		t.Errorf("origin reason = %q, want suite", task.Origin.Reason)
	}
	if task.Approval == nil {
		t.Error("want the filed task already approved: the suite does not require approval")
	}

	updated, err := store.GetSchedule(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.NextRunAt.After(baseTime) {
		t.Errorf("nextRunAt = %v, want it advanced past %v", updated.NextRunAt, baseTime)
	}
	if updated.LastRunAt == nil || !updated.LastRunAt.Equal(baseTime) {
		t.Errorf("lastRunAt = %v, want %v", updated.LastRunAt, baseTime)
	}
	if updated.Title != suite.Name {
		t.Errorf("display title = %q, want the suite's own name %q", updated.Title, suite.Name)
	}
}

// The suite counterpart of "a previous firing that has not finished
// suppresses the next one": for a suite the previous firing is a whole
// run, not a single task, so an active run is what holds the next firing
// back until it stops.
func TestReconcileScheduleWaitsForThePreviousSuiteRunToFinish(t *testing.T) {
	store, ctx := openStore(t)
	_, sched := filedSuiteSchedule(t, store, "sched-1", baseTime.Add(-time.Minute))

	if err := runScheduleOnly(t, store, baseTime); err != nil {
		t.Fatalf("first RunCycle: %v", err)
	}
	first := runsOfSchedule(t, store, sched.ID)
	if len(first) != 1 {
		t.Fatalf("started %d runs on the first cycle, want 1", len(first))
	}

	// Force the schedule due again while its first run is still going.
	later := baseTime.Add(time.Hour)
	forceDue := func() {
		t.Helper()
		if err := store.UpdateSchedule(ctx, sched.ID, func(s *model.Schedule) error {
			s.NextRunAt = baseTime
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	forceDue()
	if err := runScheduleOnly(t, store, later); err != nil {
		t.Fatalf("second RunCycle: %v", err)
	}
	if runs := runsOfSchedule(t, store, sched.ID); len(runs) != 1 {
		t.Fatalf("started %d runs while the first was still active, want still 1", len(runs))
	}

	// Once that run stops, the next cycle is free to start another.
	if err := store.CompleteSuiteRun(ctx, first[0].ID, model.SuiteRunSucceeded, "", later); err != nil {
		t.Fatal(err)
	}
	forceDue()
	if err := runScheduleOnly(t, store, later); err != nil {
		t.Fatalf("third RunCycle: %v", err)
	}
	if runs := runsOfSchedule(t, store, sched.ID); len(runs) != 2 {
		t.Fatalf("started %d runs once the first finished, want 2", len(runs))
	}
}

// fireSuiteSchedule's own defensive path, fireTaskSchedule's
// missing-template case one level out: a suite deleted out from under a
// schedule (ui.Client.DeleteSuite tries to prevent it) fails that one
// firing and leaves NextRunAt exactly where it was, rather than starting
// a run of nothing.
func TestReconcileScheduleFailsToFireWhenItsSuiteIsMissing(t *testing.T) {
	store, ctx := openStore(t)
	missing := "suite-does-not-exist"
	sched := model.Schedule{
		ID:         "sched-1",
		Title:      "nightly smoke",
		SuiteID:    &missing,
		Target:     model.RepoRef{Owner: "acme", Name: "widgets"},
		Base:       "main",
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24},
		Enabled:    true,
		NextRunAt:  baseTime.Add(-time.Minute),
		CreatedAt:  baseTime,
	}
	if err := store.PutSchedule(ctx, sched); err != nil {
		t.Fatal(err)
	}

	if err := runScheduleOnly(t, store, baseTime); err == nil {
		t.Fatal("want RunCycle to report an error for a schedule whose suite is missing")
	}
	if runs := runsOfSchedule(t, store, sched.ID); len(runs) != 0 {
		t.Fatalf("started %d runs for a schedule with a missing suite, want 0", len(runs))
	}
	got, err := store.GetSchedule(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NextRunAt.Equal(sched.NextRunAt) {
		t.Errorf("nextRunAt = %v, want it left untouched at %v after a failed firing", got.NextRunAt, sched.NextRunAt)
	}
}

// grain/task-368: a daily, weekly or monthly schedule's time of day is
// read against the deployment's own wall clock (model.Config.TimeZone),
// which RunCycle refreshes out of grain_config every cycle -- so a
// schedule saying "09:00" on a Pacific deployment comes due at 09:00
// there, not at 09:00 UTC.
//
// baseTime is 2026-08-27 12:00 UTC, which is 05:00 that morning in
// California, so the next 09:00 is later the same day: 16:00 UTC. Read
// against UTC, as every wall-clock schedule was before this, the answer
// would have been 09:00 the following morning instead.
func TestReconcileScheduleAdvancesOnTheDeploymentsOwnClock(t *testing.T) {
	store, ctx := openStore(t)
	// MaxWorkers 0, so the task this firing files stays queued: these
	// tests are about when the schedule comes due next, and this
	// package's schedule tests deliberately give RunCycle nothing to
	// dispatch with (the file's own header comment).
	cfg := model.DefaultConfig()
	cfg.MaxWorkers = 0
	if err := store.PutConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	sched := filedSchedule(t, store, "sched-1", baseTime.Add(-time.Minute))
	sched.Recurrence = model.Recurrence{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60}
	if err := store.PutSchedule(ctx, sched); err != nil {
		t.Fatal(err)
	}

	if err := runScheduleOnly(t, store, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	got, err := store.GetSchedule(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC)
	if !got.NextRunAt.Equal(want) {
		t.Errorf("nextRunAt = %v, want %v (09:00 Pacific)", got.NextRunAt.UTC(), want)
	}
}

// The same schedule on a deployment that has chosen UTC keeps the
// behaviour every wall-clock schedule had before there was a setting at
// all -- the zone is a choice, and one of the choices is the old one.
func TestReconcileScheduleHonoursAUTCDeployment(t *testing.T) {
	store, ctx := openStore(t)
	cfg := model.DefaultConfig()
	cfg.MaxWorkers, cfg.TimeZone = 0, "UTC"
	if err := store.PutConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	sched := filedSchedule(t, store, "sched-1", baseTime.Add(-time.Minute))
	sched.Recurrence = model.Recurrence{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60}
	if err := store.PutSchedule(ctx, sched); err != nil {
		t.Fatal(err)
	}

	if err := runScheduleOnly(t, store, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	got, err := store.GetSchedule(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	if !got.NextRunAt.Equal(want) {
		t.Errorf("nextRunAt = %v, want %v (09:00 UTC)", got.NextRunAt.UTC(), want)
	}
}
