package ui_test

// client_test.go's own doc comment gives the reasoning for testing
// against a real embedded store rather than a fake.

import (
	"context"
	"errors"
	"testing"

	"github.com/bwsalmon/grain/pkg/ui"
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

// TestCreateScheduleFromATemplateCopiesItsContent is bwsalmon/agents#516's
// whole point: a schedule created with TemplateID needs no Title of its
// own, and reads it off the template instead. Repo and Base are never
// among the fields a template supplies (model.TaskTemplate's own doc
// comment on why), so they always come from this same request.
func TestCreateScheduleFromATemplateCopiesItsContent(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{
		Name: "Dependency bump", Title: "Bump dependencies", Description: "Bump every dependency.",
		AutoMerge:    true,
		Capabilities: []string{"gemini-key"}, Reads: []string{"acme/shared-lib"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		TemplateID: tmpl.ID, Repo: "acme/widgets", Base: "main", Recurrence: everyDay,
	})
	if err != nil {
		t.Fatalf("creating a template-backed schedule: %v", err)
	}
	if sched.TemplateID != tmpl.ID {
		t.Errorf("templateId = %q, want %q", sched.TemplateID, tmpl.ID)
	}
	if sched.TemplateName != "Dependency bump" {
		t.Errorf("templateName = %q, want %q", sched.TemplateName, "Dependency bump")
	}
	if sched.Title != "Bump dependencies" || sched.Description != "Bump every dependency." {
		t.Errorf("title/description = %q/%q, want the template's own", sched.Title, sched.Description)
	}
	if !sched.AutoMerge {
		t.Errorf("autoMerge = %v, want the template's own", sched.AutoMerge)
	}
	if sched.Repo != "acme/widgets" || sched.Base != "main" {
		t.Errorf("repo/base = %q/%q, want this request's own", sched.Repo, sched.Base)
	}
	if len(sched.Capabilities) != 1 || sched.Capabilities[0] != "gemini-key" {
		t.Errorf("capabilities = %v, want [gemini-key]", sched.Capabilities)
	}
	if len(sched.Reads) != 1 || sched.Reads[0] != "acme/shared-lib" {
		t.Errorf("reads = %v, want [acme/shared-lib]", sched.Reads)
	}
}

func TestCreateScheduleRejectsAnUnknownTemplate(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{TemplateID: "nope", Repo: "acme/widgets", Recurrence: everyDay})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

// TestUpdateScheduleAttachToATemplateOverridesInlineContent checks
// UpdateScheduleRequest.TemplateID's non-empty case: attaching an
// ordinary, inline schedule to a template overwrites its content from
// that template outright, ignoring any other content field on the same
// request (CreateScheduleRequest's own doc comment explains why -- a
// template and per-field overrides is a combination this API does not
// support) -- except Repo and Base, which a template never carries and
// so are always left exactly as they already are unless this same
// request also gives a new one.
func TestUpdateScheduleAttachToATemplateOverridesInlineContent(t *testing.T) {
	c, _, ctx := testClient(t)
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "old title", Repo: "acme/widgets", Recurrence: everyDay,
	})
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{
		Name: "Dependency bump", Title: "Bump dependencies",
	})
	if err != nil {
		t.Fatal(err)
	}
	ignoredTitle := "this should be ignored"
	updated, err := c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{
		TemplateID: &tmpl.ID, Title: &ignoredTitle,
	})
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	if updated.TemplateID != tmpl.ID {
		t.Errorf("templateId = %q, want %q", updated.TemplateID, tmpl.ID)
	}
	if updated.Title != "Bump dependencies" {
		t.Errorf("title = %q, want the template's own, not %q", updated.Title, ignoredTitle)
	}
	if updated.Repo != "acme/widgets" {
		t.Errorf("repo = %q, want it left alone (a template carries no target)", updated.Repo)
	}
}

// TestUpdateScheduleDetachFromATemplateKeepsCurrentContent checks
// UpdateScheduleRequest.TemplateID's empty-string case: it clears
// TemplateID but leaves whatever content the schedule currently has
// (most recently synced from the template) in place as an independent
// copy, rather than blanking it out.
func TestUpdateScheduleDetachFromATemplateKeepsCurrentContent(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{
		Name: "Dependency bump", Title: "Bump dependencies",
	})
	if err != nil {
		t.Fatal(err)
	}
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		TemplateID: tmpl.ID, Repo: "acme/widgets", Recurrence: everyDay,
	})
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	updated, err := c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{TemplateID: &empty})
	if err != nil {
		t.Fatalf("detaching: %v", err)
	}
	if updated.TemplateID != "" {
		t.Errorf("templateId = %q, want empty after detaching", updated.TemplateID)
	}
	if updated.Title != "Bump dependencies" || updated.Repo != "acme/widgets" {
		t.Errorf("title/repo = %q/%q, want the content already on the row kept", updated.Title, updated.Repo)
	}

	// Now independent: editing it directly works the ordinary way.
	newTitle := "renamed"
	edited, err := c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{Title: &newTitle})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Title != "renamed" {
		t.Errorf("title = %q, want %q", edited.Title, "renamed")
	}
}

func TestUpdateScheduleRejectsAnUnknownTemplate(t *testing.T) {
	c, _, ctx := testClient(t)
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{Title: "x", Repo: "acme/widgets", Recurrence: everyDay})
	if err != nil {
		t.Fatal(err)
	}
	nope := "nope"
	_, err = c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{TemplateID: &nope})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

// --- schedules that run a task suite -------------------------------------

// suiteToSchedule is the setup every suite-backed schedule test below
// shares: a template, and a suite that runs it.
func suiteToSchedule(t *testing.T, c *ui.Client, ctx context.Context, name string) ui.Suite {
	t.Helper()
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: name + " template", Title: "Find and fix a bug"})
	if err != nil {
		t.Fatalf("creating a template: %v", err)
	}
	suite, err := c.CreateSuite(ctx, ui.CreateSuiteRequest{
		Name: name, TemplateIDs: []string{tmpl.ID}, Mode: "until_clean", MaxPasses: 5,
	})
	if err != nil {
		t.Fatalf("creating a suite: %v", err)
	}
	return suite
}

// A suite-backed schedule needs no title or content of its own: the
// suite decides what runs, and its name stands in as the schedule's own
// display title.
func TestCreateScheduleFromATaskSuite(t *testing.T) {
	c, _, ctx := testClient(t)
	suite := suiteToSchedule(t, c, ctx, "Bug sweep")

	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		SuiteID: suite.ID, Repo: "acme/widgets", Base: "main", Recurrence: everyDay,
	})
	if err != nil {
		t.Fatalf("creating a suite-backed schedule: %v", err)
	}
	if sched.SuiteID != suite.ID || sched.SuiteName != suite.Name {
		t.Errorf("suite = %q/%q, want %q/%q", sched.SuiteID, sched.SuiteName, suite.ID, suite.Name)
	}
	if sched.TemplateID != "" {
		t.Errorf("templateId = %q, want empty: a schedule fires a suite or a task, never both", sched.TemplateID)
	}
	if sched.Title != suite.Name {
		t.Errorf("title = %q, want the suite's own name %q", sched.Title, suite.Name)
	}
	if sched.Repo != "acme/widgets" || sched.Base != "main" {
		t.Errorf("target = %q@%q, want acme/widgets@main", sched.Repo, sched.Base)
	}

	// It comes back off the list the same way, suite name and all.
	list, err := c.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(list) != 1 || list[0].SuiteID != suite.ID || list[0].SuiteName != suite.Name {
		t.Fatalf("listed %+v, want one schedule naming %s (%s)", list, suite.ID, suite.Name)
	}
}

func TestCreateScheduleRejectsASuiteAndATemplateTogether(t *testing.T) {
	c, _, ctx := testClient(t)
	suite := suiteToSchedule(t, c, ctx, "Bug sweep")
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "Bump", Title: "Bump dependencies"})
	if err != nil {
		t.Fatalf("creating a template: %v", err)
	}

	_, err = c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		SuiteID: suite.ID, TemplateID: tmpl.ID, Repo: "acme/widgets", Base: "main", Recurrence: everyDay,
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

// CreateSuiteRunRequest's own rule, applied to the schedule that starts
// such a run on a cadence: a suite run stacks its tasks against one named
// branch, so there is no default to fall back to.
func TestCreateScheduleFromASuiteRejectsAMissingBase(t *testing.T) {
	c, _, ctx := testClient(t)
	suite := suiteToSchedule(t, c, ctx, "Bug sweep")

	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		SuiteID: suite.ID, Repo: "acme/widgets", Recurrence: everyDay,
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateScheduleRejectsAnUnknownSuite(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		SuiteID: "suite-nope", Repo: "acme/widgets", Base: "main", Recurrence: everyDay,
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

// Which suite a schedule runs is editable; everything else about a
// suite-backed schedule that is its own (repo, branch, cadence, paused)
// still edits exactly as any other schedule's does.
func TestUpdateScheduleRepointsASuiteBackedScheduleAtAnotherSuite(t *testing.T) {
	c, _, ctx := testClient(t)
	first := suiteToSchedule(t, c, ctx, "Bug sweep")
	second := suiteToSchedule(t, c, ctx, "Dependency sweep")
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		SuiteID: first.ID, Repo: "acme/widgets", Base: "main", Recurrence: everyDay,
	})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}

	paused := false
	updated, err := c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{
		SuiteID: &second.ID, Enabled: &paused,
	})
	if err != nil {
		t.Fatalf("repointing: %v", err)
	}
	if updated.SuiteID != second.ID || updated.SuiteName != second.Name {
		t.Errorf("suite = %q/%q, want %q/%q", updated.SuiteID, updated.SuiteName, second.ID, second.Name)
	}
	if updated.Title != second.Name {
		t.Errorf("title = %q, want the new suite's own name %q", updated.Title, second.Name)
	}
	if updated.Enabled {
		t.Error("want the schedule paused")
	}
}

// What a schedule fires is fixed when it is created -- neither direction
// of "convert it into the other kind" is supported, since there is
// nothing sensible to carry across (model.Schedule.SuiteID's own doc
// comment).
func TestUpdateScheduleRefusesToChangeWhatAScheduleFires(t *testing.T) {
	c, _, ctx := testClient(t)
	suite := suiteToSchedule(t, c, ctx, "Bug sweep")
	suiteBacked, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		SuiteID: suite.ID, Repo: "acme/widgets", Base: "main", Recurrence: everyDay,
	})
	if err != nil {
		t.Fatalf("creating a suite-backed schedule: %v", err)
	}
	taskBacked, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Title: "Nightly dependency bump", Repo: "acme/widgets", Recurrence: everyDay,
	})
	if err != nil {
		t.Fatalf("creating a task schedule: %v", err)
	}

	empty := ""
	var ve *ui.ValidationError
	if _, err := c.UpdateSchedule(ctx, suiteBacked.ID, ui.UpdateScheduleRequest{SuiteID: &empty}); !errors.As(err, &ve) {
		t.Errorf("detaching a suite: error = %v, want a ValidationError", err)
	}
	if _, err := c.UpdateSchedule(ctx, taskBacked.ID, ui.UpdateScheduleRequest{SuiteID: &suite.ID}); !errors.As(err, &ve) {
		t.Errorf("attaching a suite to a task schedule: error = %v, want a ValidationError", err)
	}

	// Still exactly as they were.
	list, err := c.ListSchedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == suiteBacked.ID && s.SuiteID != suite.ID {
			t.Errorf("suite-backed schedule lost its suite: %+v", s)
		}
		if s.ID == taskBacked.ID && s.SuiteID != "" {
			t.Errorf("task schedule gained a suite: %+v", s)
		}
	}
}

func TestUpdateScheduleRejectsAnUnknownSuite(t *testing.T) {
	c, _, ctx := testClient(t)
	suite := suiteToSchedule(t, c, ctx, "Bug sweep")
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		SuiteID: suite.ID, Repo: "acme/widgets", Base: "main", Recurrence: everyDay,
	})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}

	nope := "suite-nope"
	_, err = c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{SuiteID: &nope})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

// A suite-backed schedule's base branch cannot be emptied, for the same
// reason it was required at creation.
func TestUpdateScheduleRejectsEmptyingASuiteBackedScheduleBase(t *testing.T) {
	c, _, ctx := testClient(t)
	suite := suiteToSchedule(t, c, ctx, "Bug sweep")
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		SuiteID: suite.ID, Repo: "acme/widgets", Base: "main", Recurrence: everyDay,
	})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}

	empty := ""
	_, err = c.UpdateSchedule(ctx, sched.ID, ui.UpdateScheduleRequest{Base: &empty})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}
