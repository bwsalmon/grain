package orchestrator_test

// SyncQualifications' own tests (bwsalmon/agents#518). Unlike releases_
// test.go, nothing here talks to (simulated) GitHub: qualification
// scheduling and auto-promotion are both pure store operations, the same
// "declare, then let a later reconciler carry out the GitHub-side effect"
// split SyncReleases itself already draws, just one step further removed
// from GitHub than a promotion request is.

import (
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

func qualifyingReleaseConfig(repo model.RepoRef) model.ReleaseConfig {
	return model.ReleaseConfig{
		Repo: repo, ProdBranch: "main", RCBranch: "rc", ReleaseBranchPrefix: "release/", MajorVersion: 1,
	}
}

func smokeTestTemplate(repo model.RepoRef) model.TaskTemplate {
	return model.TaskTemplate{
		ID: "template-smoke", Name: "Smoke test", Title: "Smoke test", Body: "run the smoke suite",
		Target: repo, CreatedAt: baseTime,
	}
}

func onePassingItem() model.QualificationPlan {
	return model.QualificationPlan{
		Configured: true, AutoPromote: true,
		Items: []model.QualificationItem{{TemplateID: "template-smoke", Repeat: 1}},
	}
}

func TestSyncQualificationsCreatesARunForAFreshlyActiveCandidate(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if err := store.PutTaskTemplate(ctx, smokeTestTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	if err := store.PutReleaseConfig(ctx, qualifyingReleaseConfig(repo)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	candidate, err := store.CutCandidate(ctx, repo, baseTime)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if err := store.MarkCandidateCut(ctx, candidate.ID); err != nil {
		t.Fatalf("mark cut: %v", err)
	}

	plan := onePassingItem()
	plan.Repo = repo
	if err := store.PutQualificationPlan(ctx, plan); err != nil {
		t.Fatalf("put plan: %v", err)
	}

	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncQualifications: %v", err)
	}

	run, err := store.CandidateQualificationRun(ctx, candidate.ID)
	if err != nil || run == nil {
		t.Fatalf("candidate qualification run: (%+v, %v)", run, err)
	}
	if len(run.Tasks) != 1 {
		t.Fatalf("got %d task instances, want 1", len(run.Tasks))
	}

	// A second cycle must not create a second run.
	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("second SyncQualifications: %v", err)
	}
	again, err := store.CandidateQualificationRun(ctx, candidate.ID)
	if err != nil || again == nil || again.ID != run.ID {
		t.Fatalf("got a different run on the second cycle: (%+v, %v)", again, err)
	}
}

func TestSyncQualificationsAutoPromotesOnceEveryTaskCompletes(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if err := store.PutTaskTemplate(ctx, smokeTestTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	if err := store.PutReleaseConfig(ctx, qualifyingReleaseConfig(repo)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	candidate, err := store.CutCandidate(ctx, repo, baseTime)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if err := store.MarkCandidateCut(ctx, candidate.ID); err != nil {
		t.Fatalf("mark cut: %v", err)
	}
	plan := onePassingItem()
	plan.Repo = repo
	if err := store.PutQualificationPlan(ctx, plan); err != nil {
		t.Fatalf("put plan: %v", err)
	}
	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := store.CandidateQualificationRun(ctx, candidate.ID)
	if err != nil || run == nil {
		t.Fatalf("run: (%+v, %v)", run, err)
	}

	// Not yet promoted -- the task has not completed.
	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncQualifications before completion: %v", err)
	}
	current, err := store.CurrentCandidate(ctx, repo)
	if err != nil || current == nil {
		t.Fatalf("current: (%+v, %v)", current, err)
	}
	if current.Status != model.CandidateActive {
		t.Fatalf("got status %q before completion, want still active", current.Status)
	}

	completedAt := baseTime
	if err := store.Observe(ctx, model.Observation{TaskID: run.Tasks[0].TaskID, CompletedAt: &completedAt}); err != nil {
		t.Fatalf("observe completion: %v", err)
	}

	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncQualifications after completion: %v", err)
	}
	current, err = store.CurrentCandidate(ctx, repo)
	if err != nil || current == nil {
		t.Fatalf("current: (%+v, %v)", current, err)
	}
	if current.Status != model.CandidatePromoting {
		t.Fatalf("got status %q after every task completed, want promoting", current.Status)
	}
}

func TestSyncQualificationsDoesNotAutoPromoteWhenThePlanDoesNotAskForIt(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if err := store.PutTaskTemplate(ctx, smokeTestTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	if err := store.PutReleaseConfig(ctx, qualifyingReleaseConfig(repo)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	candidate, err := store.CutCandidate(ctx, repo, baseTime)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if err := store.MarkCandidateCut(ctx, candidate.ID); err != nil {
		t.Fatalf("mark cut: %v", err)
	}
	plan := onePassingItem()
	plan.Repo = repo
	plan.AutoPromote = false
	if err := store.PutQualificationPlan(ctx, plan); err != nil {
		t.Fatalf("put plan: %v", err)
	}
	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := store.CandidateQualificationRun(ctx, candidate.ID)
	if err != nil || run == nil {
		t.Fatalf("run: (%+v, %v)", run, err)
	}
	completedAt := baseTime
	if err := store.Observe(ctx, model.Observation{TaskID: run.Tasks[0].TaskID, CompletedAt: &completedAt}); err != nil {
		t.Fatalf("observe completion: %v", err)
	}

	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncQualifications: %v", err)
	}
	current, err := store.CurrentCandidate(ctx, repo)
	if err != nil || current == nil {
		t.Fatalf("current: (%+v, %v)", current, err)
	}
	if current.Status != model.CandidateActive {
		t.Fatalf("got status %q with AutoPromote disabled, want still active", current.Status)
	}
}
