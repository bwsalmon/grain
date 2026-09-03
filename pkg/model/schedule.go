package model

import (
	"fmt"
	"time"
)

// RecurrenceKind is which of the four cadences a Recurrence expresses --
// bwsalmon/agents#464's widening of the schedule feature past "every N
// hours" into the options a human setting up a cron job already expects.
type RecurrenceKind string

const (
	// RecurrenceEveryNHours fires every EveryNHours hours, counted from
	// whenever it last fired (or was created) -- no wall-clock alignment,
	// the original (and still simplest) cadence.
	RecurrenceEveryNHours RecurrenceKind = "everyNHours"
	// RecurrenceDaily fires once a day, at TimeOfDay.
	RecurrenceDaily RecurrenceKind = "daily"
	// RecurrenceWeekly fires once a week, on Weekday at TimeOfDay.
	RecurrenceWeekly RecurrenceKind = "weekly"
	// RecurrenceMonthly fires once a month, on DayOfMonth at TimeOfDay --
	// clamped to a shorter month's last day, so DayOfMonth 31 reads as
	// much as "the last day of every month" as it does "the 31st".
	RecurrenceMonthly RecurrenceKind = "monthly"
)

// Recurrence is a schedule's cadence. TimeOfDay, Weekday and DayOfMonth
// are always read against UTC -- docs/schedules.md's "no per-schedule
// timezone" limit on the old plain-interval version applies again here,
// just against a wall clock instead of a duration.
type Recurrence struct {
	Kind RecurrenceKind

	// EveryNHours is how many hours between firings -- only set when Kind
	// is RecurrenceEveryNHours. Hours, not an arbitrary duration: v1's
	// own Interval-Hours header (docs/schedules.md) already put the
	// cadence a human actually asks for ("every so often") at this
	// granularity, and it is what bwsalmon/agents#464 itself asks for.
	EveryNHours int

	// TimeOfDay is minutes since UTC midnight (0-1439) -- only set when
	// Kind is RecurrenceDaily, RecurrenceWeekly or RecurrenceMonthly.
	TimeOfDay int

	// Weekday is only set when Kind is RecurrenceWeekly.
	Weekday time.Weekday

	// DayOfMonth is 1-31 -- only set when Kind is RecurrenceMonthly.
	DayOfMonth int
}

// Validate reports whether r is a recognized Kind with its fields in
// range for it -- checked once at creation/update (ui.parseRecurrence) so
// Next never has to handle a Kind it does not know or an out-of-range
// field.
func (r Recurrence) Validate() error {
	switch r.Kind {
	case RecurrenceEveryNHours:
		if r.EveryNHours <= 0 {
			return fmt.Errorf("everyNHours must be positive")
		}
	case RecurrenceDaily:
		return validTimeOfDay(r.TimeOfDay)
	case RecurrenceWeekly:
		if err := validTimeOfDay(r.TimeOfDay); err != nil {
			return err
		}
		if r.Weekday < time.Sunday || r.Weekday > time.Saturday {
			return fmt.Errorf("weekday out of range")
		}
	case RecurrenceMonthly:
		if err := validTimeOfDay(r.TimeOfDay); err != nil {
			return err
		}
		if r.DayOfMonth < 1 || r.DayOfMonth > 31 {
			return fmt.Errorf("dayOfMonth must be between 1 and 31")
		}
	default:
		return fmt.Errorf("unknown recurrence kind %q", r.Kind)
	}
	return nil
}

func validTimeOfDay(minutes int) error {
	if minutes < 0 || minutes >= 24*60 {
		return fmt.Errorf("timeOfDay must be between 0 and 1439 minutes")
	}
	return nil
}

// Next returns the first time this recurrence comes due strictly after
// after. fireTaskSchedule calls this in a loop from a schedule's own
// NextRunAt (never from "now" directly) so a schedule paused, or a daemon
// down, through several missed occurrences still fires exactly once on
// resume and resyncs to its normal cadence -- Next itself only has to
// answer "what's the one after this", the same contract the old
// "NextRunAt.Add(Interval)" loop already had, generalized to cadences a
// fixed duration cannot express (a calendar month is not a fixed number
// of hours).
func (r Recurrence) Next(after time.Time) time.Time {
	after = after.UTC()
	switch r.Kind {
	case RecurrenceDaily:
		return nextDaily(after, r.TimeOfDay)
	case RecurrenceWeekly:
		return nextWeekly(after, r.Weekday, r.TimeOfDay)
	case RecurrenceMonthly:
		return nextMonthly(after, r.DayOfMonth, r.TimeOfDay)
	case RecurrenceEveryNHours:
		every := time.Duration(r.EveryNHours) * time.Hour
		if every <= 0 {
			every = time.Hour
		}
		return after.Add(every)
	default:
		// Validate is expected to have already rejected this -- an hour
		// out is a safe, visible fallback rather than looping forever.
		return after.Add(time.Hour)
	}
}

func atTimeOfDay(day time.Time, timeOfDay int) time.Time {
	y, m, d := day.Date()
	return time.Date(y, m, d, timeOfDay/60, timeOfDay%60, 0, 0, time.UTC)
}

func nextDaily(after time.Time, timeOfDay int) time.Time {
	candidate := atTimeOfDay(after, timeOfDay)
	if !candidate.After(after) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

func nextWeekly(after time.Time, weekday time.Weekday, timeOfDay int) time.Time {
	candidate := atTimeOfDay(after, timeOfDay)
	for candidate.Weekday() != weekday || !candidate.After(after) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

func nextMonthly(after time.Time, dayOfMonth, timeOfDay int) time.Time {
	y, m, _ := after.Date()
	candidate := monthlyOccurrence(y, m, dayOfMonth, timeOfDay)
	if !candidate.After(after) {
		y, m = addMonth(y, m)
		candidate = monthlyOccurrence(y, m, dayOfMonth, timeOfDay)
	}
	return candidate
}

func addMonth(y int, m time.Month) (int, time.Month) {
	m++
	if m > time.December {
		m = time.January
		y++
	}
	return y, m
}

// monthlyOccurrence is dayOfMonth (clamped to year/month's own last day)
// at timeOfDay -- time.Date's own month-overflow semantics would instead
// roll a too-large day into the following month, which is exactly the
// drift clamping avoids.
func monthlyOccurrence(y int, m time.Month, dayOfMonth, timeOfDay int) time.Time {
	lastDay := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
	day := dayOfMonth
	if day > lastDay {
		day = lastDay
	}
	return time.Date(y, m, day, timeOfDay/60, timeOfDay%60, 0, 0, time.UTC)
}

// Schedule is what a human sets up once and grain refiles as a real Task
// on a fixed cadence -- bwsalmon/agents#376's v2 equivalent of v1's
// scheduled_jobs.py, with one deliberate difference: there is no template
// directory here, because the whole point of this package's own shape
// (README, "Input is a model update, not a GitHub issue") is that a
// schedule is a store row a UI can create, edit and delete, not a file an
// operator drops on a server's disk. ("Template" here used to describe
// this type itself, before TaskTemplate (bwsalmon/agents#516,
// template.go) existed to name the reusable half of a schedule's own
// content -- TemplateID below is what points at one.)
//
// docs/data-model.md's "the SCHEDULED special case dissolves" is why
// firing carries no approval flag of its own: creating a schedule is
// itself the standing human approval, so every task it later files lands
// already approved (orchestrator.fireTaskSchedule sets Approval
// unconditionally) -- the actor rule (LandsQueued) still holds
// unqualified, because the automation that actually creates the firing is
// never what decided to trust it.
//
// Reads and Grants mirror Task's own fields (bwsalmon/agents#464): a
// firing carries whatever read-only repos and capabilities the schedule
// itself was given, the same as a task filed by hand. DependsOn and
// Approved have no equivalent here, and deliberately so -- a one-shot
// dependency link makes no sense against a task this schedule refiles
// indefinitely, and CreateSchedule's own doc comment already explains why
// approval is not a per-firing choice.
//
// SuiteID is the one field that changes *what* a firing is: set, this
// schedule starts a TaskSuiteRun instead of filing a single Task. Every
// other field here means the same thing either way -- the cadence, the
// enabled switch, the target repo and base branch, and the "a previous
// firing that has not finished suppresses the next one" rule are the
// schedule's, not the task's.
type Schedule struct {
	ID    string
	Title string
	Body  string

	Target    RepoRef
	Base      string
	AutoMerge bool
	Reads     []RepoRef
	Grants    []Grant

	// TemplateID, if set, names the TaskTemplate (template.go) this
	// schedule fires from instead of its own
	// Title/Body/AutoMerge/Reads/Grants above (bwsalmon/agents#516) --
	// Target and Base are never among them: a template carries no target
	// of its own (TaskTemplate's own doc comment on why), so this
	// schedule's own Target and Base always decide what every firing
	// targets, whether or not TemplateID is set. TemplateID is resolved
	// fresh at firing time (orchestrator.fireTaskSchedule), not copied in
	// once, so editing the template changes what every schedule pointing
	// at it files next -- the same way editing this schedule's own fields
	// already changes what it itself files next. The fields above still
	// hold a value even when this is set: fireTaskSchedule keeps them in
	// sync as a display cache each time it fires, so ui.scheduleFrom can
	// render a schedule's effective title and the rest with no extra
	// lookup, but while TemplateID is set they are not what decides what
	// gets filed.
	TemplateID *string

	// SuiteID, if set, names the TaskSuite (suite.go) this schedule runs
	// on its cadence instead of filing a single task: every firing is one
	// CreateScheduledSuiteRun against this schedule's own Target and
	// Base, the same run a human starts by hand from the suites page,
	// started by the clock instead. Mutually exclusive with TemplateID --
	// a suite already resolves its own items' templates, so there is no
	// second template for a firing to also carry -- and, like it,
	// resolved fresh at firing time (orchestrator.fireSuiteSchedule),
	// never copied in once, so editing the suite changes what every
	// schedule pointing at it runs next. Title is kept in sync with the
	// suite's own Name as a display cache while this is set, exactly as
	// the content fields above are for TemplateID;
	// Body/AutoMerge/Reads/Grants are unused, since a suite run takes all
	// of those from the suite and its templates.
	//
	// What a schedule fires -- a task or a suite -- is fixed when it is
	// created: ui.UpdateSchedule will repoint a suite-backed schedule at
	// a different suite, but never turn one kind into the other, since
	// there is nothing sensible to carry across (a suite has no title or
	// body of its own to become a task's, and a task's content has
	// nowhere to go in a suite).
	SuiteID *string

	// Recurrence is how often this schedule fires.
	Recurrence Recurrence
	// Enabled is the pause/resume switch a UI toggles -- a disabled
	// schedule stays around (with its own history in NextRunAt/LastRunAt)
	// rather than being deleted, the same as closing a task keeps its row
	// rather than erasing it.
	Enabled bool
	// NextRunAt is when this schedule next comes due. Set to the
	// schedule's own creation time when it is first created, so a fresh
	// schedule fires once right away rather than waiting for its first
	// occurrence -- the same "queued the moment it is written" immediacy
	// CreateTask already gives a task filed by hand.
	NextRunAt time.Time
	// LastRunAt is when this schedule last actually filed a task, nil if
	// it never has.
	LastRunAt *time.Time

	CreatedAt time.Time
}
