package sqlite_test

// The scheduled-task store, against a real embedded SQLite database --
// store_test.go's own doc comment on why gives the reasoning again here.

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
)

func schedule(id string, nextRunAt time.Time) model.ScheduledTask {
	return model.ScheduledTask{
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

func TestScheduledTaskRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	want := schedule("sched-1", now)
	want.Base = "main"
	want.AutoMerge = true
	want.Reads = []model.RepoRef{{Owner: "owner", Name: "shared-lib"}}
	want.Grants = []model.Grant{{Capability: "web-search", Via: model.GrantByLabel}}
	last := now.Add(-24 * time.Hour)
	want.LastRunAt = &last

	if err := store.PutScheduledTask(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetScheduledTask(ctx, "sched-1")
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

func TestScheduledTaskRoundTripsEveryRecurrenceKind(t *testing.T) {
	store, _, ctx := openStore(t)
	cases := []model.Recurrence{
		{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60},
		{Kind: model.RecurrenceWeekly, TimeOfDay: 14*60 + 30, Weekday: time.Monday},
		{Kind: model.RecurrenceMonthly, TimeOfDay: 0, DayOfMonth: 31},
	}
	for i, want := range cases {
		sched := schedule("sched-"+string(rune('a'+i)), now)
		sched.Recurrence = want
		if err := store.PutScheduledTask(ctx, sched); err != nil {
			t.Fatalf("put %+v: %v", want, err)
		}
		got, err := store.GetScheduledTask(ctx, sched.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Recurrence != want {
			t.Errorf("recurrence = %+v, want %+v", got.Recurrence, want)
		}
	}
}

func TestGetScheduledTaskReturnsNilWhenAbsent(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetScheduledTask(ctx, "nope")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
}

func TestNewScheduledTaskIDsAreDistinctFromTaskIDs(t *testing.T) {
	store, _, ctx := openStore(t)
	taskID, err := store.NewTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schedID, err := store.NewScheduledTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if taskID == schedID {
		t.Fatalf("task id %q and scheduled task id %q collided", taskID, schedID)
	}
	if schedID[:6] != "sched-" {
		t.Errorf("scheduled task id = %q, want a sched- prefix", schedID)
	}
}

func TestListScheduledTasksReturnsNewestFirst(t *testing.T) {
	store, _, ctx := openStore(t)
	earlier := schedule("sched-1", now)
	earlier.CreatedAt = now.Add(-time.Hour)
	later := schedule("sched-2", now)
	later.CreatedAt = now
	for _, s := range []model.ScheduledTask{earlier, later} {
		if err := store.PutScheduledTask(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.ListScheduledTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "sched-2" || got[1].ID != "sched-1" {
		t.Fatalf("list = %+v, want [sched-2, sched-1]", got)
	}
}

func TestDueScheduledTasksFiltersOnEnabledAndNextRunAt(t *testing.T) {
	store, _, ctx := openStore(t)
	due := schedule("sched-due", now.Add(-time.Minute))
	notYet := schedule("sched-not-yet", now.Add(time.Hour))
	paused := schedule("sched-paused", now.Add(-time.Minute))
	paused.Enabled = false
	for _, s := range []model.ScheduledTask{due, notYet, paused} {
		if err := store.PutScheduledTask(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.DueScheduledTasks(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "sched-due" {
		t.Fatalf("due = %+v, want only sched-due", got)
	}
}

func TestUpdateScheduledTaskAppliesAndPersists(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutScheduledTask(ctx, schedule("sched-1", now)); err != nil {
		t.Fatal(err)
	}
	next := now.Add(24 * time.Hour)
	err := store.UpdateScheduledTask(ctx, "sched-1", func(s *model.ScheduledTask) error {
		s.Enabled = false
		s.NextRunAt = next
		s.LastRunAt = &now
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.GetScheduledTask(ctx, "sched-1")
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

func TestUpdateScheduledTaskOnAnUnknownIDErrors(t *testing.T) {
	store, _, ctx := openStore(t)
	err := store.UpdateScheduledTask(ctx, "nope", func(s *model.ScheduledTask) error { return nil })
	if err == nil {
		t.Fatal("want an error updating an unknown schedule, got nil")
	}
}

func TestDeleteScheduledTaskRemovesIt(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutScheduledTask(ctx, schedule("sched-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteScheduledTask(ctx, "sched-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := store.GetScheduledTask(ctx, "sched-1")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) after delete, got (%v, %v)", got, err)
	}
}

// bwsalmon/agents#464: scheduled_task.interval_ms (every N hours since a
// schedule last fired, no wall-clock alignment) became five columns
// behind model.Recurrence, so a schedule could also fire daily, weekly or
// monthly at a fixed time of day. This simulates a database built with
// the pre-#464 column set -- interval_ms present, the new recurrence
// columns absent -- directly rather than through Store, and checks
// Store.Init's own migration step (ensureScheduledTaskRecurrenceColumns)
// backfills every_n_hours from interval_ms (rounded down to whole hours)
// and drops interval_ms, without disturbing the rest of the row.
func TestInitMigratesAnExistingDatabaseWithBareIntervalMS(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+"`scheduled_task`"+` (
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
		t.Fatalf("creating the pre-#464 scheduled_task table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO `scheduled_task` (`id`,`title`,`body`,`target_owner`,`target_name`,`base`,"+
			"`auto_merge`,`interval_ms`,`enabled`,`next_run_at`,`last_run_at`,`created_at`) "+
			"VALUES ('sched-1','Nightly dependency bump','Bump every dependency.','owner','payments-api',NULL,"+
			"0,90000000,1,?,NULL,?)", now, now); err != nil {
		t.Fatalf("seeding a pre-#464 schedule row: %v", err)
	}

	store := model.New(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init against an existing database with a bare interval_ms: %v", err)
	}

	got, err := store.GetScheduledTask(ctx, "sched-1")
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	// 90000000ms = 25h, rounded down to 25 whole hours.
	if got.Title != "Nightly dependency bump" ||
		got.Recurrence.Kind != model.RecurrenceEveryNHours || got.Recurrence.EveryNHours != 25 {
		t.Fatalf("got %+v, want the pre-existing row intact and recurrence migrated to every 25 hours", got)
	}

	// The old column is gone, not merely ignored -- PutScheduledTask stops
	// supplying it, so it would otherwise fail every write with a NOT NULL
	// constraint violation.
	want := schedule("sched-2", now)
	if err := store.PutScheduledTask(ctx, want); err != nil {
		t.Fatalf("put after migrating: %v", err)
	}
}

func TestHasOpenTaskWithTag(t *testing.T) {
	store, _, ctx := openStore(t)
	tagged := task("t1", true)
	tagged.Tags = []string{"schedule:sched-1"}
	if err := store.PutScheduledTask(ctx, schedule("sched-1", now)); err != nil {
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
