package sqlite_test

// Qualification's own store tests (bwsalmon/agents#518), against a real
// embedded SQLite database -- release_store_test.go's own reasoning for
// why this matters applies again here.

import (
	"reflect"
	"sort"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

func buildTemplate(id string) model.TaskTemplate {
	return model.TaskTemplate{
		ID: id, Name: "Build", Title: "Build", Body: "go build ./...",
		Target: widgets, Reads: []model.RepoRef{gadgets},
		Grants: []model.Grant{{Capability: "run-tests", Via: model.GrantByLabel}}, CreatedAt: now,
	}
}

func unitTestTemplate(id string) model.TaskTemplate {
	return model.TaskTemplate{
		ID: id, Name: "Unit tests", Title: "Unit tests", Body: "go test ./...",
		Target: widgets, CreatedAt: now,
	}
}

func testQualificationPlan() model.QualificationPlan {
	return model.QualificationPlan{
		Repo: widgets, Configured: true, RequireApproval: false, AutoPromote: true,
		Items: []model.QualificationItem{
			{TemplateID: "template-build", Repeat: 1},
			{TemplateID: "template-unit", Repeat: 2, DependsOn: []string{"template-build"}},
		},
	}
}

func TestGetQualificationPlanReturnsNilOnAFreshDatabase(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetQualificationPlan(ctx, widgets)
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) before anything has configured %s, got (%+v, %v)", widgets, got, err)
	}
}

func TestQualificationPlanRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	want := testQualificationPlan()
	if err := store.PutQualificationPlan(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetQualificationPlan(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
}

// PutQualificationPlan replaces a repo's items wholesale rather than
// accumulating them, the same discipline PutReleaseConfig already holds
// its own single row to.
func TestPutQualificationPlanReplacesRatherThanAccumulating(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutQualificationPlan(ctx, testQualificationPlan()); err != nil {
		t.Fatalf("first put: %v", err)
	}
	updated := model.QualificationPlan{
		Repo: widgets, Configured: true, RequireApproval: true, AutoPromote: false,
		Items: []model.QualificationItem{{TemplateID: "template-smoke", Repeat: 1}},
	}
	if err := store.PutQualificationPlan(ctx, updated); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err := store.GetQualificationPlan(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, updated) {
		t.Fatalf("got %+v, want %+v", *got, updated)
	}
}

func TestCreateQualificationRunInstantiatesEveryItemWithDependencyLinks(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTaskTemplate(ctx, buildTemplate("template-build")); err != nil {
		t.Fatalf("put build template: %v", err)
	}
	if err := store.PutTaskTemplate(ctx, unitTestTemplate("template-unit")); err != nil {
		t.Fatalf("put unit template: %v", err)
	}
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	candidate, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if err := store.MarkCandidateCut(ctx, candidate.ID); err != nil {
		t.Fatalf("mark cut: %v", err)
	}
	candidate.Status = model.CandidateActive

	plan := testQualificationPlan()
	run, err := store.CreateQualificationRun(ctx, candidate, plan, now)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(run.Tasks) != 3 {
		t.Fatalf("got %d task instances, want 3 (1 build + 2 unit)", len(run.Tasks))
	}

	var buildID string
	unitCount := 0
	for _, ts := range run.Tasks {
		if !ts.Approved {
			t.Errorf("task %s: want approved (RequireApproval is false), got unapproved", ts.TaskID)
		}
		if ts.State != model.StateQueued {
			t.Errorf("task %s: got state %s, want queued", ts.TaskID, ts.State)
		}
		switch ts.TemplateID {
		case "template-build":
			buildID = ts.TaskID
			if ts.TemplateName != "Build" {
				t.Errorf("got template name %q, want Build", ts.TemplateName)
			}
		case "template-unit":
			unitCount++
		}
	}
	if buildID == "" {
		t.Fatal("no build instance found")
	}
	if unitCount != 2 {
		t.Fatalf("got %d unit instances, want 2", unitCount)
	}

	// Every unit instance should depend on the build instance, and
	// nothing should block build itself.
	for _, ts := range run.Tasks {
		task, err := store.GetTask(ctx, ts.TaskID)
		if err != nil || task == nil {
			t.Fatalf("get task %s: (%+v, %v)", ts.TaskID, task, err)
		}
		if task.Base != candidate.Branch {
			t.Errorf("task %s: got base %q, want candidate branch %q", ts.TaskID, task.Base, candidate.Branch)
		}
		if task.Target == nil || *task.Target != widgets {
			t.Errorf("task %s: got target %v, want %s", ts.TaskID, task.Target, widgets)
		}
		if task.Origin.Reason != model.ReasonQualification {
			t.Errorf("task %s: got origin reason %q, want qualification", ts.TaskID, task.Origin.Reason)
		}
		var deps []string
		for _, l := range task.Links {
			if l.Kind == model.LinkDependsOn {
				deps = append(deps, l.Target)
			}
		}
		sort.Strings(deps)
		switch ts.TemplateID {
		case "template-build":
			if len(deps) != 0 {
				t.Errorf("build task %s: got deps %v, want none", ts.TaskID, deps)
			}
			if len(task.Reads) != 1 || task.Reads[0] != gadgets {
				t.Errorf("build task %s: got reads %v, want [%s]", ts.TaskID, task.Reads, gadgets)
			}
		case "template-unit":
			if len(deps) != 1 || deps[0] != buildID {
				t.Errorf("unit task %s: got deps %v, want [%s]", ts.TaskID, deps, buildID)
			}
		}
	}

	// A second run for the same candidate is refused outright.
	if _, err := store.CreateQualificationRun(ctx, candidate, plan, now); err == nil {
		t.Fatal("want an error creating a second run for the same candidate")
	}
}

func TestCreateQualificationRunFailsWhenATemplateIsMissing(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	candidate, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	plan := model.QualificationPlan{
		Repo: widgets, Items: []model.QualificationItem{{TemplateID: "does-not-exist", Repeat: 1}},
	}
	if _, err := store.CreateQualificationRun(ctx, candidate, plan, now); err == nil {
		t.Fatal("want an error when a plan's template no longer exists")
	}
}

func TestCreateQualificationRunFailsWhenATemplateTargetsADifferentRepo(t *testing.T) {
	store, _, ctx := openStore(t)
	mismatched := buildTemplate("template-build")
	mismatched.Target = gadgets
	if err := store.PutTaskTemplate(ctx, mismatched); err != nil {
		t.Fatalf("put template: %v", err)
	}
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	candidate, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	plan := model.QualificationPlan{
		Repo: widgets, Items: []model.QualificationItem{{TemplateID: "template-build", Repeat: 1}},
	}
	if _, err := store.CreateQualificationRun(ctx, candidate, plan, now); err == nil {
		t.Fatal("want an error when a plan's template targets a different repo")
	}
}

func TestCreateQualificationRunLeavesTasksUnapprovedWhenPlanRequiresApproval(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutTaskTemplate(ctx, buildTemplate("template-build")); err != nil {
		t.Fatalf("put build template: %v", err)
	}
	if err := store.PutTaskTemplate(ctx, unitTestTemplate("template-unit")); err != nil {
		t.Fatalf("put unit template: %v", err)
	}
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	candidate, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}

	plan := testQualificationPlan()
	plan.RequireApproval = true
	run, err := store.CreateQualificationRun(ctx, candidate, plan, now)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, ts := range run.Tasks {
		if ts.Approved {
			t.Errorf("task %s: want unapproved, got approved", ts.TaskID)
		}
		if ts.State != model.StateProposed {
			t.Errorf("task %s: got state %s, want proposed", ts.TaskID, ts.State)
		}
	}
	if got := model.QualificationStatus(run.Tasks); got != model.QualificationPendingApproval {
		t.Fatalf("got status %v, want pending_approval", got)
	}

	if err := store.ApproveQualificationRun(ctx, run.ID, model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "alice"}}, now); err != nil {
		t.Fatalf("approve: %v", err)
	}
	approved, err := store.CandidateQualificationRun(ctx, candidate.ID)
	if err != nil || approved == nil {
		t.Fatalf("reading run after approval: (%+v, %v)", approved, err)
	}
	for _, ts := range approved.Tasks {
		if !ts.Approved {
			t.Errorf("task %s: want approved after ApproveQualificationRun, got unapproved", ts.TaskID)
		}
	}
}

func TestQualifiableActiveCandidatesFindsAnActiveCandidateWithAnItemButNoRunYet(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	candidate, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if err := store.MarkCandidateCut(ctx, candidate.ID); err != nil {
		t.Fatalf("mark cut: %v", err)
	}

	got, err := store.QualifiableActiveCandidates(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d candidates before any plan is configured, want 0", len(got))
	}

	if err := store.PutQualificationPlan(ctx, testQualificationPlan()); err != nil {
		t.Fatalf("put plan: %v", err)
	}
	got, err = store.QualifiableActiveCandidates(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != candidate.ID {
		t.Fatalf("got %+v, want exactly candidate %d", got, candidate.ID)
	}
}

func TestQualificationPlansUsingTemplateFindsAPlanThatReferencesIt(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.QualificationPlansUsingTemplate(ctx, "template-build")
	if err != nil {
		t.Fatalf("list before any plan exists: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}

	if err := store.PutQualificationPlan(ctx, testQualificationPlan()); err != nil {
		t.Fatalf("put plan: %v", err)
	}
	got, err = store.QualificationPlansUsingTemplate(ctx, "template-build")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0] != widgets {
		t.Fatalf("got %+v, want [%s]", got, widgets)
	}
}
