package orchestrator_test

import (
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

func TestSyncPullRequestsClosesOutAMergedPullRequest(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	pr, err := orchestrator.EnsurePullRequest(ctx, store, client, task, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	task.Links = append(task.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: pr.Number}.String(),
	})
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}

	// GitHub merges the PR "out of band" -- SyncPullRequests must notice
	// on its own next read, not be told.
	if err := client.MergePullRequest("acme", "widgets", pr.Number, ""); err != nil {
		t.Fatal(err)
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed", st)
	}
	// Closing out is one store write. There is no issue to close alongside
	// it, so nothing here can end up with a closed issue and a store that
	// still believes the task is open.
	if len(sim.Issues) != 0 {
		t.Fatalf("expected no GitHub issues at all, got %+v", sim.Issues)
	}

	obs, err := store.GetObservation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.PrMergedAt == nil {
		t.Fatal("expected PrMergedAt to be set for a merged pull request")
	}
	if obs.PrClosedAt != nil {
		t.Fatal("expected PrClosedAt to stay nil for a merged pull request")
	}
	if obs.PrOpenedAt == nil {
		t.Fatal("expected PrOpenedAt to be set alongside PrMergedAt")
	}
}

func TestSyncPullRequestsRecordsAPullRequestClosedWithoutMerging(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	pr, err := orchestrator.EnsurePullRequest(ctx, store, client, task, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	task.Links = append(task.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: pr.Number}.String(),
	})
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}

	// A human closes the PR on GitHub without merging it -- Sim's own
	// doc comment on PullRequest.State says a test does this by setting
	// State directly, standing in for GitHub's own close-without-merge
	// button (there is no merge involved, so Merged stays false).
	for i := range sim.PullRequests {
		sim.PullRequests[i].State = "closed"
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed", st)
	}

	obs, err := store.GetObservation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.PrClosedAt == nil {
		t.Fatal("expected PrClosedAt to be set for a pull request closed without merging")
	}
	if obs.PrMergedAt != nil {
		t.Fatal("expected PrMergedAt to stay nil for a pull request closed without merging")
	}
}

func TestSyncPullRequestsLeavesAnOpenCleanPullRequestAlone(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	pr, err := orchestrator.EnsurePullRequest(ctx, store, client, task, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	task.Links = append(task.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: pr.Number}.String(),
	})
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state = %q, want still completed (not yet closed)", st)
	}
	obs, err := store.GetObservation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs != nil && obs.ClosedAt != nil {
		t.Fatal("expected the task not to have been closed out yet")
	}
	// PrOpenedAt is recorded the first cycle SyncPullRequests sees the
	// pull request at all, whether or not it is done -- an open PR left
	// alone still gets its own "PR opened" timeline moment recorded.
	if obs == nil || obs.PrOpenedAt == nil {
		t.Fatal("expected PrOpenedAt to be recorded even for a still-open pull request")
	}
}

func TestSyncPullRequestsAutoMergesACleanPullRequest(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.AutoMerge = true
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	pr, err := orchestrator.EnsurePullRequest(ctx, store, client, task, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	task.Links = append(task.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: pr.Number}.String(),
	})
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}

	clean := true
	for i := range sim.PullRequests {
		sim.PullRequests[i].Mergeable = &clean
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed once auto-merge landed", st)
	}
}
