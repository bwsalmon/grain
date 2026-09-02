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
	got := r.Next(after)
	want := utc(2026, time.March, 1, 6, 0)
	if !got.Equal(want) {
		t.Errorf("Next(%v) = %v, want %v", after, got, want)
	}
}

func TestRecurrenceDailyFiresAtTheSameTimeTheNextDayOnceTodaysHasPassed(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60}
	got := r.Next(utc(2026, time.March, 1, 10, 0))
	want := utc(2026, time.March, 2, 9, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestRecurrenceDailyFiresLaterTodayWhenItsTimeHasNotPassedYet(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceDaily, TimeOfDay: 9 * 60}
	got := r.Next(utc(2026, time.March, 1, 8, 0))
	want := utc(2026, time.March, 1, 9, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestRecurrenceWeeklyFiresOnTheNextMatchingWeekday(t *testing.T) {
	// 2026-03-02 is a Monday.
	r := model.Recurrence{Kind: model.RecurrenceWeekly, Weekday: time.Friday, TimeOfDay: 14 * 60}
	got := r.Next(utc(2026, time.March, 2, 0, 0))
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
	got := r.Next(utc(2026, time.March, 6, 10, 0))
	want := utc(2026, time.March, 13, 9, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestRecurrenceMonthlyFiresOnTheGivenDayOfMonth(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceMonthly, DayOfMonth: 15, TimeOfDay: 12 * 60}
	got := r.Next(utc(2026, time.March, 1, 0, 0))
	want := utc(2026, time.March, 15, 12, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestRecurrenceMonthlyRollsToNextMonthOnceThisMonthsHasPassed(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceMonthly, DayOfMonth: 15, TimeOfDay: 12 * 60}
	got := r.Next(utc(2026, time.March, 16, 0, 0))
	want := utc(2026, time.April, 15, 12, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

// A day-of-month past a shorter month's own last day fires on that last
// day instead -- 31 in April, which only has 30.
func TestRecurrenceMonthlyClampsToTheMonthsLastDay(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceMonthly, DayOfMonth: 31, TimeOfDay: 0}
	got := r.Next(utc(2026, time.April, 1, 0, 0))
	want := utc(2026, time.April, 30, 0, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

// February, the shortest month, is where clamping matters most: a
// schedule asking for the 31st fires on the 28th (2026 is not a leap
// year) and then the loop fireScheduledTask runs (advancing from the
// previous NextRunAt, not from "now") must not get stuck re-clamping to
// February forever.
func TestRecurrenceMonthlyAdvancesPastFebruaryOnRepeatedCalls(t *testing.T) {
	r := model.Recurrence{Kind: model.RecurrenceMonthly, DayOfMonth: 31, TimeOfDay: 0}
	first := r.Next(utc(2026, time.January, 31, 1, 0))
	if want := utc(2026, time.February, 28, 0, 0); !first.Equal(want) {
		t.Fatalf("first = %v, want %v", first, want)
	}
	second := r.Next(first)
	if want := utc(2026, time.March, 31, 0, 0); !second.Equal(want) {
		t.Fatalf("second = %v, want %v", second, want)
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
