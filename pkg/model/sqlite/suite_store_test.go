package sqlite_test

// Suites' own store tests (bwsalmon/agents#642), against a real
// embedded SQLite database -- qualification_store_test.go's own
// reasoning applied to the second "declared template, instantiated run"
// mechanism built on Template. What matters here and cannot be seen
// from pkg/orchestrator's own reconciler tests is the store half: that
// a run snapshots its suite rather than reading it live, that a pass
// files one task per item in order, and that the reads SyncSuites
// decides from (OpenedPullRequest, Proposed, State) come back off the
// joins that produce them.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
)

func smokeTemplate(id string) model.Template {
	return model.Template{
		ID: id, Name: "Smoke", Title: "Smoke test", Body: "run the smoke suite",
		Reads:  []model.RepoRef{gadgets},
		Grants: []model.Grant{{Capability: "run-tests", Via: model.GrantByLabel}}, CreatedAt: now,
	}
}

func lintTemplate(id string) model.Template {
	return model.Template{
		ID: id, Name: "Lint", Title: "Lint", Body: "go vet ./...",
		CreatedAt: now,
	}
}

// putSuite writes a two-item suite through the store's own id
// allocation, the way ui.Client.CreateSuite does.
func putSuite(t *testing.T, ctx context.Context, store *model.Store, suite model.Suite) model.Suite {
	t.Helper()
	if err := store.PutTemplate(ctx, smokeTemplate("template-smoke")); err != nil {
		t.Fatalf("put smoke template: %v", err)
	}
	if err := store.PutTemplate(ctx, lintTemplate("template-lint")); err != nil {
		t.Fatalf("put lint template: %v", err)
	}
	id, err := store.NewSuiteID(ctx)
	if err != nil {
		t.Fatalf("NewSuiteID: %v", err)
	}
	suite.ID = id
	if err := store.PutSuite(ctx, suite); err != nil {
		t.Fatalf("put suite: %v", err)
	}
	return suite
}

func twoItemSuite() model.Suite {
	return model.Suite{
		Name: "nightly",
		Items: []model.SuiteItem{
			{TemplateID: "template-smoke"}, {TemplateID: "template-lint"},
		},
		Mode: model.SuiteCount, Count: 2, MaxPasses: 0,
		RequireApproval: false, AutoMerge: true, CreatedAt: now,
	}
}

func TestNewSuiteIDsAreDistinctAndPrefixed(t *testing.T) {
	store, _, ctx := openStore(t)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		id, err := store.NewSuiteID(ctx)
		if err != nil {
			t.Fatalf("NewSuiteID: %v", err)
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

func TestGetSuiteReturnsNilOnAFreshDatabase(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetSuite(ctx, "suite-1")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) for a suite that was never created, got (%+v, %v)", got, err)
	}
}

func TestSuiteRoundTripsWithItsItemsInOrder(t *testing.T) {
	store, _, ctx := openStore(t)
	want := putSuite(t, ctx, store, twoItemSuite())

	got, err := store.GetSuite(ctx, want.ID)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
}

func TestPutSuiteReplacesItemsRatherThanAppendingThem(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())

	suite.Items = []model.SuiteItem{{TemplateID: "template-lint"}}
	if err := store.PutSuite(ctx, suite); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err := store.GetSuite(ctx, suite.ID)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(got.Items, suite.Items) {
		t.Fatalf("got items %+v, want exactly the one written second", got.Items)
	}
}

func TestListSuitesIsNewestFirstAndCarriesItems(t *testing.T) {
	store, _, ctx := openStore(t)
	older := putSuite(t, ctx, store, twoItemSuite())
	newer := twoItemSuite()
	newer.Name = "weekly"
	newer.CreatedAt = now.Add(time.Hour)
	newer = putSuite(t, ctx, store, newer)

	got, err := store.ListSuites(ctx)
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

func TestUpdateSuiteAppliesMutateAndRefusesAMissingSuite(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())

	if err := store.UpdateSuite(ctx, suite.ID, func(s *model.Suite) error {
		s.Name = "renamed"
		s.Mode, s.MaxPasses = model.SuiteUntilClean, 4
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.GetSuite(ctx, suite.ID)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Name != "renamed" || got.Mode != model.SuiteUntilClean || got.MaxPasses != 4 {
		t.Fatalf("got %+v, want the mutated name/mode/maxPasses", *got)
	}
	if err := store.UpdateSuite(ctx, "suite-nope", func(*model.Suite) error { return nil }); err == nil {
		t.Fatal("updating a suite that does not exist succeeded, want an error")
	}
}

func TestDeleteSuiteRemovesItsItemsToo(t *testing.T) {
	// suite_item has no foreign key onto suite, so deleting only the
	// parent row would leave every item behind -- rows
	// SuitesUsingTemplate would go on finding forever.
	store, db, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())

	if err := store.DeleteSuite(ctx, suite.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := store.GetSuite(ctx, suite.ID)
	if err != nil || got != nil {
		t.Fatalf("get after delete: (%+v, %v), want (nil, nil)", got, err)
	}
	var items int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `suite_item` WHERE `suite_id` = ?", suite.ID).Scan(&items); err != nil {
		t.Fatalf("counting items: %v", err)
	}
	if items != 0 {
		t.Fatalf("got %d item rows left behind after deleting %s, want 0", items, suite.ID)
	}
	using, err := store.SuitesUsingTemplate(ctx, "template-smoke")
	if err != nil {
		t.Fatalf("SuitesUsingTemplate: %v", err)
	}
	if len(using) != 0 {
		t.Fatalf("got %d suites still using template-smoke after the only one was deleted, want 0", len(using))
	}
}

func TestSuitesUsingTemplateFindsEverySuiteNamingIt(t *testing.T) {
	store, _, ctx := openStore(t)
	both := putSuite(t, ctx, store, twoItemSuite())
	lintOnly := twoItemSuite()
	lintOnly.Items = []model.SuiteItem{{TemplateID: "template-lint"}}
	lintOnly = putSuite(t, ctx, store, lintOnly)

	using, err := store.SuitesUsingTemplate(ctx, "template-smoke")
	if err != nil {
		t.Fatalf("SuitesUsingTemplate: %v", err)
	}
	if len(using) != 1 || using[0].ID != both.ID {
		t.Fatalf("got %+v, want only %s", using, both.ID)
	}
	using, err = store.SuitesUsingTemplate(ctx, "template-lint")
	if err != nil {
		t.Fatalf("SuitesUsingTemplate: %v", err)
	}
	if len(using) != 2 {
		t.Fatalf("got %d suites using template-lint, want both %s and %s", len(using), both.ID, lintOnly.ID)
	}
	using, err = store.SuitesUsingTemplate(ctx, "template-unused")
	if err != nil || len(using) != 0 {
		t.Fatalf("got (%+v, %v) for a template no suite names, want none", using, err)
	}
}

// --- runs ----------------------------------------------------------------

func TestCreateSuiteRunFilesOneTaskPerItemInTheFirstPass(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())

	run, err := store.CreateSuiteRun(ctx, suite, widgets, "release-1", now)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.Status != model.SuiteRunActive || run.SuiteID != suite.ID || run.SuiteName != "nightly" {
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

// TestCreateSuiteRunSendsABoundItemToItsOwnRepo is fireSuitePass's own
// binding rule (grain/task-285): a run's target and base are what its
// items normally get, but an item whose template is bound to a repo of
// its own goes there instead, so a suite can mix repo-agnostic items
// with ones that only make sense against one repo.
func TestCreateSuiteRunSendsABoundItemToItsOwnRepo(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	bound := lintTemplate("template-lint")
	bound.Target, bound.Base = &gadgets, "release"
	if err := store.PutTemplate(ctx, bound); err != nil {
		t.Fatalf("binding the lint template: %v", err)
	}

	run, err := store.CreateSuiteRun(ctx, suite, widgets, "release-1", now)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, ts := range run.PassTasks(1) {
		task, err := store.GetTask(ctx, ts.TaskID)
		if err != nil || task == nil {
			t.Fatalf("get task %s: (%+v, %v)", ts.TaskID, task, err)
		}
		wantTarget, wantBase := widgets, "release-1"
		if ts.TemplateID == "template-lint" {
			wantTarget, wantBase = gadgets, "release"
		}
		if task.Target == nil || *task.Target != wantTarget || task.Base != wantBase {
			t.Errorf("%s: target/base = %v/%q, want %s/%q",
				ts.TemplateID, task.Target, task.Base, wantTarget, wantBase)
		}
	}
}

func TestCreateSuiteRunLeavesTasksUnapprovedWhenTheSuiteAsks(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := twoItemSuite()
	suite.RequireApproval, suite.AutoMerge = true, false
	suite = putSuite(t, ctx, store, suite)

	run, err := store.CreateSuiteRun(ctx, suite, widgets, "main", now)
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

func TestCreateSuiteRunRefusesAMissingTemplate(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	suite.Items = append(suite.Items, model.SuiteItem{TemplateID: "template-gone"})

	if _, err := store.CreateSuiteRun(ctx, suite, widgets, "main", now); err == nil {
		t.Fatal("creating a run whose item names no template succeeded, want an error")
	}
}

func TestASuiteRunIsASnapshotOfTheSuiteThatStartedIt(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	run, err := store.CreateSuiteRun(ctx, suite, widgets, "main", now)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Edit the suite out from under the run: fewer items, a different
	// mode, and the opposite of both switches.
	if err := store.UpdateSuite(ctx, suite.ID, func(s *model.Suite) error {
		s.Items = []model.SuiteItem{{TemplateID: "template-lint"}}
		s.Mode, s.MaxPasses, s.Count = model.SuiteUntilClean, 9, 0
		s.RequireApproval, s.AutoMerge = true, false
		return nil
	}); err != nil {
		t.Fatalf("update suite: %v", err)
	}

	got, err := store.GetSuiteRun(ctx, run.ID)
	if err != nil || got == nil {
		t.Fatalf("get run: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(got.Items, suite.Items) {
		t.Errorf("got items %+v, want the two the run was created with", got.Items)
	}
	if got.Mode != model.SuiteCount || got.Count != 2 {
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
	run, err := store.CreateSuiteRun(ctx, suite, widgets, "main", now)
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

func TestSuiteRunTasksReportPullRequestsAndProposals(t *testing.T) {
	// The two reads OutcomeOfPass' "clean" decision is built from, taken
	// straight off task_link the way finishWithPullRequest and
	// relayProposedTasks write them.
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	run, err := store.CreateSuiteRun(ctx, suite, widgets, "main", now)
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

	got, err := store.GetSuiteRun(ctx, run.ID)
	if err != nil || got == nil {
		t.Fatalf("get run: (%+v, %v)", got, err)
	}
	by := map[string]model.SuiteTaskStatus{}
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

func TestActiveSuiteRunsAndCompleteSuiteRun(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	first, err := store.CreateSuiteRun(ctx, suite, widgets, "main", now)
	if err != nil {
		t.Fatalf("create first run: %v", err)
	}
	second, err := store.CreateSuiteRun(ctx, suite, gadgets, "main", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("create second run: %v", err)
	}

	active, err := store.ActiveSuiteRuns(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("got %d active runs, want both", len(active))
	}

	done := now.Add(time.Hour)
	if err := store.CompleteSuiteRun(ctx, first.ID, model.SuiteRunFailed, "a task failed", done); err != nil {
		t.Fatalf("complete: %v", err)
	}
	active, err = store.ActiveSuiteRuns(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if len(active) != 1 || active[0].ID != second.ID {
		t.Fatalf("got %+v, want only the still-active run %d", active, second.ID)
	}

	got, err := store.GetSuiteRun(ctx, first.ID)
	if err != nil || got == nil {
		t.Fatalf("get run: (%+v, %v)", got, err)
	}
	if got.Status != model.SuiteRunFailed || got.LastError != "a task failed" {
		t.Fatalf("got status %q error %q, want failed / a task failed", got.Status, got.LastError)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(done) {
		t.Fatalf("got completedAt %v, want %v", got.CompletedAt, done)
	}

	// A succeeded run carries no error at all.
	if err := store.CompleteSuiteRun(ctx, second.ID, model.SuiteRunSucceeded, "", done); err != nil {
		t.Fatalf("complete second: %v", err)
	}
	got, err = store.GetSuiteRun(ctx, second.ID)
	if err != nil || got == nil {
		t.Fatalf("get second run: (%+v, %v)", got, err)
	}
	if got.Status != model.SuiteRunSucceeded || got.LastError != "" {
		t.Fatalf("got status %q error %q, want succeeded and no error", got.Status, got.LastError)
	}
}

// A run a schedule fired records the schedule it came from, and
// HasActiveRunForSchedule reads it back -- the idempotency gate
// orchestrator.fireSuiteSchedule needs, and the exact counterpart of
// HasOpenTaskWithTag for a schedule that files a task instead. A
// database created before a schedule could fire a suite has a suite_run
// with no schedule_id column, which CREATE TABLE IF NOT EXISTS never
// adds -- Store.Init's own migration step
// (ensureSuiteRunScheduleColumn) is what does, leaving every run
// already on it reading as what it was: started by a human, not by a
// schedule.
func TestInitMigratesAnExistingDatabaseWithNoRunScheduleColumn(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`suite_run`"+` (
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
		t.Fatalf("creating the pre-schedule suite_run table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `suite_run` (`suite_id`,`suite_name`,`owner`,`repo`,`base`,`mode`,`count`,"+
			"`max_passes`,`require_approval`,`auto_merge`,`status`,`created_at`) "+
			"VALUES ('suite-1','nightly','acme','widgets','main','count',2,0,0,1,'active',?)", now); err != nil {
		t.Fatalf("seeding a pre-schedule run row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database with no schedule_id: %v", err)
	}

	got, err := store.GetSuiteRun(ctx, 1)
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

	byHand, err := store.CreateSuiteRun(ctx, suite, widgets, "main", now)
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
	if err := store.CompleteSuiteRun(ctx, fired.ID, model.SuiteRunSucceeded, "", now.Add(time.Hour)); err != nil {
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

func TestListSuiteRunsIsNewestFirst(t *testing.T) {
	store, _, ctx := openStore(t)
	suite := putSuite(t, ctx, store, twoItemSuite())
	older, err := store.CreateSuiteRun(ctx, suite, widgets, "main", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	newer, err := store.CreateSuiteRun(ctx, suite, gadgets, "main", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.ListSuiteRuns(ctx)
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

func TestGetSuiteRunReturnsNilForAnUnknownID(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetSuiteRun(ctx, 404)
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) for a run that does not exist, got (%+v, %v)", got, err)
	}
}

// The feature was called "task suites" before it was called suites
// (docs/schedules.md), and its six tables were named for it. This
// simulates a database built under the old names -- directly rather
// than through Store, since Store no longer knows how to write one --
// and checks Store.Init's own renameTemplateAndSuiteTables step carries
// it onto the new ones with every row, child row, sequence position and
// index intact.
func TestInitRenamesTheOldTaskSuiteTables(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	for _, stmt := range []string{
		"CREATE TABLE `task_suite` (" +
			"`id` TEXT NOT NULL, `name` TEXT NOT NULL, `mode` TEXT NOT NULL," +
			"`count` INTEGER NOT NULL, `max_passes` INTEGER NOT NULL," +
			"`require_approval` INTEGER NOT NULL, `auto_merge` INTEGER NOT NULL," +
			"`created_at` DATETIME NOT NULL, PRIMARY KEY (`id`))",
		"CREATE TABLE `task_suite_sequence` (" +
			"`id` INTEGER PRIMARY KEY AUTOINCREMENT, `issued_at` DATETIME NOT NULL)",
		"CREATE TABLE `task_suite_item` (" +
			"`id` INTEGER PRIMARY KEY AUTOINCREMENT, `suite_id` TEXT NOT NULL," +
			"`template_id` TEXT NOT NULL, `order_key` REAL NOT NULL)",
		"CREATE INDEX `task_suite_item_suite` ON `task_suite_item` (`suite_id`, `order_key`)",
		"CREATE TABLE `task_suite_run` (" +
			"`id` INTEGER PRIMARY KEY AUTOINCREMENT, `suite_id` TEXT NOT NULL," +
			"`suite_name` TEXT NOT NULL, `schedule_id` TEXT NULL, `owner` TEXT NOT NULL," +
			"`repo` TEXT NOT NULL, `base` TEXT NOT NULL, `mode` TEXT NOT NULL," +
			"`count` INTEGER NOT NULL, `max_passes` INTEGER NOT NULL," +
			"`require_approval` INTEGER NOT NULL, `auto_merge` INTEGER NOT NULL," +
			"`status` TEXT NOT NULL, `last_error` TEXT NULL," +
			"`created_at` DATETIME NOT NULL, `completed_at` DATETIME NULL)",
		"CREATE INDEX `task_suite_run_status` ON `task_suite_run` (`status`)",
		"CREATE TABLE `task_suite_run_item` (" +
			"`id` INTEGER PRIMARY KEY AUTOINCREMENT, `run_id` INTEGER NOT NULL," +
			"`template_id` TEXT NOT NULL, `order_key` REAL NOT NULL)",
		"CREATE INDEX `task_suite_run_item_run` ON `task_suite_run_item` (`run_id`, `order_key`)",
		"CREATE TABLE `task_suite_run_task` (" +
			"`id` INTEGER PRIMARY KEY AUTOINCREMENT, `run_id` INTEGER NOT NULL," +
			"`task_id` TEXT NOT NULL, `template_id` TEXT NOT NULL," +
			"`template_name` TEXT NOT NULL, `pass_number` INTEGER NOT NULL)",
		"CREATE INDEX `task_suite_run_task_run` ON `task_suite_run_task` (`run_id`)",
		"CREATE UNIQUE INDEX `task_suite_run_task_task` ON `task_suite_run_task` (`task_id`)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("creating the pre-rename tables: %v", err)
		}
	}
	for _, seed := range []struct {
		what string
		stmt string
		args []any
	}{
		{"suite", "INSERT INTO `task_suite` (`id`,`name`,`mode`,`count`,`max_passes`," +
			"`require_approval`,`auto_merge`,`created_at`) " +
			"VALUES ('suite-2','nightly','count',2,0,0,1,?)", []any{now}},
		{"suite item", "INSERT INTO `task_suite_item` (`suite_id`,`template_id`,`order_key`) " +
			"VALUES ('suite-2','template-smoke',1.0)", nil},
		{"run", "INSERT INTO `task_suite_run` (`suite_id`,`suite_name`,`owner`,`repo`,`base`," +
			"`mode`,`count`,`max_passes`,`require_approval`,`auto_merge`,`status`,`created_at`) " +
			"VALUES ('suite-2','nightly','acme','widgets','main','count',2,0,0,1,'active',?)", []any{now}},
		{"run item", "INSERT INTO `task_suite_run_item` (`run_id`,`template_id`,`order_key`) " +
			"VALUES (1,'template-smoke',1.0)", nil},
		{"run task", "INSERT INTO `task_suite_run_task` (`run_id`,`task_id`,`template_id`," +
			"`template_name`,`pass_number`) VALUES (1,'task-1','template-smoke','Smoke',1)", nil},
	} {
		if _, err := db.ExecContext(ctx, seed.stmt, seed.args...); err != nil {
			t.Fatalf("seeding a pre-rename %s: %v", seed.what, err)
		}
	}
	// Two ids already issued, so the sequence has somewhere to carry
	// from: an id allocated after the rename must not collide with
	// suite-2 above.
	for i := 0; i < 2; i++ {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO `task_suite_sequence` (`issued_at`) VALUES (?)", now); err != nil {
			t.Fatalf("seeding the pre-rename sequence: %v", err)
		}
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against a database written before the rename: %v", err)
	}

	got, err := store.GetSuite(ctx, "suite-2")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Name != "nightly" || got.Mode != model.SuiteCount || got.Count != 2 ||
		len(got.Items) != 1 || got.Items[0].TemplateID != "template-smoke" {
		t.Fatalf("got %+v, want the pre-rename suite intact under the new table name", got)
	}

	// The run came across whole: its own snapshot of the suite's items,
	// and the task its first pass filed -- which needs suite_run_task to
	// have arrived with it, since that is what the task is read through.
	if err := store.PutTask(ctx, task("task-1", true)); err != nil {
		t.Fatalf("put the run's own task: %v", err)
	}
	run, err := store.GetSuiteRun(ctx, 1)
	if err != nil || run == nil {
		t.Fatalf("get run: (%+v, %v)", run, err)
	}
	if run.SuiteName != "nightly" || len(run.Items) != 1 ||
		len(run.Tasks) != 1 || run.Tasks[0].TaskID != "task-1" || run.Tasks[0].PassNumber != 1 {
		t.Fatalf("got %+v, want the pre-rename run, its items and its tasks intact", run)
	}

	// SQLite carries an index across a table rename under its own old
	// name, so the rename drops the five old ones and lets the DDL
	// create them again: the old names are gone, and task_id is still
	// unique on suite_run_task.
	for _, index := range []string{"task_suite_item_suite", "task_suite_run_status",
		"task_suite_run_item_run", "task_suite_run_task_run", "task_suite_run_task_task"} {
		var name string
		err := db.QueryRowContext(ctx,
			"SELECT `name` FROM `sqlite_master` WHERE `type` = 'index' AND `name` = ?",
			index).Scan(&name)
		if err == nil {
			t.Errorf("index %s is still there under its pre-rename name", index)
		}
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `suite_run_task` (`run_id`,`task_id`,`template_id`,`template_name`,`pass_number`) "+
			"VALUES (1,'task-1','template-smoke','Smoke',2)"); err == nil {
		t.Error("a second suite_run_task row for task-1 was accepted, so the unique index is gone")
	}

	// The sequence came across too, rather than restarting at 1 and
	// handing out an id suite-2 already has.
	id, err := store.NewSuiteID(ctx)
	if err != nil {
		t.Fatalf("allocating an id after the rename: %v", err)
	}
	if id != "suite-3" {
		t.Errorf("NewSuiteID after the rename = %q, want suite-3", id)
	}
}

// The actor a suite run attributes its tasks to was "task-suite" while
// the feature was called task suites, and is plain "suite" now that it
// is not. This seeds a task carrying the old actor -- in both places a
// run writes one, its origin and the approval it stamps itself -- and
// checks Store.Init's own renameSuitePrincipal step brings it onto the
// new id, so a task filed before the rename and one filed after it read
// as the same mechanism's work.
func TestInitRenamesTheOldSuitePrincipal(t *testing.T) {
	store, _, ctx := openStore(t)

	old := model.Principal{Kind: model.PrincipalAutomation, ID: "task-suite"}
	filed := task("task-1", false)
	filed.Origin = model.Origin{
		Attribution: model.Attribution{Actor: old},
		Reason:      model.ReasonSuite,
	}
	filed.Approval = &model.Attribution{Actor: old}
	if err := store.PutTask(ctx, filed); err != nil {
		t.Fatalf("put a task filed before the rename: %v", err)
	}
	// A task nothing about suites filed, to check the migration stays on
	// its own rows.
	if err := store.PutTask(ctx, task("task-2", true)); err != nil {
		t.Fatalf("put an unrelated task: %v", err)
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against a database written before the rename: %v", err)
	}

	got, err := store.GetTask(ctx, "task-1")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Origin.Attribution.Actor != model.SuitePrincipal {
		t.Errorf("origin actor = %+v, want SuitePrincipal", got.Origin.Attribution.Actor)
	}
	if got.Approval == nil || got.Approval.Actor != model.SuitePrincipal {
		t.Errorf("approval actor = %+v, want SuitePrincipal", got.Approval)
	}

	other, err := store.GetTask(ctx, "task-2")
	if err != nil || other == nil {
		t.Fatalf("get: (%+v, %v)", other, err)
	}
	if other.Origin.Attribution.Actor == model.SuitePrincipal {
		t.Errorf("origin actor = %+v, want the human who actually filed it", other.Origin.Attribution.Actor)
	}
}
