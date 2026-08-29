package ui_test

// client_test.go's own doc comment gives the reasoning for testing
// against a real embedded store rather than a fake.

import (
	"errors"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func TestCreateScheduleFilesAnEnabledScheduleDueImmediately(t *testing.T) {
	c, _, ctx := testClient(t)
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "Nightly dependency bump", Repo: "acme/widgets", Interval: "24h",
	})
	if err != nil {
		t.Fatalf("creating a schedule: %v", err)
	}
	if sched.ID == "" {
		t.Fatal("want a non-empty id")
	}
	if !sched.Enabled {
		t.Error("want a freshly created schedule enabled by default")
	}
	if sched.Repo != "acme/widgets" {
		t.Errorf("repo = %q, want acme/widgets", sched.Repo)
	}
	if sched.Interval != "24h0m0s" {
		t.Errorf("interval = %q, want 24h0m0s", sched.Interval)
	}
	if !sched.NextRunAt.Equal(baseTime) {
		t.Errorf("nextRunAt = %v, want %v (fires the moment it is created)", sched.NextRunAt, baseTime)
	}
	if sched.LastRunAt != nil {
		t.Errorf("lastRunAt = %v, want nil: nothing has fired yet", sched.LastRunAt)
	}
}

func TestCreateScheduleRejectsAMissingTitle(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Repo: "acme/widgets", Interval: "24h"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleRejectsAMissingRepo(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "x", Interval: "24h"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleRejectsAnUnparseableInterval(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "x", Repo: "acme/widgets", Interval: "nope"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleRejectsANonPositiveInterval(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "x", Repo: "acme/widgets", Interval: "0s"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleCanBeFiledDisabled(t *testing.T) {
	c, _, ctx := testClient(t)
	disabled := false
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "x", Repo: "acme/widgets", Interval: "24h", Enabled: &disabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sched.Enabled {
		t.Error("want the schedule filed paused")
	}
}

func TestListSchedulesReturnsNewestFirst(t *testing.T) {
	c, _, ctx := testClient(t)
	first, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "first", Repo: "acme/widgets", Interval: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "second", Repo: "acme/widgets", Interval: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := c.ListSchedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list = %+v, want [%s, %s]", list, second.ID, first.ID)
	}
}

func TestUpdateScheduleAppliesOnlyGivenFields(t *testing.T) {
	c, _, ctx := testClient(t)
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "old title", Repo: "acme/widgets", Interval: "24h",
	})
	if err != nil {
		t.Fatal(err)
	}
	newTitle := "new title"
	updated, err := c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{Title: &newTitle})
	if err != nil {
		t.Fatalf("updating: %v", err)
	}
	if updated.Title != "new title" {
		t.Errorf("title = %q, want %q", updated.Title, "new title")
	}
	if updated.Repo != "acme/widgets" {
		t.Errorf("repo = %q, want it left alone", updated.Repo)
	}
}

func TestUpdateScheduleCanPauseAndResume(t *testing.T) {
	c, _, ctx := testClient(t)
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "x", Repo: "acme/widgets", Interval: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	paused, err := c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if paused.Enabled {
		t.Fatal("want it paused")
	}
	enabled := true
	resumed, err := c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Enabled {
		t.Fatal("want it resumed")
	}
}

func TestUpdateScheduleOnAnUnknownIDIsNotFound(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.UpdateSchedule(ctx, "nope", ui.UpdateScheduleRequest{})
	if err == nil {
		t.Fatal("want an error")
	}
}

func TestDeleteScheduleRemovesIt(t *testing.T) {
	c, _, ctx := testClient(t)
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "x", Repo: "acme/widgets", Interval: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSchedule(ctx, sched.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, err := c.ListSchedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("list = %+v, want empty after delete", list)
	}
}

func TestDeleteScheduleOnAnUnknownIDIsNotFound(t *testing.T) {
	c, _, ctx := testClient(t)
	err := c.DeleteSchedule(ctx, "nope")
	if err == nil {
		t.Fatal("want an error")
	}
}
