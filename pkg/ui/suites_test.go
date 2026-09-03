package ui_test

// schedules_test.go's own doc comment gives the reasoning for testing
// against a real embedded store rather than a fake.

import (
	"errors"
	"testing"

	"github.com/bwsalmon/grain/pkg/ui"
)

func TestCreateSuiteFilesASuite(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{
		Name: "Find bugs", Title: "Find and fix a bug",
	})
	if err != nil {
		t.Fatalf("creating a template: %v", err)
	}

	suite, err := c.CreateSuite(ctx, ui.CreateSuiteRequest{
		Name: "Bug sweep", TemplateIDs: []string{tmpl.ID},
		Mode: "until_clean", MaxPasses: 5,
	})
	if err != nil {
		t.Fatalf("creating a suite: %v", err)
	}
	if suite.ID == "" {
		t.Fatal("want a non-empty id")
	}
	if suite.Name != "Bug sweep" || suite.Mode != "until_clean" || suite.MaxPasses != 5 {
		t.Errorf("suite = %+v, want the fields just given", suite)
	}
	// Auto queue and auto merge by default (bwsalmon/agents#642).
	if suite.RequireApproval {
		t.Error("want RequireApproval false by default")
	}
	if !suite.AutoMerge {
		t.Error("want AutoMerge true by default")
	}
	if len(suite.Items) != 1 || suite.Items[0].TemplateID != tmpl.ID || suite.Items[0].TemplateName != tmpl.Name {
		t.Errorf("items = %+v, want one item naming %s (%s)", suite.Items, tmpl.ID, tmpl.Name)
	}
}

func TestCreateSuiteRejectsAMissingName(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSuite(ctx, ui.CreateSuiteRequest{TemplateIDs: []string{"whatever"}, Mode: "count", Count: 1})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateSuiteRejectsNoTemplates(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSuite(ctx, ui.CreateSuiteRequest{Name: "x", Mode: "count", Count: 1})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateSuiteRejectsAnUnknownTemplate(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateSuite(ctx, ui.CreateSuiteRequest{
		Name: "x", TemplateIDs: []string{"does-not-exist"}, Mode: "count", Count: 1,
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateSuiteRejectsCountModeWithNoCount(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x"})
	if err != nil {
		t.Fatalf("creating a template: %v", err)
	}
	_, err = c.CreateSuite(ctx, ui.CreateSuiteRequest{Name: "x", TemplateIDs: []string{tmpl.ID}, Mode: "count"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestDeleteTemplateRefusesWhileASuiteStillUsesIt(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x"})
	if err != nil {
		t.Fatalf("creating a template: %v", err)
	}
	if _, err := c.CreateSuite(ctx, ui.CreateSuiteRequest{
		Name: "x", TemplateIDs: []string{tmpl.ID}, Mode: "count", Count: 1,
	}); err != nil {
		t.Fatalf("creating a suite: %v", err)
	}

	err = c.DeleteTemplate(ctx, tmpl.ID)
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError refusing the delete", err)
	}
}

// DeleteTemplate's own guard, one level out: a suite a schedule still
// runs on a cadence cannot be deleted out from under it, since that
// schedule would otherwise have nothing to fire.
func TestDeleteSuiteRefusesWhileAScheduleStillRunsIt(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x"})
	if err != nil {
		t.Fatalf("creating a template: %v", err)
	}
	suite, err := c.CreateSuite(ctx, ui.CreateSuiteRequest{
		Name: "x", TemplateIDs: []string{tmpl.ID}, Mode: "count", Count: 1,
	})
	if err != nil {
		t.Fatalf("creating a suite: %v", err)
	}
	sched, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		SuiteID: suite.ID, Repo: "acme/widgets", Base: "main",
		Recurrence: ui.Recurrence{Kind: "daily", TimeOfDay: "09:00"},
	})
	if err != nil {
		t.Fatalf("creating a schedule: %v", err)
	}

	err = c.DeleteSuite(ctx, suite.ID)
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError refusing the delete", err)
	}

	// Delete that schedule and the suite goes.
	if err := c.DeleteSchedule(ctx, sched.ID); err != nil {
		t.Fatalf("deleting the schedule: %v", err)
	}
	if err := c.DeleteSuite(ctx, suite.ID); err != nil {
		t.Fatalf("deleting the suite once nothing runs it: %v", err)
	}
}

func TestCreateSuiteRunStartsARunAgainstARepoAndBranch(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "Do the thing"})
	if err != nil {
		t.Fatalf("creating a template: %v", err)
	}
	suite, err := c.CreateSuite(ctx, ui.CreateSuiteRequest{
		Name: "x", TemplateIDs: []string{tmpl.ID}, Mode: "count", Count: 3,
	})
	if err != nil {
		t.Fatalf("creating a suite: %v", err)
	}

	run, err := c.CreateSuiteRun(ctx, ui.CreateSuiteRunRequest{
		SuiteID: suite.ID, Repo: "acme/widgets", Base: "feature/qualify-me",
	})
	if err != nil {
		t.Fatalf("creating a suite run: %v", err)
	}
	if run.ID == 0 {
		t.Fatal("want a non-zero run id")
	}
	if run.Repo != "acme/widgets" || run.Base != "feature/qualify-me" {
		t.Errorf("run = %+v, want repo/base as given", run)
	}
	if run.Status != "active" {
		t.Errorf("status = %q, want active", run.Status)
	}
	if run.Pass != 1 {
		t.Errorf("pass = %d, want 1 immediately after creation", run.Pass)
	}
	// Every task in the run stacks against the run's own base branch --
	// bwsalmon/agents#642's own "tasks created from the suite should
	// stack against the source branch."
	if len(run.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(run.Tasks))
	}
	task, err := c.Task(ctx, run.Tasks[0].TaskID)
	if err != nil {
		t.Fatalf("reading filed task: %v", err)
	}
	if task.Base != "feature/qualify-me" {
		t.Errorf("task base = %q, want the run's own base", task.Base)
	}
	if !task.AutoMerge {
		t.Error("want the filed task to inherit the suite's own AutoMerge")
	}
	if !task.SuiteRun {
		t.Error("want the filed task marked SuiteRun")
	}

	list, err := c.ListSuiteRuns(ctx)
	if err != nil {
		t.Fatalf("listing suite runs: %v", err)
	}
	if len(list) != 1 || list[0].ID != run.ID {
		t.Errorf("ListSuiteRuns = %+v, want the one run just created", list)
	}
}

func TestCreateSuiteRunRejectsAMissingBase(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x"})
	if err != nil {
		t.Fatalf("creating a template: %v", err)
	}
	suite, err := c.CreateSuite(ctx, ui.CreateSuiteRequest{Name: "x", TemplateIDs: []string{tmpl.ID}, Mode: "count", Count: 1})
	if err != nil {
		t.Fatalf("creating a suite: %v", err)
	}
	_, err = c.CreateSuiteRun(ctx, ui.CreateSuiteRunRequest{SuiteID: suite.ID, Repo: "acme/widgets"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}
