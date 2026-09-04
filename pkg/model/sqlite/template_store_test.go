package sqlite_test

// The template store (bwsalmon/agents#516), against a real embedded
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

func template(id string) model.Template {
	return model.Template{
		ID:        id,
		Name:      "Dependency bump",
		Title:     "Bump dependencies",
		Body:      "Bump every dependency to its latest patch release.",
		CreatedAt: now,
	}
}

func TestTemplateRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	want := template("template-1")
	want.AutoMerge = true
	want.Reads = []model.RepoRef{{Owner: "owner", Name: "shared-lib"}}
	want.Grants = []model.Grant{{Capability: "web-search", Via: model.GrantByLabel}}

	if err := store.PutTemplate(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetTemplate(ctx, "template-1")
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
	if got.Target != nil || got.Base != "" {
		t.Errorf("binding = (%+v, %q), want a template bound to nothing", got.Target, got.Base)
	}
}

// A binding is optional, so both halves of the choice need to survive a
// round trip: the unbound template above, and this one, bound to a repo
// and a branch (grain/task-285).
func TestBoundTemplateRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	want := template("template-1")
	want.Target = &model.RepoRef{Owner: "owner", Name: "payments-api"}
	want.Base = "release"

	if err := store.PutTemplate(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetTemplate(ctx, "template-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.Target == nil || *got.Target != *want.Target || got.Base != "release" {
		t.Fatalf("binding did not survive: (%+v, %q)", got.Target, got.Base)
	}

	// Unbinding is a write like any other, and has to clear the repo
	// rather than leave the old one readable underneath.
	want.Target, want.Base = nil, ""
	if err := store.PutTemplate(ctx, want); err != nil {
		t.Fatalf("put unbound: %v", err)
	}
	got, err = store.GetTemplate(ctx, "template-1")
	if err != nil || got == nil {
		t.Fatalf("get after unbinding: %v (nil=%v)", err, got == nil)
	}
	if got.Target != nil || got.Base != "" {
		t.Fatalf("binding = (%+v, %q), want it cleared", got.Target, got.Base)
	}
}

// ListTemplates reads through its own SELECT rather than getTemplate's,
// so the binding it scans is worth checking separately -- a column list
// that drifts between the two would otherwise only show up as a
// template that reads bound one way and unbound the other.
func TestListTemplatesReadsBindings(t *testing.T) {
	store, _, ctx := openStore(t)
	bound := template("template-1")
	bound.Target = &model.RepoRef{Owner: "owner", Name: "payments-api"}
	bound.Base = "release"
	if err := store.PutTemplate(ctx, bound); err != nil {
		t.Fatalf("put bound: %v", err)
	}
	unbound := template("template-2")
	if err := store.PutTemplate(ctx, unbound); err != nil {
		t.Fatalf("put unbound: %v", err)
	}

	list, err := store.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]model.Template{}
	for _, tmpl := range list {
		byID[tmpl.ID] = tmpl
	}
	if got := byID["template-1"]; got.Target == nil || *got.Target != *bound.Target || got.Base != "release" {
		t.Errorf("template-1 = (%+v, %q), want its binding", got.Target, got.Base)
	}
	if got := byID["template-2"]; got.Target != nil || got.Base != "" {
		t.Errorf("template-2 = (%+v, %q), want no binding", got.Target, got.Base)
	}
}

func TestGetTemplateReturnsNilWhenAbsent(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetTemplate(ctx, "nope")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
}

func TestNewTemplateIDsAreDistinctFromTaskAndScheduleIDs(t *testing.T) {
	store, _, ctx := openStore(t)
	taskID, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schedID, err := store.NewScheduleID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	templateID, err := store.NewTemplateID(ctx)
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

func TestListTemplatesReturnsNewestFirst(t *testing.T) {
	store, _, ctx := openStore(t)
	earlier := template("template-1")
	earlier.CreatedAt = now.Add(-time.Hour)
	later := template("template-2")
	later.CreatedAt = now
	for _, tmpl := range []model.Template{earlier, later} {
		if err := store.PutTemplate(ctx, tmpl); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "template-2" || got[1].ID != "template-1" {
		t.Fatalf("list = %+v, want [template-2, template-1]", got)
	}
}

func TestUpdateTemplateAppliesAndPersists(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTemplate(ctx, template("template-1")); err != nil {
		t.Fatal(err)
	}
	err := store.UpdateTemplate(ctx, "template-1", func(tmpl *model.Template) error {
		tmpl.Title = "Bump dependencies (patch only)"
		tmpl.AutoMerge = true
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.GetTemplate(ctx, "template-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Bump dependencies (patch only)" || !got.AutoMerge {
		t.Errorf("update did not persist: %+v", got)
	}
}

func TestUpdateTemplateOnAnUnknownIDErrors(t *testing.T) {
	store, _, ctx := openStore(t)
	err := store.UpdateTemplate(ctx, "nope", func(*model.Template) error { return nil })
	if err == nil {
		t.Fatal("want an error updating an unknown template, got nil")
	}
}

func TestDeleteTemplateRemovesIt(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTemplate(ctx, template("template-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTemplate(ctx, "template-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := store.GetTemplate(ctx, "template-1")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) after delete, got (%v, %v)", got, err)
	}
}

func TestSchedulesUsingTemplate(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTemplate(ctx, template("template-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTemplate(ctx, template("template-2")); err != nil {
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
// database built before bwsalmon/agents#516 -- schedule with none of
// the columns template introduced, template_id included -- the same
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

// TestInitMigratesAnExistingDatabaseWithTemplateTargetColumns simulates
// a database built while a template *had* to carry a
// target_owner/target_name/base, directly rather than through Store, and
// checks Store.Init leaves those three columns and their values alone:
// what was a mandatory target is exactly what a binding means now
// (model.Template's own doc comment), so such a row upgrades into a
// bound template rather than losing where it pointed.
func TestInitMigratesAnExistingDatabaseWithTemplateTargetColumns(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`template`"+` (
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
		t.Fatalf("creating the pre-target-removal template table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `template` (`id`,`name`,`title`,`body`,`target_owner`,`target_name`,`base`,"+
			"`auto_merge`,`created_at`) VALUES "+
			"('template-1','Dependency bump','Bump dependencies','Bump every dependency.','owner','payments-api',NULL,0,?)",
		now); err != nil {
		t.Fatalf("seeding a pre-target-removal template row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database with template.target_owner: %v", err)
	}

	got, err := store.GetTemplate(ctx, "template-1")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Name != "Dependency bump" || got.Title != "Bump dependencies" {
		t.Fatalf("got %+v, want the pre-existing row intact", got)
	}
	if got.Target == nil || *got.Target != (model.RepoRef{Owner: "owner", Name: "payments-api"}) {
		t.Fatalf("target = %+v, want the row's own target read back as a binding", got.Target)
	}
	if got.Base != "" {
		t.Fatalf("base = %q, want a binding with no branch pinned", got.Base)
	}

	// Writable as well as readable: the columns are NOT NULL in this old
	// shape, and PutTemplate supplies them on every write, bound or not.
	if err := store.PutTemplate(ctx, template("template-2")); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
}

// TestInitMigratesAnExistingDatabaseWithNoTemplateTargetColumns is the
// other direction: a database built while a template could carry no
// target at all (the era between the two), which Init has to give the
// binding columns back -- every template in it staying unbound, which is
// what it was.
func TestInitMigratesAnExistingDatabaseWithNoTemplateTargetColumns(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`template`"+` (
  `+"`id`"+`           TEXT     NOT NULL,
  `+"`name`"+`         TEXT     NOT NULL,
  `+"`title`"+`        TEXT     NOT NULL,
  `+"`body`"+`         TEXT     NOT NULL,
  `+"`auto_merge`"+`   INTEGER  NOT NULL,
  `+"`created_at`"+`   DATETIME NOT NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the targetless template table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `template` (`id`,`name`,`title`,`body`,`auto_merge`,`created_at`) VALUES "+
			"('template-1','Dependency bump','Bump dependencies','Bump every dependency.',0,?)",
		now); err != nil {
		t.Fatalf("seeding a targetless template row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database with no template.target_owner: %v", err)
	}

	got, err := store.GetTemplate(ctx, "template-1")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Target != nil || got.Base != "" {
		t.Fatalf("got %+v, want a template that stayed unbound", got)
	}

	bound := template("template-2")
	bound.Target = &model.RepoRef{Owner: "owner", Name: "payments-api"}
	bound.Base = "release"
	if err := store.PutTemplate(ctx, bound); err != nil {
		t.Fatalf("put a bound template after migrating: %v", err)
	}
	back, err := store.GetTemplate(ctx, "template-2")
	if err != nil || back == nil {
		t.Fatalf("get after migrating: (%+v, %v)", back, err)
	}
	if back.Target == nil || *back.Target != *bound.Target || back.Base != "release" {
		t.Fatalf("binding did not survive the added columns: %+v", back)
	}
}

// The feature was called "task templates" before it was called
// templates (docs/schedules.md), and its four tables were named for it.
// This simulates a database built under the old names -- directly
// rather than through Store, since Store no longer knows how to write
// one -- and checks Store.Init's own renameTemplateAndSuiteTables step
// carries it onto the new ones with every row, child row and sequence
// position intact.
func TestInitRenamesTheOldTaskTemplateTables(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	for _, stmt := range []string{
		"CREATE TABLE `task_template` (" +
			"`id` TEXT NOT NULL, `name` TEXT NOT NULL, `title` TEXT NOT NULL," +
			"`body` TEXT NOT NULL, `auto_merge` INTEGER NOT NULL," +
			"`created_at` DATETIME NOT NULL, PRIMARY KEY (`id`))",
		"CREATE TABLE `task_template_sequence` (" +
			"`number` INTEGER PRIMARY KEY AUTOINCREMENT, `issued_at` DATETIME NOT NULL)",
		"CREATE TABLE `task_template_read` (" +
			"`task_template_id` TEXT NOT NULL, `owner` TEXT NOT NULL, `name` TEXT NOT NULL," +
			"PRIMARY KEY (`task_template_id`, `owner`, `name`))",
		"CREATE TABLE `task_template_grant` (" +
			"`task_template_id` TEXT NOT NULL, `capability` TEXT NOT NULL," +
			"`via` TEXT NOT NULL, `folder` TEXT NULL, PRIMARY KEY (`task_template_id`, `capability`))",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("creating the pre-rename tables: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task_template` (`id`,`name`,`title`,`body`,`auto_merge`,`created_at`) "+
			"VALUES ('template-3','Dependency bump','Bump dependencies','Bump every dependency.',1,?)",
		now); err != nil {
		t.Fatalf("seeding a pre-rename template row: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task_template_read` (`task_template_id`,`owner`,`name`) "+
			"VALUES ('template-3','owner','shared-lib')"); err != nil {
		t.Fatalf("seeding a pre-rename read: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `task_template_grant` (`task_template_id`,`capability`,`via`,`folder`) "+
			"VALUES ('template-3','web-search','label',NULL)"); err != nil {
		t.Fatalf("seeding a pre-rename grant: %v", err)
	}
	// Three ids already issued, so the sequence has somewhere to carry
	// from: an id allocated after the rename must not collide with
	// template-3 above.
	for i := 0; i < 3; i++ {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO `task_template_sequence` (`issued_at`) VALUES (?)", now); err != nil {
			t.Fatalf("seeding the pre-rename sequence: %v", err)
		}
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against a database written before the rename: %v", err)
	}

	got, err := store.GetTemplate(ctx, "template-3")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Name != "Dependency bump" || got.Title != "Bump dependencies" || !got.AutoMerge {
		t.Fatalf("got %+v, want the pre-rename row intact under the new table name", got)
	}
	if len(got.Reads) != 1 || got.Reads[0] != (model.RepoRef{Owner: "owner", Name: "shared-lib"}) {
		t.Errorf("reads = %+v, want the pre-rename child row", got.Reads)
	}
	if len(got.Grants) != 1 || got.Grants[0].Capability != "web-search" {
		t.Errorf("grants = %+v, want the pre-rename child row", got.Grants)
	}

	// The sequence came across too, rather than restarting at 1 and
	// handing out an id template-3 already has.
	id, err := store.NewTemplateID(ctx)
	if err != nil {
		t.Fatalf("allocating an id after the rename: %v", err)
	}
	if id != "template-4" {
		t.Errorf("NewTemplateID after the rename = %q, want template-4", id)
	}

	// And the renamed tables are writable under their new names, which
	// is what every later PutTemplate needs.
	if err := store.PutTemplate(ctx, template(id)); err != nil {
		t.Fatalf("put after the rename: %v", err)
	}
}
