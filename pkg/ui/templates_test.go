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
		Name: "Dependency bump", Title: "Bump dependencies",
	})
	if err != nil {
		t.Fatalf("creating a template: %v", err)
	}
	if tmpl.ID == "" {
		t.Fatal("want a non-empty id")
	}
	if tmpl.Name != "Dependency bump" || tmpl.Title != "Bump dependencies" {
		t.Errorf("template = %+v, want the fields just given", tmpl)
	}
}

func TestCreateTemplateRejectsAMissingName(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Title: "x"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateTemplateRejectsAMissingTitle(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestCreateTemplateAcceptsCapabilitiesAndReads(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{
		Name: "x", Title: "x",
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
		Name: "x", Title: "x", Capabilities: []string{"not-a-real-capability"},
	})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestListTemplatesReturnsNewestFirst(t *testing.T) {
	c, _, ctx := testClient(t)
	first, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "first", Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "second", Title: "x"})
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
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "old title"})
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
	if updated.Name != "x" {
		t.Errorf("name = %q, want it left alone", updated.Name)
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
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x"})
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
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSchedule(ctx, ui.CreateScheduleRequest{
		Repo: "acme/widgets", TemplateID: tmpl.ID, Recurrence: everyDay,
	}); err != nil {
		t.Fatalf("creating a template-backed schedule: %v", err)
	}
	err = c.DeleteTemplate(ctx, tmpl.ID)
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

// --- binding (grain/task-285) --------------------------------------------

func TestCreateTemplateBindsARepoAndBranch(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{
		Name: "x", Title: "x", Repo: "acme/widgets", Base: "release",
	})
	if err != nil {
		t.Fatalf("creating a bound template: %v", err)
	}
	if tmpl.Repo != "acme/widgets" || tmpl.Base != "release" {
		t.Errorf("binding = (%q, %q), want acme/widgets on release", tmpl.Repo, tmpl.Base)
	}
}

// Binding is optional, and a template created without one says so:
// blank repo and base, not a repo nobody named.
func TestCreateTemplateLeavesTheBindingEmptyByDefault(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.Repo != "" || tmpl.Base != "" {
		t.Errorf("binding = (%q, %q), want an unbound template", tmpl.Repo, tmpl.Base)
	}
}

func TestCreateTemplateRejectsAMalformedRepo(t *testing.T) {
	c, _, ctx := testClient(t)
	_, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x", Repo: "widgets"})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}

func TestUpdateTemplateBindsAndUnbinds(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	repo, base := "acme/widgets", "release"
	bound, err := c.UpdateTemplate(ctx, tmpl.ID, ui.UpdateTemplateRequest{Repo: &repo, Base: &base})
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	if bound.Repo != "acme/widgets" || bound.Base != "release" {
		t.Fatalf("binding = (%q, %q), want acme/widgets on release", bound.Repo, bound.Base)
	}

	// A branch on its own moves within the same binding.
	main := "main"
	rebased, err := c.UpdateTemplate(ctx, tmpl.ID, ui.UpdateTemplateRequest{Base: &main})
	if err != nil {
		t.Fatalf("repinning the branch: %v", err)
	}
	if rebased.Repo != "acme/widgets" || rebased.Base != "main" {
		t.Fatalf("binding = (%q, %q), want the repo left alone on main", rebased.Repo, rebased.Base)
	}

	// An empty repo unbinds, and takes the pinned branch with it.
	none := ""
	unbound, err := c.UpdateTemplate(ctx, tmpl.ID, ui.UpdateTemplateRequest{Repo: &none})
	if err != nil {
		t.Fatalf("unbinding: %v", err)
	}
	if unbound.Repo != "" || unbound.Base != "" {
		t.Fatalf("binding = (%q, %q), want it cleared", unbound.Repo, unbound.Base)
	}
}

func TestUpdateTemplateRejectsABranchWithNoRepo(t *testing.T) {
	c, _, ctx := testClient(t)
	tmpl, err := c.CreateTemplate(ctx, ui.CreateTemplateRequest{Name: "x", Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	base := "release"
	_, err = c.UpdateTemplate(ctx, tmpl.ID, ui.UpdateTemplateRequest{Base: &base})
	var ve *ui.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
}
