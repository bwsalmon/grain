package sqlite_test

// Task suites' own store tests (bwsalmon/agents#642), against a real
// embedded SQLite database -- qualification_store_test.go's own
// reasoning applied to the second "declared template, instantiated
// run" mechanism built on TaskTemplate. What matters here and cannot be
// seen from pkg/orchestrator's own reconciler tests is the store half:
// that a run snapshots its suite rather than reading it live, that a
// pass files one task per item in order, and that the reads
// SyncTaskSuites decides from (OpenedPullRequest, Proposed, State)
// come back off the joins that produce them.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
)

func smokeTemplate(id string) model.TaskTemplate {
	return model.TaskTemplate{
		ID: id, Name: "Smoke", Title: "Smoke test", Body: "run the smoke suite",
		Reads:  []model.RepoRef{gadgets},
		Grants: []model.Grant{{Capability: "run-tests", Via: model.GrantByLabel}}, CreatedAt: now,
	}
}

func lintTemplate(id string) model.TaskTemplate {
	return model.TaskTemplate{
		ID: id, Name: "Lint", Title: "Lint", Body: "go vet ./...",
		CreatedAt: now,
	}
}

// putSuite writes a two-item suite through the store's own id
// allocation, the way ui.Client.CreateSuite does.
func putSuite(t *testing.T, ctx context.Context, store *model.Store, suite model.TaskSuite) model.TaskSuite {
	t.Helper()
	if err := store.PutTaskTemplate(ctx, smokeTemplate("template-smoke")); err != nil {
		t.Fatalf("put smoke template: %v", err)
	}
	if err := store.PutTaskTemplate(ctx, lintTemplate("template-lint")); err != nil {
		t.Fatalf("put lint template: %v", err)
	}
	id, err := store.NewTaskSuiteID(ctx)
	if err != nil {
		t.Fatalf("NewTaskSuiteID: %v", err)
	}
	suite.ID = id
	if err := store.PutTaskSuite(ctx, suite); err != nil {
		t.Fatalf("put suite: %v", err)
	}
	return suite
}

func twoItemSuite() model.TaskSuite {
	return model.TaskSuite{
		Name: "nightly",
		Items: []model.TaskSuiteItem{
			{TemplateID: "template-smoke"}, {TemplateID: "template-lint"},
		},
		Mode: model.TaskSuiteCount, Count: 2, MaxPasses: 0,
		RequireApproval: false, AutoMerge: true, CreatedAt: now,
	}
}

func TestNewTaskSuiteIDsAreDistinctAndPrefixed(t *testing.T) {
	store, _, ctx := openStore(t)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		id, err := store.NewTaskSuiteID(ctx)
		if err != nil {
			t.Fatalf("NewTaskSuiteID: %v", err)
		}
		if len(id) < len("suite-") || id[:len("suite-")] != "suite-" {
			t.Fatalf("got id %q, want a suite- prefix", id)
		}
		if seen[id] {
			t.Fatalf("id %q issued twice", id)
		}
		seen[id] = true
	}
}

func TestGetTaskSuiteReturnsNilOnAFreshDatabase(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetTaskSuite(ctx, "suite-1")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) for a suite that was never created, got (%+v, %v)", got, err)
	}
}

func TestTaskSuiteRoundTripsWithItsItemsInOrder(t *testing.T) {
	store, _, ctx := openStore(t)
	want := putSuite(t, ctx, store, twoItemSuite())

	got, err := store.GetTaskSuite(ctx, want.ID)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
}

func TestPutTaskSuiteReplacesItemsRatherThanAppendingThem(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())

	suite.Items = []model.TaskSuiteItem{{TemplateID: "template-lint"}}
	if err := store.PutTaskSuite(ctx, suite); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err := store.GetTaskSuite(ctx, suite.ID)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(got.Items, suite.Items) {
		t.Fatalf("got items %+v, want exactly the one written second", got.Items)
	}
}

func TestListTaskSuitesIsNewestFirstAndCarriesItems(t *testing.T) {
	store, _, ctx := openStore(t)
	older := putSuite(t, ctx, store, twoItemSuite())
	newer := twoItemSuite()
	newer.Name = "weekly"
	newer.CreatedAt = now.Add(time.Hour)
	newer = putSuite(t, ctx, store, newer)

	got, err := store.ListTaskSuites(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d suites, want 2", len(got))
	}
	if got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Fatalf("got order [%s %s], want newest (%s) first", got[0].ID, got[1].ID, newer.ID)
	}
	for _, s := range got {
		if len(s.Items) != 2 {
			t.Errorf("suite %s: got %d items, want 2 -- a list must hydrate items too", s.ID, len(s.Items))
		}
	}
}

func TestUpdateTaskSuiteAppliesMutateAndRefusesAMissingSuite(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())

	if err := store.UpdateTaskSuite(ctx, suite.ID, func(s *model.TaskSuite) error {
		s.Name = "renamed"
		s.Mode, s.MaxPasses = model.TaskSuiteUntilClean, 4
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.GetTaskSuite(ctx, suite.ID)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Name != "renamed" || got.Mode != model.TaskSuiteUntilClean || got.MaxPasses != 4 {
		t.Fatalf("got %+v, want the mutated name/mode/maxPasses", *got)
	}
	if err := store.UpdateTaskSuite(ctx, "suite-nope", func(*model.TaskSuite) error { return nil }); err == nil {
		t.Fatal("updating a suite that does not exist succeeded, want an error")
	}
}

func TestDeleteTaskSuiteRemovesItsItemsToo(t *testing.T) {
	// task_suite_item has no foreign key onto task_suite, so deleting
	// only the parent row would leave every item behind -- rows
	// TaskSuitesUsingTemplate would go on finding forever.
	store, db, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())

	if err := store.DeleteTaskSuite(ctx, suite.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := store.GetTaskSuite(ctx, suite.ID)
	if err != nil || got != nil {
		t.Fatalf("get after delete: (%+v, %v), want (nil, nil)", got, err)
	}
	var items int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `task_suite_item` WHERE `suite_id` = ?", suite.ID).Scan(&items); err != nil {
		t.Fatalf("counting items: %v", err)
	}
	if items != 0 {
		t.Fatalf("got %d item rows left behind after deleting %s, want 0", items, suite.ID)
	}
	using, err := store.TaskSuitesUsingTemplate(ctx, "template-smoke")
	if err != nil {
		t.Fatalf("TaskSuitesUsingTemplate: %v", err)
	}
	if len(using) != 0 {
		t.Fatalf("got %d suites still using template-smoke after the only one was deleted, want 0", len(using))
	}
}

func TestTaskSuitesUsingTemplateFindsEverySuiteNamingIt(t *testing.T) {
	store, _, ctx := openStore(t)
	both := putSuite(t, ctx, store, twoItemSuite())
	lintOnly := twoItemSuite()
	lintOnly.Items = []model.TaskSuiteItem{{TemplateID: "template-lint"}}
	lintOnly = putSuite(t, ctx, store, lintOnly)

	using, err := store.TaskSuitesUsingTemplate(ctx, "template-smoke")
	if err != nil {
		t.Fatalf("TaskSuitesUsingTemplate: %v", err)
	}
	if len(using) != 1 || using[0].ID != both.ID {
		t.Fatalf("got %+v, want only %s", using, both.ID)
	}
	using, err = store.TaskSuitesUsingTemplate(ctx, "template-lint")
	if err != nil {
		t.Fatalf("TaskSuitesUsingTemplate: %v", err)
	}
	if len(using) != 2 {
		t.Fatalf("got %d suites using template-lint, want both %s and %s", len(using), both.ID, lintOnly.ID)
	}
	using, err = store.TaskSuitesUsingTemplate(ctx, "template-unused")
	if err != nil || len(using) != 0 {
		t.Fatalf("got (%+v, %v) for a template no suite names, want none", using, err)
	}
}

// --- runs ----------------------------------------------------------------

func TestCreateTaskSuiteRunFilesOneTaskPerItemInTheFirstPass(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())

	run, err := store.CreateTaskSuiteRun(ctx, suite, widgets, "release-1", now)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.Status != model.TaskSuiteRunActive || run.SuiteID != suite.ID || run.SuiteName != "nightly" {
		t.Fatalf("got %+v, want an active run naming its suite", run)
	}
	if run.Target != widgets || run.Base != "release-1" {
		t.Fatalf("got target %s base %q, want %s / release-1", run.Target, run.Base, widgets)
	}
	if run.CurrentPass() != 1 {
		t.Fatalf("CurrentPass() = %d after creation, want 1", run.CurrentPass())
	}
	tasks := run.PassTasks(1)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks in pass 1, want one per item", len(tasks))
	}
	if tasks[0].TemplateID != "template-smoke" || tasks[1].TemplateID != "template-lint" {
		t.Fatalf("got %q then %q, want the suite's own item order", tasks[0].TemplateID, tasks[1].TemplateID)
	}
	if tasks[0].TemplateName != "Smoke" || tasks[1].TemplateName != "Lint" {
		t.Fatalf("got names %q/%q, want the resolved template names", tasks[0].TemplateName, tasks[1].TemplateName)
	}

	for _, ts := range tasks {
		if !ts.Approved {
			t.Errorf("task %s: want approved, RequireApproval is false", ts.TaskID)
		}
		if ts.State != model.StateQueued {
			t.Errorf("task %s: got state %s, want queued", ts.TaskID, ts.State)
		}
		if ts.OpenedPullRequest || ts.Proposed {
			t.Errorf("task %s: a freshly filed task has neither a pull request nor a proposal", ts.TaskID)
		}
		task, err := store.GetTask(ctx, ts.TaskID)
		if err != nil || task == nil {
			t.Fatalf("get task %s: (%+v, %v)", ts.TaskID, task, err)
		}
		if task.Base != "release-1" {
			t.Errorf("task %s: got base %q, want the run's own base", ts.TaskID, task.Base)
		}
		if task.Target == nil || *task.Target != widgets {
			t.Errorf("task %s: got target %v, want %s", ts.TaskID, task.Target, widgets)
		}
		if task.Origin.Reason != model.ReasonSuite {
			t.Errorf("task %s: got origin reason %q, want suite", ts.TaskID, task.Origin.Reason)
		}
		if task.Origin.Attribution.Actor != model.SuitePrincipal {
			t.Errorf("task %s: got actor %+v, want SuitePrincipal", ts.TaskID, task.Origin.Attribution.Actor)
		}
		if !task.AutoMerge {
			t.Errorf("task %s: want AutoMerge, the run's own switch is on", ts.TaskID)
		}
	}
}

func TestCreateTaskSuiteRunLeavesTasksUnapprovedWhenTheSuiteAsks(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := twoItemSuite()
	suite.RequireApproval, suite.AutoMerge = true, false
	suite = putSuite(t, ctx, store, suite)

	run, err := store.CreateTaskSuiteRun(ctx, suite, widgets, "main", now)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, ts := range run.PassTasks(1) {
		if ts.Approved {
			t.Errorf("task %s: want unapproved, RequireApproval is true", ts.TaskID)
		}
		if ts.State != model.StateProposed {
			t.Errorf("task %s: got state %s, want proposed", ts.TaskID, ts.State)
		}
		task, err := store.GetTask(ctx, ts.TaskID)
		if err != nil || task == nil {
			t.Fatalf("get task %s: (%+v, %v)", ts.TaskID, task, err)
		}
		if task.AutoMerge {
			t.Errorf("task %s: want AutoMerge off, the run's own switch is off", ts.TaskID)
		}
	}
}

func TestCreateTaskSuiteRunRefusesAMissingTemplate(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	suite.Items = append(suite.Items, model.TaskSuiteItem{TemplateID: "template-gone"})

	if _, err := store.CreateTaskSuiteRun(ctx, suite, widgets, "main", now); err == nil {
		t.Fatal("creating a run whose item names no template succeeded, want an error")
	}
}

func TestATaskSuiteRunIsASnapshotOfTheSuiteThatStartedIt(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	run, err := store.CreateTaskSuiteRun(ctx, suite, widgets, "main", now)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Edit the suite out from under the run: fewer items, a different
	// mode, and the opposite of both switches.
	if err := store.UpdateTaskSuite(ctx, suite.ID, func(s *model.TaskSuite) error {
		s.Items = []model.TaskSuiteItem{{TemplateID: "template-lint"}}
		s.Mode, s.MaxPasses, s.Count = model.TaskSuiteUntilClean, 9, 0
		s.RequireApproval, s.AutoMerge = true, false
		return nil
	}); err != nil {
		t.Fatalf("update suite: %v", err)
	}

	got, err := store.GetTaskSuiteRun(ctx, run.ID)
	if err != nil || got == nil {
		t.Fatalf("get run: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(got.Items, suite.Items) {
		t.Errorf("got items %+v, want the two the run was created with", got.Items)
	}
	if got.Mode != model.TaskSuiteCount || got.Count != 2 {
		t.Errorf("got mode %q count %d, want the snapshot count/2", got.Mode, got.Count)
	}
	if got.RequireApproval || !got.AutoMerge {
		t.Errorf("got requireApproval=%v autoMerge=%v, want the snapshot false/true", got.RequireApproval, got.AutoMerge)
	}

	// And the next pass fires the snapshot's items, not the suite's.
	after, err := store.FireNextPass(ctx, *got, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("fire next pass: %v", err)
	}
	if len(after.PassTasks(2)) != 2 {
		t.Fatalf("pass 2 filed %d tasks, want the snapshot's own 2", len(after.PassTasks(2)))
	}
}

func TestFireNextPassAddsAPassWithoutDisturbingTheLastOne(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	run, err := store.CreateTaskSuiteRun(ctx, suite, widgets, "main", now)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	firstPass := run.PassTasks(1)

	next, err := store.FireNextPass(ctx, run, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("fire next pass: %v", err)
	}
	if next.CurrentPass() != 2 {
		t.Fatalf("CurrentPass() = %d, want 2", next.CurrentPass())
	}
	if len(next.Tasks) != 4 {
		t.Fatalf("got %d tasks across the run, want 2 per pass over 2 passes", len(next.Tasks))
	}
	if !reflect.DeepEqual(next.PassTasks(1), firstPass) {
		t.Fatalf("pass 1 changed when pass 2 fired:\n got %+v\nwant %+v", next.PassTasks(1), firstPass)
	}
	for _, ts := range next.PassTasks(2) {
		if ts.PassNumber != 2 {
			t.Errorf("task %s: got pass %d, want 2", ts.TaskID, ts.PassNumber)
		}
	}
}

func TestTaskSuiteRunTasksReportPullRequestsAndProposals(t *testing.T) {
	// The two reads OutcomeOfPass' "clean" decision is built from, taken
	// straight off task_link the way finishWithPullRequest and
	// relayProposedTasks write them.
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	run, err := store.CreateTaskSuiteRun(ctx, suite, widgets, "main", now)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	tasks := run.PassTasks(1)
	opener, proposer := tasks[0].TaskID, tasks[1].TaskID

	if err := store.UpdateTask(ctx, opener, func(task *model.Task) error {
		task.Links = append(task.Links, model.Link{Kind: model.LinkFixes, Target: "acme/widgets#7"})
		return nil
	}); err != nil {
		t.Fatalf("recording a pull request: %v", err)
	}
	followUpID, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatalf("NewTaskID: %v", err)
	}
	if err := store.PutTask(ctx, model.Task{
		ID: followUpID, Intent: model.IntentImplement, Title: "follow up",
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.SuitePrincipal},
			Reason:      model.ReasonProposal,
		},
		Target: &widgets, Binding: model.BindingDirective, CreatedAt: &now,
		Links: []model.Link{{Kind: model.LinkProposedBy, Target: proposer}},
	}); err != nil {
		t.Fatalf("filing the follow-up task: %v", err)
	}

	got, err := store.GetTaskSuiteRun(ctx, run.ID)
	if err != nil || got == nil {
		t.Fatalf("get run: (%+v, %v)", got, err)
	}
	by := map[string]model.TaskSuiteTaskStatus{}
	for _, ts := range got.PassTasks(1) {
		by[ts.TaskID] = ts
	}
	if !by[opener].OpenedPullRequest || by[opener].Proposed {
		t.Errorf("opener %s: got openedPullRequest=%v proposed=%v, want true/false",
			opener, by[opener].OpenedPullRequest, by[opener].Proposed)
	}
	if by[proposer].OpenedPullRequest || !by[proposer].Proposed {
		t.Errorf("proposer %s: got openedPullRequest=%v proposed=%v, want false/true",
			proposer, by[proposer].OpenedPullRequest, by[proposer].Proposed)
	}
	if len(got.PassTasks(1)) != 2 {
		t.Fatalf("the follow-up task joined the pass: got %d tasks, want 2", len(got.PassTasks(1)))
	}
}

func TestActiveTaskSuiteRunsAndCompleteTaskSuiteRun(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	first, err := store.CreateTaskSuiteRun(ctx, suite, widgets, "main", now)
	if err != nil {
		t.Fatalf("create first run: %v", err)
	}
	second, err := store.CreateTaskSuiteRun(ctx, suite, gadgets, "main", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("create second run: %v", err)
	}

	active, err := store.ActiveTaskSuiteRuns(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("got %d active runs, want both", len(active))
	}

	done := now.Add(time.Hour)
	if err := store.CompleteTaskSuiteRun(ctx, first.ID, model.TaskSuiteRunFailed, "a task failed", done); err != nil {
		t.Fatalf("complete: %v", err)
	}
	active, err = store.ActiveTaskSuiteRuns(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if len(active) != 1 || active[0].ID != second.ID {
		t.Fatalf("got %+v, want only the still-active run %d", active, second.ID)
	}

	got, err := store.GetTaskSuiteRun(ctx, first.ID)
	if err != nil || got == nil {
		t.Fatalf("get run: (%+v, %v)", got, err)
	}
	if got.Status != model.TaskSuiteRunFailed || got.LastError != "a task failed" {
		t.Fatalf("got status %q error %q, want failed / a task failed", got.Status, got.LastError)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(done) {
		t.Fatalf("got completedAt %v, want %v", got.CompletedAt, done)
	}

	// A succeeded run carries no error at all.
	if err := store.CompleteTaskSuiteRun(ctx, second.ID, model.TaskSuiteRunSucceeded, "", done); err != nil {
		t.Fatalf("complete second: %v", err)
	}
	got, err = store.GetTaskSuiteRun(ctx, second.ID)
	if err != nil || got == nil {
		t.Fatalf("get second run: (%+v, %v)", got, err)
	}
	if got.Status != model.TaskSuiteRunSucceeded || got.LastError != "" {
		t.Fatalf("got status %q error %q, want succeeded and no error", got.Status, got.LastError)
	}
}

// A run a schedule fired records the schedule it came from, and
// HasActiveRunForSchedule reads it back -- the idempotency gate
// orchestrator.fireSuiteSchedule needs, and the exact counterpart of
// HasOpenTaskWithTag for a schedule that files a task instead. A database
// created before a schedule could fire a suite has a task_suite_run with
// no schedule_id column, which CREATE TABLE IF NOT EXISTS never adds --
// Store.Init's own migration step (ensureTaskSuiteRunScheduleColumn) is
// what does, leaving every run already on it reading as what it was:
// started by a human, not by a schedule.
func TestInitMigratesAnExistingDatabaseWithNoRunScheduleColumn(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`task_suite_run`"+` (
  `+"`id`"+`                INTEGER PRIMARY KEY AUTOINCREMENT,
  `+"`suite_id`"+`          TEXT     NOT NULL,
  `+"`suite_name`"+`        TEXT     NOT NULL,
  `+"`owner`"+`             TEXT     NOT NULL,
  `+"`repo`"+`              TEXT     NOT NULL,
  `+"`base`"+`              TEXT     NOT NULL,
  `+"`mode`"+`              TEXT     NOT NULL,
  `+"`count`"+`             INTEGER  NOT NULL,
  `+"`max_passes`"+`        INTEGER  NOT NULL,
  `+"`require_approval`"+`  INTEGER  NOT NULL,
  `+"`auto_merge`"+`        INTEGER  NOT NULL,
  `+"`status`"+`            TEXT     NOT NULL,
  `+"`last_error`"+`        TEXT     NULL,
  `+"`created_at`"+`        DATETIME NOT NULL,
  `+"`completed_at`"+`      DATETIME NULL
)`); err != nil {
		t.Fatalf("creating the pre-schedule task_suite_run table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task_suite_run` (`suite_id`,`suite_name`,`owner`,`repo`,`base`,`mode`,`count`,"+
			"`max_passes`,`require_approval`,`auto_merge`,`status`,`created_at`) "+
			"VALUES ('suite-1','nightly','acme','widgets','main','count',2,0,0,1,'active',?)", now); err != nil {
		t.Fatalf("seeding a pre-schedule run row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database with no schedule_id: %v", err)
	}

	got, err := store.GetTaskSuiteRun(ctx, 1)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.SuiteName != "nightly" || got.ScheduleID != "" {
		t.Fatalf("got %+v, want the pre-existing run intact and unattributed to any schedule", got)
	}
	active, err := store.HasActiveRunForSchedule(ctx, "sched-1")
	if err != nil {
		t.Fatalf("HasActiveRunForSchedule: %v", err)
	}
	if active {
		t.Error("a migrated run belongs to no schedule, so no schedule may be gated by it")
	}
}

func TestCreateScheduledSuiteRunRecordsItsSchedule(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())

	byHand, err := store.CreateTaskSuiteRun(ctx, suite, widgets, "main", now)
	if err != nil {
		t.Fatalf("create by hand: %v", err)
	}
	if byHand.ScheduleID != "" {
		t.Errorf("scheduleId = %q, want empty for a run started by hand", byHand.ScheduleID)
	}
	active, err := store.HasActiveRunForSchedule(ctx, "sched-1")
	if err != nil {
		t.Fatalf("HasActiveRunForSchedule: %v", err)
	}
	if active {
		t.Error("a run started by hand must not count as a schedule's own firing")
	}

	fired, err := store.CreateScheduledSuiteRun(ctx, suite, widgets, "main", "sched-1", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("create from a schedule: %v", err)
	}
	if fired.ScheduleID != "sched-1" {
		t.Errorf("scheduleId = %q, want sched-1", fired.ScheduleID)
	}
	active, err = store.HasActiveRunForSchedule(ctx, "sched-1")
	if err != nil {
		t.Fatalf("HasActiveRunForSchedule: %v", err)
	}
	if !active {
		t.Fatal("want sched-1 to have an active run while its firing is still going")
	}

	// Once that run finishes, the schedule is free to fire again.
	if err := store.CompleteTaskSuiteRun(ctx, fired.ID, model.TaskSuiteRunSucceeded, "", now.Add(time.Hour)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	active, err = store.HasActiveRunForSchedule(ctx, "sched-1")
	if err != nil {
		t.Fatalf("HasActiveRunForSchedule: %v", err)
	}
	if active {
		t.Error("want no active run for sched-1 once its firing completed")
	}
}

func TestListTaskSuiteRunsIsNewestFirst(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	older, err := store.CreateTaskSuiteRun(ctx, suite, widgets, "main", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	newer, err := store.CreateTaskSuiteRun(ctx, suite, gadgets, "main", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.ListTaskSuiteRuns(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Fatalf("got %+v, want newest (%d) first", got, newer.ID)
	}
	if len(got[0].Items) != 2 || len(got[0].PassTasks(1)) != 2 {
		t.Fatalf("a listed run must be hydrated: got %d items and %d pass-1 tasks",
			len(got[0].Items), len(got[0].PassTasks(1)))
	}
}

func TestGetTaskSuiteRunReturnsNilForAnUnknownID(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetTaskSuiteRun(ctx, 404)
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) for a run that does not exist, got (%+v, %v)", got, err)
	}
}
