package sqlite_test

// The schedule store, against a real embedded SQLite database --
// store_test.go's own doc comment on why gives the reasoning again here.

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
)

func schedule(id string, nextRunAt time.Time) model.Schedule {
	return model.Schedule{
		ID:         id,
		Title:      "Nightly dependency bump",
		Body:       "Bump every dependency to its latest patch release.",
		Target:     model.RepoRef{Owner: "owner", Name: "payments-api"},
		Recurrence: model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24},
		Enabled:    true,
		NextRunAt:  nextRunAt,
		CreatedAt:  now,
	}
}

func TestScheduleRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	want := schedule("sched-1", now)
	want.Base = "main"
	want.AutoMerge = true
	want.Reads = []model.RepoRef{{Owner: "owner", Name: "shared-lib"}}
	want.Grants = []model.Grant{{Capability: "web-search", Via: model.GrantByLabel}}
	last := now.Add(-24 * time.Hour)
	want.LastRunAt = &last

	if err := store.PutSchedule(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetSchedule(ctx, "sched-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.Title != want.Title || got.Body != want.Body {
		t.Errorf("text did not survive: %+v", got)
	}
	if got.Target != want.Target {
		t.Errorf("target = %+v, want %+v", got.Target, want.Target)
	}
	if got.Base != "main" || !got.AutoMerge {
		t.Errorf("declared fields did not survive: %+v", got)
	}
	if got.Recurrence.Kind != model.RecurrenceEveryNHours || got.Recurrence.EveryNHours != 24 {
		t.Errorf("recurrence = %+v, want every 24 hours", got.Recurrence)
	}
	if len(got.Reads) != 1 || got.Reads[0] != want.Reads[0] {
		t.Errorf("reads = %+v, want %+v", got.Reads, want.Reads)
	}
	if len(got.Grants) != 1 || got.Grants[0].Capability != "web-search" {
		t.Errorf("grants = %+v, want web-search", got.Grants)
	}
	if !got.Enabled {
		t.Errorf("enabled did not survive")
	}
	if !got.NextRunAt.Equal(now) {
		t.Errorf("nextRunAt = %v, want %v", got.NextRunAt, now)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(last) {
		t.Errorf("lastRunAt = %v, want %v", got.LastRunAt, last)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("createdAt = %v, want %v", got.CreatedAt, now)
	}
}

// A schedule that runs a task suite instead of filing a task: SuiteID is
// what says so, and SchedulesUsingSuite is what reads it back for
// ui.Client.DeleteSuite's own "not out from under a schedule still
// running it" guard.
func TestSuiteBackedScheduleRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	suiteID := "suite-1"
	want := schedule("sched-1", now)
	want.Title = "nightly"
	want.Base = "main"
	want.SuiteID = &suiteID

	if err := store.PutSchedule(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetSchedule(ctx, "sched-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.SuiteID == nil || *got.SuiteID != suiteID {
		t.Fatalf("suiteId = %v, want %q", got.SuiteID, suiteID)
	}
	if got.TemplateID != nil {
		t.Errorf("templateId = %v, want nil: a schedule fires a suite or a task, never both", got.TemplateID)
	}

	using, err := store.SchedulesUsingSuite(ctx, suiteID)
	if err != nil {
		t.Fatalf("SchedulesUsingSuite: %v", err)
	}
	if len(using) != 1 || using[0].ID != "sched-1" {
		t.Fatalf("schedules using %s = %+v, want just sched-1", suiteID, using)
	}
	other, err := store.SchedulesUsingSuite(ctx, "suite-nobody-uses")
	if err != nil {
		t.Fatalf("SchedulesUsingSuite: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("schedules using an unreferenced suite = %+v, want none", other)
	}
}

func TestScheduleRoundTripsEveryRecurrenceKind(t *testing.T) {
	store, _, ctx := openStore(t)
	cases := []model.Recurrence{
		{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60},
		{Kind: model.RecurrenceWeekly, TimeOfDay: 14*60 + 30, Weekday: time.Monday},
		{Kind: model.RecurrenceMonthly, TimeOfDay: 0, DayOfMonth: 31},
	}
	for i, want := range cases {
		sched := schedule("sched-"+string(rune('a'+i)), now)
		sched.Recurrence = want
		if err := store.PutSchedule(ctx, sched); err != nil {
			t.Fatalf("put %+v: %v", want, err)
		}
		got, err := store.GetSchedule(ctx, sched.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Recurrence != want {
			t.Errorf("recurrence = %+v, want %+v", got.Recurrence, want)
		}
	}
}

func TestGetScheduleReturnsNilWhenAbsent(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetSchedule(ctx, "nope")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
}

func TestNewScheduleIDsAreDistinctFromTaskIDs(t *testing.T) {
	store, _, ctx := openStore(t)
	taskID, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schedID, err := store.NewScheduleID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if taskID == schedID {
		t.Fatalf("task id %q and schedule id %q collided", taskID, schedID)
	}
	if schedID[:6] != "sched-" {
		t.Errorf("schedule id = %q, want a sched- prefix", schedID)
	}
}

func TestListSchedulesReturnsNewestFirst(t *testing.T) {
	store, _, ctx := openStore(t)
	earlier := schedule("sched-1", now)
	earlier.CreatedAt = now.Add(-time.Hour)
	later := schedule("sched-2", now)
	later.CreatedAt = now
	for _, s := range []model.Schedule{earlier, later} {
		if err := store.PutSchedule(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.ListSchedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "sched-2" || got[1].ID != "sched-1" {
		t.Fatalf("list = %+v, want [sched-2, sched-1]", got)
	}
}

func TestDueSchedulesFiltersOnEnabledAndNextRunAt(t *testing.T) {
	store, _, ctx := openStore(t)
	due := schedule("sched-due", now.Add(-time.Minute))
	notYet := schedule("sched-not-yet", now.Add(time.Hour))
	paused := schedule("sched-paused", now.Add(-time.Minute))
	paused.Enabled = false
	for _, s := range []model.Schedule{due, notYet, paused} {
		if err := store.PutSchedule(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.DueSchedules(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "sched-due" {
		t.Fatalf("due = %+v, want only sched-due", got)
	}
}

func TestUpdateScheduleAppliesAndPersists(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutSchedule(ctx, schedule("sched-1", now)); err != nil {
		t.Fatal(err)
	}
	next := now.Add(24 * time.Hour)
	err := store.UpdateSchedule(ctx, "sched-1", func(s *model.Schedule) error {
		s.Enabled = false
		s.NextRunAt = next
		s.LastRunAt = &now
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.GetSchedule(ctx, "sched-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Errorf("enabled = true, want false after update")
	}
	if !got.NextRunAt.Equal(next) {
		t.Errorf("nextRunAt = %v, want %v", got.NextRunAt, next)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(now) {
		t.Errorf("lastRunAt = %v, want %v", got.LastRunAt, now)
	}
}

func TestUpdateScheduleOnAnUnknownIDErrors(t *testing.T) {
	store, _, ctx := openStore(t)
	err := store.UpdateSchedule(ctx, "nope", func(s *model.Schedule) error { return nil })
	if err == nil {
		t.Fatal("want an error updating an unknown schedule, got nil")
	}
}

func TestDeleteScheduleRemovesIt(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutSchedule(ctx, schedule("sched-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSchedule(ctx, "sched-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := store.GetSchedule(ctx, "sched-1")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) after delete, got (%v, %v)", got, err)
	}
}

// bwsalmon/agents#464: schedule.interval_ms (every N hours since a
// schedule last fired, no wall-clock alignment) became five columns
// behind model.Recurrence, so a schedule could also fire daily, weekly or
// monthly at a fixed time of day. This simulates a database built with
// the pre-#464 column set -- interval_ms present, the new recurrence
// columns absent -- directly rather than through Store, and checks
// Store.Init's own migration step (ensureScheduleRecurrenceColumns)
// backfills every_n_hours from interval_ms (rounded down to whole hours)
// and drops interval_ms, without disturbing the rest of the row.
func TestInitMigratesAnExistingDatabaseWithBareIntervalMS(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`schedule`"+` (
  `+"`id`"+`            TEXT     NOT NULL,
  `+"`title`"+`         TEXT     NOT NULL,
  `+"`body`"+`          TEXT     NOT NULL,
  `+"`target_owner`"+`  TEXT     NOT NULL,
  `+"`target_name`"+`   TEXT     NOT NULL,
  `+"`base`"+`          TEXT     NULL,
  `+"`auto_merge`"+`    INTEGER  NOT NULL,
  `+"`interval_ms`"+`   INTEGER  NOT NULL,
  `+"`enabled`"+`       INTEGER  NOT NULL,
  `+"`next_run_at`"+`   DATETIME NOT NULL,
  `+"`last_run_at`"+`   DATETIME NULL,
  `+"`created_at`"+`    DATETIME NOT NULL,
  PRIMARY KEY (`+"`id`"+`)
)`); err != nil {
		t.Fatalf("creating the pre-#464 schedule table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `schedule` (`id`,`title`,`body`,`target_owner`,`target_name`,`base`,"+
			"`auto_merge`,`interval_ms`,`enabled`,`next_run_at`,`last_run_at`,`created_at`) "+
			"VALUES ('sched-1','Nightly dependency bump','Bump every dependency.','owner','payments-api',NULL,"+
			"0,90000000,1,?,NULL,?)", now, now); err != nil {
		t.Fatalf("seeding a pre-#464 schedule row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database with a bare interval_ms: %v", err)
	}

	got, err := store.GetSchedule(ctx, "sched-1")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	// 90000000ms = 25h, rounded down to 25 whole hours.
	if got.Title != "Nightly dependency bump" ||
		got.Recurrence.Kind != model.RecurrenceEveryNHours || got.Recurrence.EveryNHours != 25 {
		t.Fatalf("got %+v, want the pre-existing row intact and recurrence migrated to every 25 hours", got)
	}

	// The old column is gone, not merely ignored -- PutSchedule stops
	// supplying it, so it would otherwise fail every write with a NOT
	// NULL constraint violation.
	want := schedule("sched-2", now)
	if err := store.PutSchedule(ctx, want); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
}

// The feature was called "scheduled tasks" before it was called
// schedules (docs/schedules.md), and its four tables were named for it.
// This simulates a database built under the old names -- directly rather
// than through Store, since Store no longer knows how to write one --
// and checks Store.Init's own renameScheduleTables step carries it onto
// the new ones with every row, child row and sequence position intact.
func TestInitRenamesTheOldScheduledTaskTables(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	for _, stmt := range []string{
		"CREATE TABLE `scheduled_task` (" +
			"`id` TEXT NOT NULL, `title` TEXT NOT NULL, `body` TEXT NOT NULL," +
			"`target_owner` TEXT NOT NULL, `target_name` TEXT NOT NULL, `base` TEXT NULL," +
			"`auto_merge` INTEGER NOT NULL, `template_id` TEXT NULL, `suite_id` TEXT NULL," +
			"`recurrence_kind` TEXT NOT NULL, `every_n_hours` INTEGER NULL," +
			"`time_of_day_minutes` INTEGER NULL, `weekday` INTEGER NULL, `day_of_month` INTEGER NULL," +
			"`enabled` INTEGER NOT NULL, `next_run_at` DATETIME NOT NULL," +
			"`last_run_at` DATETIME NULL, `created_at` DATETIME NOT NULL, PRIMARY KEY (`id`))",
		"CREATE TABLE `scheduled_task_sequence` (" +
			"`number` INTEGER PRIMARY KEY AUTOINCREMENT, `issued_at` DATETIME NOT NULL)",
		"CREATE TABLE `scheduled_task_read` (" +
			"`scheduled_task_id` TEXT NOT NULL, `owner` TEXT NOT NULL, `name` TEXT NOT NULL," +
			"PRIMARY KEY (`scheduled_task_id`, `owner`, `name`))",
		"CREATE TABLE `scheduled_task_grant` (" +
			"`scheduled_task_id` TEXT NOT NULL, `capability` TEXT NOT NULL," +
			"`via` TEXT NOT NULL, `folder` TEXT NULL, PRIMARY KEY (`scheduled_task_id`, `capability`))",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("creating the pre-rename tables: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `scheduled_task` (`id`,`title`,`body`,`target_owner`,`target_name`,`base`,"+
			"`auto_merge`,`template_id`,`suite_id`,`recurrence_kind`,`every_n_hours`,"+
			"`time_of_day_minutes`,`weekday`,`day_of_month`,`enabled`,`next_run_at`,`last_run_at`,`created_at`) "+
			"VALUES ('sched-7','Nightly dependency bump','Bump every dependency.','owner','payments-api','main',"+
			"1,NULL,NULL,'everyNHours',24,NULL,NULL,NULL,1,?,NULL,?)", now, now); err != nil {
		t.Fatalf("seeding a pre-rename schedule row: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `scheduled_task_read` (`scheduled_task_id`,`owner`,`name`) VALUES ('sched-7','owner','shared-lib')"); err != nil {
		t.Fatalf("seeding a pre-rename read: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `scheduled_task_grant` (`scheduled_task_id`,`capability`,`via`,`folder`) "+
			"VALUES ('sched-7','web-search','label',NULL)"); err != nil {
		t.Fatalf("seeding a pre-rename grant: %v", err)
	}
	// Seven ids already issued, so the sequence has somewhere to carry
	// from: an id allocated after the rename must not collide with
	// sched-7 above.
	for i := 0; i < 7; i++ {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO `scheduled_task_sequence` (`issued_at`) VALUES (?)", now); err != nil {
			t.Fatalf("seeding the pre-rename sequence: %v", err)
		}
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against a database written before the rename: %v", err)
	}

	got, err := store.GetSchedule(ctx, "sched-7")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if got.Title != "Nightly dependency bump" || got.Base != "main" || !got.AutoMerge ||
		got.Recurrence.Kind != model.RecurrenceEveryNHours || got.Recurrence.EveryNHours != 24 {
		t.Fatalf("got %+v, want the pre-rename row intact under the new table name", got)
	}
	if len(got.Reads) != 1 || got.Reads[0] != (model.RepoRef{Owner: "owner", Name: "shared-lib"}) {
		t.Errorf("reads = %+v, want the pre-rename child row", got.Reads)
	}
	if len(got.Grants) != 1 || got.Grants[0].Capability != "web-search" {
		t.Errorf("grants = %+v, want the pre-rename child row", got.Grants)
	}

	// The sequence came across too, rather than restarting at 1 and
	// handing out an id sched-7 already has.
	id, err := store.NewScheduleID(ctx)
	if err != nil {
		t.Fatalf("allocating an id after the rename: %v", err)
	}
	if id != "sched-8" {
		t.Errorf("NewScheduleID after the rename = %q, want sched-8", id)
	}

	// And the renamed tables are writable under their new names, which is
	// what every later PutSchedule needs.
	if err := store.PutSchedule(ctx, schedule(id, now)); err != nil {
		t.Fatalf("put after the rename: %v", err)
	}
}

func TestHasOpenTaskWithTag(t *testing.T) {
	store, _, ctx := openStore(t)
	tagged := task("t1", true)
	tagged.Tags = []string{"schedule:sched-1"}
	if err := store.PutSchedule(ctx, schedule("sched-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTask(ctx, tagged); err != nil {
		t.Fatal(err)
	}

	open, err := store.HasOpenTaskWithTag(ctx, "schedule:sched-1")
	if err != nil {
		t.Fatal(err)
	}
	if !open {
		t.Fatal("want an open task tagged schedule:sched-1, got none")
	}

	if err := store.Observe(ctx, model.Observation{TaskID: "t1", ClosedAt: &now}); err != nil {
		t.Fatal(err)
	}
	open, err = store.HasOpenTaskWithTag(ctx, "schedule:sched-1")
	if err != nil {
		t.Fatal(err)
	}
	if open {
		t.Fatal("want no open task tagged schedule:sched-1 once it is closed")
	}

	open, err = store.HasOpenTaskWithTag(ctx, "schedule:unknown")
	if err != nil {
		t.Fatal(err)
	}
	if open {
		t.Fatal("want false for a tag nothing carries")
	}
}
