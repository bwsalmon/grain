package ui_test

// schedules_test.go's own doc comment gives the reasoning for testing
// against a real embedded store rather than a fake.

import (
	"errors"
	"testing"

	"github.com/bwsalmon/grain/pkg/ui"
)

func TestCreateTemplateFilesATemplate(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{
		Name: "Dependency bump", Title: "Bump dependencies", Repo: "acme/widgets",
	})
	if err != nil {
		t.Fatalf("creating a template: %v", err)
	}
	if tmpl.ID == "" {
		t.Fatal("want a non-empty id")
	}
	if tmpl.Name != "Dependency bump" || tmpl.Title != "Bump dependencies" || tmpl.Repo != "acme/widgets" {
		t.Errorf("template = %+v, want the fields just given", tmpl)
	}
}

func TestCreateTemplateRejectsAMissingName(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Title: "x", Repo: "acme/widgets"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateTemplateRejectsAMissingTitle(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Repo: "acme/widgets"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateTemplateRejectsAMissingRepo(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateTemplateAcceptsCapabilitiesAndReads(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{
		Name: "x", Title: "x", Repo: "acme/widgets",
		Capabilities: []string{"gemini-key"},
		Reads:        []string{"acme/shared-lib"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpl.Capabilities) != 1 || tmpl.Capabilities[0] != "gemini-key" {
		t.Errorf("capabilities = %v, want [gemini-key]", tmpl.Capabilities)
	}
	if len(tmpl.Reads) != 1 || tmpl.Reads[0] != "acme/shared-lib" {
		t.Errorf("reads = %v, want [acme/shared-lib]", tmpl.Reads)
	}
}

func TestCreateTemplateRejectsAnUnknownCapability(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{
		Name: "x", Title: "x", Repo: "acme/widgets", Capabilities: []string{"not-a-real-capability"},
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestListTemplatesReturnsNewestFirst(t *testing.T) {
	c, _, ctx := testClient(t)
	first, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "first", Title: "x", Repo: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "second", Title: "x", Repo: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := c.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list = %+v, want [%s, %s]", list, second.ID, first.ID)
	}
}

func TestUpdateTemplateAppliesOnlyGivenFields(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "old title", Repo: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	newTitle := "new title"
	updated, err := c.UpdateTemplate(ctx, tmpl.ID, ui.UpdateTemplateRequest{Title: &newTitle})
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

func TestUpdateTemplateOnAnUnknownIDIsNotFound(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.UpdateTemplate(ctx, "nope", ui.UpdateTemplateRequest{})
	if err == nil {
		t.Fatal("want an error")
	}
}

func TestDeleteTemplateRemovesIt(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x", Repo: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteTemplate(ctx, tmpl.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, err := c.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("list = %+v, want empty after delete", list)
	}
}

func TestDeleteTemplateOnAnUnknownIDIsNotFound(t *testing.T) {
	c, _, ctx := testClient(t)
	err := c.DeleteTemplate(ctx, "nope")
	if err == nil {
		t.Fatal("want an error")
	}
}

// TestDeleteTemplateRefusesWhileASchedulePointsAtIt is
// ui.Client.DeleteTemplate's own safety net (its doc comment): deleting a
// template a schedule still fires from would silently strand that
// schedule's next firing, so this is rejected instead of allowed to
// succeed and orphan it.
func TestDeleteTemplateRefusesWhileASchedulePointsAtIt(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x", Repo: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		TemplateID: tmpl.ID, Recurrence: everyDay,
	}); err != nil {
		t.Fatalf("creating a template-backed schedule: %v", err)
	}
	err = c.DeleteTemplate(ctx, tmpl.ID)
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}
