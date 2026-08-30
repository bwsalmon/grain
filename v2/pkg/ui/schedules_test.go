package ui_test

// client_test.go's own doc comment gives the reasoning for testing
// against a real embedded store rather than a fake.

import (
	"errors"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/ui"
)

var everyDay = ui.Recurrence{Kind: "daily", TimeOfDay: "09:00"}

func TestCreateScheduleFilesAnEnabledScheduleDueImmediately(t *testing.T) {
	c, _, ctx := testClient(t)
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "Nightly dependency bump", Repo: "acme/widgets",
		Recurrence: ui.Recurrence{Kind: "everyNHours", EveryNHours: 24},
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
	if sched.Recurrence.Kind != "everyNHours" || sched.Recurrence.EveryNHours != 24 {
		t.Errorf("recurrence = %+v, want every 24 hours", sched.Recurrence)
	}
	if !sched.NextRunAt.Equal(baseTime) {
		t.Errorf("nextRunAt = %v, want %v (fires the moment it is created)", sched.NextRunAt, baseTime)
	}
	if sched.LastRunAt != nil {
		t.Errorf("lastRunAt = %v, want nil: nothing has fired yet", sched.LastRunAt)
	}
}

func TestCreateScheduleAcceptsDailyWeeklyAndMonthlyRecurrences(t *testing.T) {
	c, _, ctx := testClient(t)
	cases := []ui.Recurrence{
		{Kind: "daily", TimeOfDay: "09:00"},
		{Kind: "weekly", TimeOfDay: "14:30", Weekday: "monday"},
		{Kind: "monthly", TimeOfDay: "00:00", DayOfMonth: 31},
	}
	for _, r := range cases {
		sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
			Title: "x", Repo: "acme/widgets", Recurrence: r,
		})
		if err != nil {
			t.Fatalf("creating a %s schedule: %v", r.Kind, err)
		}
		if sched.Recurrence != r {
			t.Errorf("recurrence = %+v, want %+v", sched.Recurrence, r)
		}
	}
}

func TestCreateScheduleRejectsAMissingTitle(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Repo: "acme/widgets", Recurrence: everyDay})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleRejectsAMissingRepo(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "x", Recurrence: everyDay})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleRejectsAnUnrecognizedRecurrenceKind(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "x", Repo: "acme/widgets", Recurrence: ui.Recurrence{Kind: "nope"},
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleRejectsANonPositiveEveryNHours(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "x", Repo: "acme/widgets", Recurrence: ui.Recurrence{Kind: "everyNHours", EveryNHours: 0},
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleRejectsAnUnparseableTimeOfDay(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "x", Repo: "acme/widgets", Recurrence: ui.Recurrence{Kind: "daily", TimeOfDay: "nope"},
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleRejectsAnUnknownWeekday(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "x", Repo: "acme/widgets",
		Recurrence: ui.Recurrence{Kind: "weekly", TimeOfDay: "09:00", Weekday: "someday"},
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleRejectsADayOfMonthOutOfRange(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "x", Repo: "acme/widgets",
		Recurrence: ui.Recurrence{Kind: "monthly", TimeOfDay: "09:00", DayOfMonth: 32},
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleAcceptsCapabilitiesAndReads(t *testing.T) {
	c, _, ctx := testClient(t)
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "x", Repo: "acme/widgets", Recurrence: everyDay,
		Capabilities: []string{"gemini-key"},
		Reads:        []string{"acme/shared-lib"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sched.Capabilities) != 1 || sched.Capabilities[0] != "gemini-key" {
		t.Errorf("capabilities = %v, want [gemini-key]", sched.Capabilities)
	}
	if len(sched.Reads) != 1 || sched.Reads[0] != "acme/shared-lib" {
		t.Errorf("reads = %v, want [acme/shared-lib]", sched.Reads)
	}
}

func TestCreateScheduleRejectsAnUnknownCapability(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "x", Repo: "acme/widgets", Recurrence: everyDay,
		Capabilities: []string{"not-a-real-capability"},
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleCanBeFiledDisabled(t *testing.T) {
	c, _, ctx := testClient(t)
	disabled := false
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "x", Repo: "acme/widgets", Recurrence: everyDay, Enabled: &disabled,
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
	first, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "first", Repo: "acme/widgets", Recurrence: everyDay})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "second", Repo: "acme/widgets",
		Recurrence: ui.Recurrence{Kind: "everyNHours", EveryNHours: 1},
	})
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
		Title: "old title", Repo: "acme/widgets", Recurrence: everyDay,
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
	if updated.Recurrence != everyDay {
		t.Errorf("recurrence = %+v, want it left alone at %+v", updated.Recurrence, everyDay)
	}
}

func TestUpdateScheduleReplacesRecurrenceCapabilitiesAndReads(t *testing.T) {
	c, _, ctx := testClient(t)
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "x", Repo: "acme/widgets", Recurrence: everyDay})
	if err != nil {
		t.Fatal(err)
	}
	weekly := ui.Recurrence{Kind: "weekly", TimeOfDay: "08:00", Weekday: "friday"}
	capabilities := []string{"gemini-key"}
	reads := []string{"acme/shared-lib"}
	updated, err := c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{
		Recurrence:   &weekly,
		Capabilities: &capabilities,
		Reads:        &reads,
	})
	if err != nil {
		t.Fatalf("updating: %v", err)
	}
	if updated.Recurrence != weekly {
		t.Errorf("recurrence = %+v, want %+v", updated.Recurrence, weekly)
	}
	if len(updated.Capabilities) != 1 || updated.Capabilities[0] != "gemini-key" {
		t.Errorf("capabilities = %v, want [gemini-key]", updated.Capabilities)
	}
	if len(updated.Reads) != 1 || updated.Reads[0] != "acme/shared-lib" {
		t.Errorf("reads = %v, want [acme/shared-lib]", updated.Reads)
	}
}

func TestUpdateScheduleCanPauseAndResume(t *testing.T) {
	c, _, ctx := testClient(t)
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "x", Repo: "acme/widgets", Recurrence: everyDay})
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
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "x", Repo: "acme/widgets", Recurrence: everyDay})
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
