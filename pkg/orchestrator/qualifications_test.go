package orchestrator_test

// SyncQualifications' own tests (bwsalmon/agents#518). Unlike releases_
// test.go, nothing here talks to (simulated) GitHub: qualification
// scheduling and auto-promotion are both pure store operations, the same
// "declare, then let a later reconciler carry out the GitHub-side effect"
// split SyncReleases itself already draws, just one step further removed
// from GitHub than a promotion request is.

import (
	"context"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// cutFirstCandidate creates a fresh release named "myfeat" for repo and
// returns its own first candidate -- CreateRelease's own "also cuts its
// first candidate" (bwsalmon/agents#571), which is all every test below
// needs a candidate for.
func cutFirstCandidate(t *testing.T, ctx context.Context, store *model.Store, repo model.RepoRef) model.Candidate {
	t.Helper()
	release, err := store.CreateRelease(ctx, repo, "myfeat", baseTime)
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	// Store-level tests exercise SyncQualifications alone, never
	// SyncReleases (this package's own doc comment on why: qualification
	// scheduling and auto-promotion are pure store operations) -- so
	// nothing else here ever advances the release out of provisioning the
	// way the real releases reconciler would.
	if err := store.MarkReleaseProvisioned(ctx, release.ID); err != nil {
		t.Fatalf("mark provisioned: %v", err)
	}
	c, err := store.CurrentCandidateForRelease(ctx, release.ID)
	if err != nil || c == nil {
		t.Fatalf("current candidate: (%+v, %v)", c, err)
	}
	return *c
}

// currentCandidate returns repo's release named "myfeat"'s own current
// candidate -- store.CurrentCandidate's own repo-scoped convenience,
// applied here since every test in this file only ever has the one
// release in play.
func currentCandidate(t *testing.T, ctx context.Context, store *model.Store, repo model.RepoRef) model.Candidate {
	t.Helper()
	release, err := store.GetRelease(ctx, repo, "myfeat")
	if err != nil || release == nil {
		t.Fatalf("release: (%+v, %v)", release, err)
	}
	c, err := store.CurrentCandidateForRelease(ctx, release.ID)
	if err != nil || c == nil {
		t.Fatalf("current candidate: (%+v, %v)", c, err)
	}
	return *c
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
	candidate := cutFirstCandidate(t, ctx, store, repo)
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
	candidate := cutFirstCandidate(t, ctx, store, repo)
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
	current := currentCandidate(t, ctx, store, repo)
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
	current = currentCandidate(t, ctx, store, repo)
	if current.Status != model.CandidatePromoting {
		t.Fatalf("got status %q after every task completed, want promoting", current.Status)
	}
}

// A run's own creation can fail -- here, because the plan's template does
// not exist yet -- and CreateQualificationRun's own doc comment promises
// that failure is "retried next cycle exactly like any other store error."
// Nothing should be left half-created in the meantime: no run row at all
// until a cycle that resolves every item's template cleanly.
func TestSyncQualificationsRetriesRunCreationAfterAMissingTemplateIsAdded(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	candidate := cutFirstCandidate(t, ctx, store, repo)
	if err := store.MarkCandidateCut(ctx, candidate.ID); err != nil {
		t.Fatalf("mark cut: %v", err)
	}
	plan := onePassingItem()
	plan.Repo = repo
	if err := store.PutQualificationPlan(ctx, plan); err != nil {
		t.Fatalf("put plan: %v", err)
	}

	// The plan's own template ("template-smoke") was never created --
	// this cycle must fail, and must not leave a run behind.
	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err == nil {
		t.Fatal("want an error when the plan's template does not exist yet")
	}
	if run, err := store.CandidateQualificationRun(ctx, candidate.ID); err != nil || run != nil {
		t.Fatalf("got (%+v, %v) after a failed attempt, want (nil, nil)", run, err)
	}

	if err := store.PutTaskTemplate(ctx, smokeTestTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncQualifications once the template exists: %v", err)
	}
	run, err := store.CandidateQualificationRun(ctx, candidate.ID)
	if err != nil || run == nil {
		t.Fatalf("candidate qualification run: (%+v, %v)", run, err)
	}
	if len(run.Tasks) != 1 {
		t.Fatalf("got %d task instances, want 1", len(run.Tasks))
	}
}

// A task closed without ever completing is a failure
// (model.QualificationStatus's own reasoning), and AutoPromote must never
// act on a run in that state -- unlike a straggler still in flight, this
// is permanent: nothing about the candidate ever becomes eligible again.
func TestSyncQualificationsNeverAutoPromotesARunWithAClosedTask(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if err := store.PutTaskTemplate(ctx, smokeTestTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	candidate := cutFirstCandidate(t, ctx, store, repo)
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

	closedAt := baseTime
	if err := store.Observe(ctx, model.Observation{TaskID: run.Tasks[0].TaskID, ClosedAt: &closedAt}); err != nil {
		t.Fatalf("observe closed: %v", err)
	}

	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncQualifications after the task closed: %v", err)
	}
	current := currentCandidate(t, ctx, store, repo)
	if current.Status != model.CandidateActive {
		t.Fatalf("got status %q for a run with a closed task, want still active (never auto-promoted)", current.Status)
	}

	updated, err := store.CandidateQualificationRun(ctx, candidate.ID)
	if err != nil || updated == nil {
		t.Fatalf("run: (%+v, %v)", updated, err)
	}
	if got := model.QualificationStatus(updated.Tasks); got != model.QualificationFailed {
		t.Fatalf("got status %v, want failed", got)
	}
}

// The full lifecycle, run twice: a candidate cut, qualified, and
// auto-promoted must not stop a fresh candidate cut afterwards from
// getting its own independent qualification run -- CandidateQualificationRun
// keys on candidate_id, so a second candidate's run must never be confused
// with, or blocked by, the first one's.
func TestSyncQualificationsHandlesASecondCandidateAfterTheFirstIsPromoted(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if err := store.PutTaskTemplate(ctx, smokeTestTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	plan := onePassingItem()
	plan.Repo = repo
	if err := store.PutQualificationPlan(ctx, plan); err != nil {
		t.Fatalf("put plan: %v", err)
	}

	first := cutFirstCandidate(t, ctx, store, repo)
	if err := store.MarkCandidateCut(ctx, first.ID); err != nil {
		t.Fatalf("mark first cut: %v", err)
	}
	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("create first run: %v", err)
	}
	firstRun, err := store.CandidateQualificationRun(ctx, first.ID)
	if err != nil || firstRun == nil {
		t.Fatalf("first run: (%+v, %v)", firstRun, err)
	}
	completedAt := baseTime
	if err := store.Observe(ctx, model.Observation{TaskID: firstRun.Tasks[0].TaskID, CompletedAt: &completedAt}); err != nil {
		t.Fatalf("observe first completion: %v", err)
	}
	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("auto-promote first: %v", err)
	}
	if err := store.MarkCandidatePromoted(ctx, first.ID, baseTime); err != nil {
		t.Fatalf("mark first promoted: %v", err)
	}

	second, err := store.CutCandidate(ctx, repo, "myfeat", baseTime)
	if err != nil {
		t.Fatalf("cut second candidate: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("second candidate got the same id as the first")
	}
	if err := store.MarkCandidateCut(ctx, second.ID); err != nil {
		t.Fatalf("mark second cut: %v", err)
	}
	if err := orchestrator.SyncQualifications(ctx, store, baseTime); err != nil {
		t.Fatalf("create second run: %v", err)
	}

	secondRun, err := store.CandidateQualificationRun(ctx, second.ID)
	if err != nil || secondRun == nil {
		t.Fatalf("second run: (%+v, %v)", secondRun, err)
	}
	if secondRun.ID == firstRun.ID {
		t.Fatal("second candidate's run must not be the first candidate's run")
	}
	if len(secondRun.Tasks) != 1 || secondRun.Tasks[0].TaskID == firstRun.Tasks[0].TaskID {
		t.Fatalf("second run's task must be its own instance, not reused from the first: %+v", secondRun.Tasks)
	}
}

func TestSyncQualificationsDoesNotAutoPromoteWhenThePlanDoesNotAskForIt(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if err := store.PutTaskTemplate(ctx, smokeTestTemplate(repo)); err != nil {
		t.Fatalf("put template: %v", err)
	}
	candidate := cutFirstCandidate(t, ctx, store, repo)
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
	current := currentCandidate(t, ctx, store, repo)
	if current.Status != model.CandidateActive {
		t.Fatalf("got status %q with AutoPromote disabled, want still active", current.Status)
	}
}
