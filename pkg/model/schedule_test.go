package model_test

import (
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

func utc(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

func TestRecurrenceEveryNHoursAdvancesByThatManyHours(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 6}
	after := utc(2026, time.March, 1, 0, 0)
	got := r.Next(after, time.UTC)
	want := utc(2026, time.March, 1, 6, 0)
	if !got.Equal(want) {
		t.Errorf("Next(%v) = %v, want %v", after, got, want)
	}
}

func TestRecurrenceDailyFiresAtTheSameTimeTheNextDayOnceTodaysHasPassed(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60}
	got := r.Next(utc(2026, time.March, 1, 10, 0), time.UTC)
	want := utc(2026, time.March, 2, 9, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestRecurrenceDailyFiresLaterTodayWhenItsTimeHasNotPassedYet(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60}
	got := r.Next(utc(2026, time.March, 1, 8, 0), time.UTC)
	want := utc(2026, time.March, 1, 9, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestRecurrenceWeeklyFiresOnTheNextMatchingWeekday(t *testing.T) {
	// 2026-03-02 is a Monday.
	r := model.Recurrence{Kind: model.RecurrenceWeekly, Weekday: time.Friday, TimeOfDay: 14 * 60}
	got := r.Next(utc(2026, time.March, 2, 0, 0), time.UTC)
	want := utc(2026, time.March, 6, 14, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
	if got.Weekday() != time.Friday {
		t.Errorf("Next landed on %v, want a Friday", got.Weekday())
	}
}

func TestRecurrenceWeeklySkipsToNextWeekOnceThisWeeksHasPassed(t *testing.T) {
	// 2026-03-06 is a Friday.
	r := model.Recurrence{Kind: model.RecurrenceWeekly, Weekday: time.Friday, TimeOfDay: 9 * 60}
	got := r.Next(utc(2026, time.March, 6, 10, 0), time.UTC)
	want := utc(2026, time.March, 13, 9, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestRecurrenceMonthlyFiresOnTheGivenDayOfMonth(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceMonthly, DayOfMonth: 15, TimeOfDay: 12 * 60}
	got := r.Next(utc(2026, time.March, 1, 0, 0), time.UTC)
	want := utc(2026, time.March, 15, 12, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestRecurrenceMonthlyRollsToNextMonthOnceThisMonthsHasPassed(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceMonthly, DayOfMonth: 15, TimeOfDay: 12 * 60}
	got := r.Next(utc(2026, time.March, 16, 0, 0), time.UTC)
	want := utc(2026, time.April, 15, 12, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

// A day-of-month past a shorter month's own last day fires on that last
// day instead -- 31 in April, which only has 30.
func TestRecurrenceMonthlyClampsToTheMonthsLastDay(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceMonthly, DayOfMonth: 31, TimeOfDay: 0}
	got := r.Next(utc(2026, time.April, 1, 0, 0), time.UTC)
	want := utc(2026, time.April, 30, 0, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

// February, the shortest month, is where clamping matters most: a
// schedule asking for the 31st fires on the 28th (2026 is not a leap
// year) and then the loop fireTaskSchedule runs (advancing from the
// previous NextRunAt, not from "now") must not get stuck re-clamping to
// February forever.
func TestRecurrenceMonthlyAdvancesPastFebruaryOnRepeatedCalls(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceMonthly, DayOfMonth: 31, TimeOfDay: 0}
	first := r.Next(utc(2026, time.January, 31, 1, 0), time.UTC)
	if want := utc(2026, time.February, 28, 0, 0); !first.Equal(want) {
		t.Fatalf("first = %v, want %v", first, want)
	}
	second := r.Next(first, time.UTC)
	if want := utc(2026, time.March, 31, 0, 0); !second.Equal(want) {
		t.Fatalf("second = %v, want %v", second, want)
	}
}

// The point of grain/task-368: a time of day is a time on the
// deployment's own clock, not on the container's accidental UTC one. A
// daily schedule set for 09:00 Pacific fires at 16:00 UTC in winter --
// the same instant, named the way the person who set it named it.
func TestRecurrenceDailyFiresAtTheGivenTimeInTheDeploymentsZone(t *testing.T) {
	pacific := model.LoadLocation(model.DefaultTimeZone)
	r := model.Recurrence{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60}
	got := r.Next(utc(2026, time.January, 15, 0, 0), pacific)
	if want := utc(2026, time.January, 15, 17, 0); !got.Equal(want) {
		t.Errorf("Next = %v, want %v (09:00 PST)", got, want)
	}
	if hh, mm, _ := got.In(pacific).Clock(); hh != 9 || mm != 0 {
		t.Errorf("Next reads as %02d:%02d locally, want 09:00", hh, mm)
	}
}

// Daylight saving moves the instant a daily schedule fires and leaves the
// wall-clock time alone, which is the whole reason a zone name is stored
// rather than a fixed offset. 2026-03-08 is when US Pacific springs
// forward: 09:00 is 17:00 UTC the day before and 16:00 UTC the day after.
func TestRecurrenceDailyKeepsItsWallClockTimeAcrossDaylightSaving(t *testing.T) {
	pacific := model.LoadLocation(model.DefaultTimeZone)
	r := model.Recurrence{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60}
	before := r.Next(utc(2026, time.March, 7, 0, 0), pacific)
	if want := utc(2026, time.March, 7, 17, 0); !before.Equal(want) {
		t.Fatalf("before the change = %v, want %v", before, want)
	}
	after := r.Next(before, pacific)
	if want := utc(2026, time.March, 8, 16, 0); !after.Equal(want) {
		t.Fatalf("after the change = %v, want %v", after, want)
	}
	if hh, _, _ := after.In(pacific).Clock(); hh != 9 {
		t.Errorf("the day after the change fires at %02d locally, want 09", hh)
	}
}

// "Every N hours" is a duration between instants, so it is the one
// cadence a zone cannot move: 24 hours after a firing is 24 hours later
// even on the 23-hour day, which is exactly how it differs from "daily".
func TestRecurrenceEveryNHoursIgnoresTheZone(t *testing.T) {
	pacific := model.LoadLocation(model.DefaultTimeZone)
	r := model.Recurrence{Kind: model.RecurrenceEveryNHours, EveryNHours: 24}
	after := utc(2026, time.March, 7, 12, 0)
	if got, want := r.Next(after, pacific), utc(2026, time.March, 8, 12, 0); !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

// A nil location is UTC -- what every cadence meant before a deployment
// had a zone at all, and what a caller with no configuration to hand
// gets rather than a panic.
func TestRecurrenceNextTreatsANilLocationAsUTC(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60}
	got := r.Next(utc(2026, time.March, 1, 10, 0), nil)
	if want := utc(2026, time.March, 2, 9, 0); !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

// A weekly schedule names a weekday on the deployment's calendar, which
// is not always the same day UTC is on: 23:00 Pacific on a Friday is
// already Saturday in UTC, and the schedule that says "Fridays" must
// still fire then.
func TestRecurrenceWeeklyReadsTheWeekdayOnTheDeploymentsCalendar(t *testing.T) {
	pacific := model.LoadLocation(model.DefaultTimeZone)
	r := model.Recurrence{Kind: model.RecurrenceWeekly, Weekday: time.Friday, TimeOfDay: 23 * 60}
	// 2026-03-02 is a Monday; the Friday after it is the 6th.
	got := r.Next(utc(2026, time.March, 2, 0, 0), pacific)
	if want := utc(2026, time.March, 7, 7, 0); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (Friday 23:00 PST)", got, want)
	}
	if day := got.In(pacific).Weekday(); day != time.Friday {
		t.Errorf("Next landed on %v locally, want a Friday", day)
	}
	if day := got.UTC().Weekday(); day != time.Saturday {
		t.Errorf("this test is checking nothing: %v is a %v in UTC too", got, day)
	}
}

func TestRecurrenceValidateRejectsOutOfRangeFields(t *testing.T) {
	cases := map[string]model.Recurrence{
		"non-positive everyNHours": {Kind: model.RecurrenceEveryNHours, EveryNHours: 0},
		"timeOfDay too large":      {Kind: model.RecurrenceDaily, TimeOfDay: 24 * 60},
		"timeOfDay negative":       {Kind: model.RecurrenceDaily, TimeOfDay: -1},
		"dayOfMonth zero":          {Kind: model.RecurrenceMonthly, TimeOfDay: 0, DayOfMonth: 0},
		"dayOfMonth too large":     {Kind: model.RecurrenceMonthly, TimeOfDay: 0, DayOfMonth: 32},
		"unknown kind":             {Kind: "nope"},
	}
	for name, r := range cases {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: want an error, got nil for %+v", name, r)
		}
	}
}

func TestRecurrenceValidateAcceptsOneOfEachKind(t *testing.T) {
	cases := []model.Recurrence{
		{Kind: model.RecurrenceEveryNHours, EveryNHours: 1},
		{Kind: model.RecurrenceDaily, TimeOfDay: 0},
		{Kind: model.RecurrenceWeekly, TimeOfDay: 0, Weekday: time.Sunday},
		{Kind: model.RecurrenceMonthly, TimeOfDay: 0, DayOfMonth: 1},
	}
	for _, r := range cases {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: want no error, got %v", r, err)
		}
	}
}
