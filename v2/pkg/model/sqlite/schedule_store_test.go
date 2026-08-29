package sqlite_test

// The scheduled-task store, against a real embedded SQLite database --
// store_test.go's own doc comment on why gives the reasoning again here.

import (
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

func schedule(id string, nextRunAt time.Time) model.ScheduledTask {
	return model.ScheduledTask{
		ID:        id,
		Title:     "Nightly dependency bump",
		Body:      "Bump every dependency to its latest patch release.",
		Target:    model.RepoRef{Owner: "owner", Name: "payments-api"},
		Interval:  24 * time.Hour,
		Enabled:   true,
		NextRunAt: nextRunAt,
		CreatedAt: now,
	}
}

func TestScheduledTaskRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	want := schedule("sched-1", now)
	want.Base = "main"
	want.AutoMerge = true
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
	if got.Interval != 24*time.Hour {
		t.Errorf("interval = %v, want 24h", got.Interval)
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
