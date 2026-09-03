package sqlite_test

// The task-template store (bwsalmon/agents#516), against a real embedded
// SQLite database -- store_test.go's own doc comment on why gives the
// reasoning again here, and schedule_store_test.go's own shape is the
// pattern this file follows almost field for field.

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
)

func template(id string) model.TaskTemplate {
	return model.TaskTemplate{
		ID:        id,
		Name:      "Dependency bump",
		Title:     "Bump dependencies",
		Body:      "Bump every dependency to its latest patch release.",
		CreatedAt: now,
	}
}

func TestTaskTemplateRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	want := template("template-1")
	want.AutoMerge = true
	want.Reads = []model.RepoRef{{Owner: "owner", Name: "shared-lib"}}
	want.Grants = []model.Grant{{Capability: "web-search", Via: model.GrantByLabel}}

	if err := store.PutTaskTemplate(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetTaskTemplate(ctx, "template-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.Name != want.Name || got.Title != want.Title || got.Body != want.Body {
		t.Errorf("text did not survive: %+v", got)
	}
	if !got.AutoMerge {
		t.Errorf("declared fields did not survive: %+v", got)
	}
	if len(got.Reads) != 1 || got.Reads[0] != want.Reads[0] {
		t.Errorf("reads = %+v, want %+v", got.Reads, want.Reads)
	}
	if len(got.Grants) != 1 || got.Grants[0].Capability != "web-search" {
		t.Errorf("grants = %+v, want web-search", got.Grants)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("createdAt = %v, want %v", got.CreatedAt, now)
	}
}

func TestGetTaskTemplateReturnsNilWhenAbsent(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetTaskTemplate(ctx, "nope")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
}

func TestNewTaskTemplateIDsAreDistinctFromTaskAndScheduleIDs(t *testing.T) {
	store, _, ctx := openStore(t)
	taskID, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schedID, err := store.NewScheduleID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	templateID, err := store.NewTaskTemplateID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if templateID == taskID || templateID == schedID {
		t.Fatalf("template id %q collided with task id %q or schedule id %q", templateID, taskID, schedID)
	}
	if templateID[:9] != "template-" {
		t.Errorf("template id = %q, want a template- prefix", templateID)
	}
}

func TestListTaskTemplatesReturnsNewestFirst(t *testing.T) {
	store, _, ctx := openStore(t)
	earlier := template("template-1")
	earlier.CreatedAt = now.Add(-time.Hour)
	later := template("template-2")
	later.CreatedAt = now
	for _, tmpl := range []model.TaskTemplate{earlier, later} {
		if err := store.PutTaskTemplate(ctx, tmpl); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.ListTaskTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "template-2" || got[1].ID != "template-1" {
		t.Fatalf("list = %+v, want [template-2, template-1]", got)
	}
}

func TestUpdateTaskTemplateAppliesAndPersists(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTaskTemplate(ctx, template("template-1")); err != nil {
		t.Fatal(err)
	}
	err := store.UpdateTaskTemplate(ctx, "template-1", func(tmpl *model.TaskTemplate) error {
		tmpl.Title = "Bump dependencies (patch only)"
		tmpl.AutoMerge = true
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.GetTaskTemplate(ctx, "template-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Bump dependencies (patch only)" || !got.AutoMerge {
		t.Errorf("update did not persist: %+v", got)
	}
}

func TestUpdateTaskTemplateOnAnUnknownIDErrors(t *testing.T) {
	store, _, ctx := openStore(t)
	err := store.UpdateTaskTemplate(ctx, "nope", func(*model.TaskTemplate) error { return nil })
	if err == nil {
		t.Fatal("want an error updating an unknown template, got nil")
	}
}

func TestDeleteTaskTemplateRemovesIt(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTaskTemplate(ctx, template("template-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTaskTemplate(ctx, "template-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := store.GetTaskTemplate(ctx, "template-1")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) after delete, got (%v, %v)", got, err)
	}
}

func TestSchedulesUsingTemplate(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTaskTemplate(ctx, template("template-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTaskTemplate(ctx, template("template-2")); err != nil {
		t.Fatal(err)
	}
	id := "template-1"
	pointsAtOne := schedule("sched-1", now)
	pointsAtOne.TemplateID = &id
	pointsAtNone := schedule("sched-2", now)
	for _, s := range []model.Schedule{pointsAtOne, pointsAtNone} {
		if err := store.PutSchedule(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.SchedulesUsingTemplate(ctx, "template-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "sched-1" {
		t.Fatalf("schedules using template-1 = %+v, want only sched-1", got)
	}

	got, err = store.SchedulesUsingTemplate(ctx, "template-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("schedules using template-2 = %+v, want none", got)
	}
}

// TestInitMigratesAnExistingDatabaseWithNoTemplateIDColumn simulates a
// database built before bwsalmon/agents#516 -- schedule with none of the
// columns task_template introduced, template_id included -- the same
// "build the old shape directly, then run Init" approach
// TestInitMigratesAnExistingDatabaseWithBareIntervalMS already uses for
// the recurrence-columns migration.
func TestInitMigratesAnExistingDatabaseWithNoTemplateIDColumn(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`schedule`"+` (
  `+"`id`"+`                    TEXT     NOT NULL,
  `+"`title`"+`                 TEXT     NOT NULL,
  `+"`body`"+`                  TEXT     NOT NULL,
  `+"`target_owner`"+`          TEXT     NOT NULL,
  `+"`target_name`"+`           TEXT     NOT NULL,
  `+"`base`"+`                  TEXT     NULL,
  `+"`auto_merge`"+`            INTEGER  NOT NULL,
  `+"`recurrence_kind`"+`       TEXT     NOT NULL,
  `+"`every_n_hours`"+`         INTEGER  NULL,
  `+"`time_of_day_minutes`"+`   INTEGER  NULL,
  `+"`weekday`"+`               INTEGER  NULL,
  `+"`day_of_month`"+`          INTEGER  NULL,
  `+"`enabled`"+`               INTEGER  NOT NULL,
  `+"`next_run_at`"+`           DATETIME NOT NULL,
  `+"`last_run_at`"+`           DATETIME NULL,
  `+"`created_at`"+`            DATETIME NOT NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#516 schedule table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `schedule` (`id`,`title`,`body`,`target_owner`,`target_name`,`base`,"+
			"`auto_merge`,`recurrence_kind`,`every_n_hours`,`enabled`,`next_run_at`,`last_run_at`,`created_at`) "+
			"VALUES ('sched-1','Nightly dependency bump','Bump every dependency.','owner','payments-api',NULL,"+
			"0,'everyNHours',24,1,?,NULL,?)", now, now); err != nil {
		t.Fatalf("seeding a pre-#516 schedule row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database with no template_id column: %v", err)
	}

	got, err := store.GetSchedule(ctx, "sched-1")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Title != "Nightly dependency bump" || got.TemplateID != nil {
		t.Fatalf("got %+v, want the pre-existing row intact with a nil TemplateID", got)
	}

	// The new column is writable, not merely readable -- PutSchedule now
	// supplies it on every write.
	id := "template-1"
	withTemplate := schedule("sched-2", now)
	withTemplate.TemplateID = &id
	if err := store.PutSchedule(ctx, withTemplate); err != nil {
		t.Fatalf("put with a template id after migrating: %v", err)
	}
}

// TestInitMigratesAnExistingDatabaseWithTaskTemplateTargetColumns
// simulates a database built while task_template still carried its own
// target_owner/target_name/base (model.TaskTemplate's own doc comment on
// why they moved out) directly rather than through Store, and checks
// Store.Init's own migration step (ensureTaskTemplateNoTargetColumns)
// drops those three columns without disturbing the rest of the row --
// store_test.go's own TestInitMigratesAnExistingDatabaseWithNamedSlots
// pattern, applied to a drop with no data to backfill anywhere else.
func TestInitMigratesAnExistingDatabaseWithTaskTemplateTargetColumns(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`task_template`"+` (
  `+"`id`"+`           TEXT     NOT NULL,
  `+"`name`"+`         TEXT     NOT NULL,
  `+"`title`"+`        TEXT     NOT NULL,
  `+"`body`"+`         TEXT     NOT NULL,
  `+"`target_owner`"+` TEXT     NOT NULL,
  `+"`target_name`"+`  TEXT     NOT NULL,
  `+"`base`"+`         TEXT     NULL,
  `+"`auto_merge`"+`   INTEGER  NOT NULL,
  `+"`created_at`"+`   DATETIME NOT NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-target-removal task_template table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task_template` (`id`,`name`,`title`,`body`,`target_owner`,`target_name`,`base`,"+
			"`auto_merge`,`created_at`) VALUES "+
			"('template-1','Dependency bump','Bump dependencies','Bump every dependency.','owner','payments-api',NULL,0,?)",
		now); err != nil {
		t.Fatalf("seeding a pre-target-removal template row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database with task_template.target_owner: %v", err)
	}

	got, err := store.GetTaskTemplate(ctx, "template-1")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Name != "Dependency bump" || got.Title != "Bump dependencies" {
		t.Fatalf("got %+v, want the pre-existing row intact", got)
	}

	// The old columns are gone, not merely ignored -- PutTaskTemplate
	// stops supplying them, so target_owner/target_name would otherwise
	// fail every write with a NOT NULL constraint violation.
	if err := store.PutTaskTemplate(ctx, template("template-2")); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
}
