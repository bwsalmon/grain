package ui_test

// Qualification's own ui.Client tests (bwsalmon/agents#518) --
// releases_test.go's own discipline: a real embedded SQLite store, no
// GitHub or orchestrator anywhere in sight.

import (
	"context"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func putTestTemplate(t *testing.T, store *model.Store, ctx context.Context, id string, repo model.RepoRef) {
	t.Helper()
	if err := store.PutTaskTemplate(ctx, model.TaskTemplate{
		ID: id, Name: "Smoke test", Title: "Smoke test", Body: "run it", Target: repo,
	}); err != nil {
		t.Fatalf("put template %s: %v", id, err)
	}
}

func TestGetQualificationPlanReportsUnconfigured(t *testing.T) {
	client, _, ctx := testClient(t)
	plan, err := client.GetQualificationPlan(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Configured {
		t.Fatalf("got %+v, want Configured false on a fresh store", plan)
	}
}

func TestPutQualificationPlanRejectsAnUnknownTemplate(t *testing.T) {
	client, _, ctx := testClient(t)
	_, err := client.PutQualificationPlan(ctx, widgets, ui.PutQualificationPlanRequest{
		Items: []ui.QualificationItem{{TemplateID: "does-not-exist", Repeat: 1}},
	})
	if _, ok := err.(*ui.ValidationError); !ok {
		t.Fatalf("got %v (%T), want a ValidationError", err, err)
	}
}

func TestPutQualificationPlanRejectsATemplateTargetingAnotherRepo(t *testing.T) {
	client, store, ctx := testClient(t)
	putTestTemplate(t, store, ctx, "template-1", model.RepoRef{Owner: "acme", Name: "other"})
	_, err := client.PutQualificationPlan(ctx, widgets, ui.PutQualificationPlanRequest{
		Items: []ui.QualificationItem{{TemplateID: "template-1", Repeat: 1}},
	})
	if _, ok := err.(*ui.ValidationError); !ok {
		t.Fatalf("got %v (%T), want a ValidationError", err, err)
	}
}

func TestPutQualificationPlanRejectsADependencyCycle(t *testing.T) {
	client, store, ctx := testClient(t)
	putTestTemplate(t, store, ctx, "template-a", widgets)
	putTestTemplate(t, store, ctx, "template-b", widgets)
	_, err := client.PutQualificationPlan(ctx, widgets, ui.PutQualificationPlanRequest{
		Items: []ui.QualificationItem{
			{TemplateID: "template-a", Repeat: 1, DependsOn: []string{"template-b"}},
			{TemplateID: "template-b", Repeat: 1, DependsOn: []string{"template-a"}},
		},
	})
	if _, ok := err.(*ui.ValidationError); !ok {
		t.Fatalf("got %v (%T), want a ValidationError", err, err)
	}
}

func TestPutThenGetQualificationPlanRoundTripsWithTemplateNameFilledIn(t *testing.T) {
	client, store, ctx := testClient(t)
	putTestTemplate(t, store, ctx, "template-1", widgets)
	saved, err := client.PutQualificationPlan(ctx, widgets, ui.PutQualificationPlanRequest{
		RequireApproval: true, AutoPromote: true,
		Items: []ui.QualificationItem{{TemplateID: "template-1", Repeat: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Configured || !saved.RequireApproval || !saved.AutoPromote {
		t.Fatalf("got %+v", saved)
	}
	if len(saved.Items) != 1 || saved.Items[0].TemplateName != "Smoke test" || saved.Items[0].Repeat != 2 {
		t.Fatalf("got items %+v", saved.Items)
	}

	got, err := client.GetQualificationPlan(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items[0].TemplateName != "Smoke test" {
		t.Fatalf("got %+v", got)
	}
}

func TestGetCandidateQualificationReturnsNilBeforeARunExists(t *testing.T) {
	client, store, ctx := testClient(t)
	candidate := cutTestCandidate(t, ctx, store, widgets)
	run, err := client.GetCandidateQualification(ctx, widgets, candidate.ID)
	if err != nil || run != nil {
		t.Fatalf("got (%+v, %v), want (nil, nil)", run, err)
	}
}

func TestApproveQualificationRunApprovesEveryTaskAndOrdersFailuresFirst(t *testing.T) {
	client, store, ctx := testClient(t)
	putTestTemplate(t, store, ctx, "template-1", widgets)
	if err := store.PutQualificationPlan(ctx, model.QualificationPlan{
		Repo: widgets, RequireApproval: true,
		Items: []model.QualificationItem{{TemplateID: "template-1", Repeat: 2}},
	}); err != nil {
		t.Fatalf("put plan: %v", err)
	}
	candidate := cutTestCandidate(t, ctx, store, widgets)
	plan, err := store.GetQualificationPlan(ctx, widgets)
	if err != nil || plan == nil {
		t.Fatalf("get plan: (%+v, %v)", plan, err)
	}
	run, err := store.CreateQualificationRun(ctx, candidate, *plan, baseTime)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Fail one instance before approving, so the summary's own
	// failures-first ordering has something to sort.
	for i := 0; i < model.MaxConsecutiveFailures; i++ {
		runID := "run-" + run.Tasks[0].TaskID + "-" + string(rune('a'+i))
		if err := store.StartRun(ctx, model.Run{ID: runID, TaskID: run.Tasks[0].TaskID, Sandbox: "sandbox-1", Attempt: i + 1, StartedAt: baseTime}, 0); err != nil {
			t.Fatalf("start run: %v", err)
		}
		if err := store.FinishRun(ctx, runID, baseTime, "failed", "boom"); err != nil {
			t.Fatalf("finish run: %v", err)
		}
	}

	updated, err := client.ApproveQualificationRun(ctx, widgets, candidate.ID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	for _, ts := range updated.Tasks {
		if !ts.Approved {
			t.Errorf("task %s: want approved, got unapproved", ts.TaskID)
		}
	}
	if updated.Status != string(model.QualificationFailed) {
		t.Fatalf("got status %q, want failed", updated.Status)
	}
	if updated.Tasks[0].State != model.StateFailed {
		t.Fatalf("got first task state %q, want failed to sort first", updated.Tasks[0].State)
	}
}

func TestApproveQualificationRunOnAnUnknownCandidateIs404(t *testing.T) {
	client, _, ctx := testClient(t)
	_, err := client.ApproveQualificationRun(ctx, widgets, 999)
	if err == nil {
		t.Fatal("want an error approving a candidate with no qualification run")
	}
}

// A candidate id is only meaningful within its own repo: asking for one
// repo's candidate through a different repo's path must behave exactly
// like the candidate does not exist, never leak the other repo's run.
func TestGetCandidateQualificationIsScopedToItsOwnRepo(t *testing.T) {
	client, store, ctx := testClient(t)
	putTestTemplate(t, store, ctx, "template-1", widgets)
	if err := store.PutQualificationPlan(ctx, model.QualificationPlan{
		Repo: widgets, Items: []model.QualificationItem{{TemplateID: "template-1", Repeat: 1}},
	}); err != nil {
		t.Fatalf("put plan: %v", err)
	}
	candidate := cutTestCandidate(t, ctx, store, widgets)
	plan, err := store.GetQualificationPlan(ctx, widgets)
	if err != nil || plan == nil {
		t.Fatalf("get plan: (%+v, %v)", plan, err)
	}
	if _, err := store.CreateQualificationRun(ctx, candidate, *plan, baseTime); err != nil {
		t.Fatalf("create run: %v", err)
	}

	otherRepo := model.RepoRef{Owner: "acme", Name: "other"}
	run, err := client.GetCandidateQualification(ctx, otherRepo, candidate.ID)
	if err != nil || run != nil {
		t.Fatalf("got (%+v, %v) asking through a different repo, want (nil, nil)", run, err)
	}
	if _, err := client.ApproveQualificationRun(ctx, otherRepo, candidate.ID); err == nil {
		t.Fatal("want an error approving a candidate's run through a different repo")
	}

	// The same candidate, asked through its own repo, works fine.
	run, err = client.GetCandidateQualification(ctx, widgets, candidate.ID)
	if err != nil || run == nil {
		t.Fatalf("got (%+v, %v) through its own repo, want the run", run, err)
	}
}

// PutQualificationPlan defaults a non-positive repeat count to 1 rather
// than rejecting it -- documenting that behavior here, since it means a
// request with Repeat 0 (the zero value an easy client mistake to send
// for "unset") never actually reaches model.QualificationPlan.Validate's
// own "repeat must be at least 1" check.
func TestPutQualificationPlanDefaultsANonPositiveRepeatToOne(t *testing.T) {
	client, store, ctx := testClient(t)
	putTestTemplate(t, store, ctx, "template-1", widgets)
	saved, err := client.PutQualificationPlan(ctx, widgets, ui.PutQualificationPlanRequest{
		Items: []ui.QualificationItem{{TemplateID: "template-1", Repeat: 0}},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if len(saved.Items) != 1 || saved.Items[0].Repeat != 1 {
		t.Fatalf("got items %+v, want repeat defaulted to 1", saved.Items)
	}
}
