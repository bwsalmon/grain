package orchestrator_test

// SyncReleases' own tests (bwsalmon/agents#398), against a real bare git
// repo behind githubsim.Sim the same way sync_test.go tests
// SyncPullRequests -- release management performs real git operations
// (creating and moving branches), so a FakeTransport standing in for
// GitHub would only prove this package's own request-building, not that
// the branch it asked GitHub to create actually exists afterwards.

import (
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

func releaseConfig(repo model.RepoRef) model.ReleaseConfig {
	return model.ReleaseConfig{
		Repo: repo, ProdBranch: "main", RCBranch: "rc", ReleaseBranchPrefix: "release/", MajorVersion: 3,
	}
}

func mustHead(t *testing.T, client *github.RESTClient, owner, repo, branch string) string {
	t.Helper()
	head, err := client.GetBranchHead(owner, repo, branch)
	if err != nil {
		t.Fatalf("reading %s: %v", branch, err)
	}
	if head == nil {
		t.Fatalf("branch %q does not exist", branch)
	}
	return head.SHA
}

func TestSyncReleasesCutsACandidateBranchAndAssignsTheRcBranch(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if err := store.PutReleaseConfig(ctx, releaseConfig(repo)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	candidate, err := store.CutCandidate(ctx, repo, baseTime)
	if err != nil {
		t.Fatalf("cut candidate: %v", err)
	}
	if candidate.Branch != "release/3.1-rc1" {
		t.Fatalf("got branch %q, want release/3.1-rc1", candidate.Branch)
	}

	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncReleases: %v", err)
	}

	mainHead := mustHead(t, client, "acme", "widgets", "main")
	if got := mustHead(t, client, "acme", "widgets", candidate.Branch); got != mainHead {
		t.Fatalf("got candidate branch tip %q, want it pinned at main's own tip %q", got, mainHead)
	}
	if got := mustHead(t, client, "acme", "widgets", "rc"); got != mainHead {
		t.Fatalf("got rc branch tip %q, want the repo's own rc branch assigned to %q", got, mainHead)
	}

	current, err := store.CurrentCandidate(ctx, repo)
	if err != nil || current == nil {
		t.Fatalf("current candidate: %+v %v", current, err)
	}
	if current.Status != model.CandidateActive {
		t.Fatalf("got status %q, want active", current.Status)
	}
	if current.LastError != "" {
		t.Fatalf("got error %q, want none", current.LastError)
	}
	if len(sim.Calls) == 0 {
		t.Fatal("expected SyncReleases to have called out to GitHub")
	}
}

func TestSyncReleasesIsIdempotentOnceACandidateIsAlreadyCut(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if err := store.PutReleaseConfig(ctx, releaseConfig(repo)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	if _, err := store.CutCandidate(ctx, repo, baseTime); err != nil {
		t.Fatalf("cut candidate: %v", err)
	}
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("first SyncReleases: %v", err)
	}
	// A second run finds nothing pending -- the candidate already
	// advanced to active -- and must not error re-creating a branch that
	// already exists.
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("second SyncReleases: %v", err)
	}
}

func TestSyncReleasesRecordsAnErrorWhenTheProdBranchIsMissing(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	cfg := releaseConfig(repo)
	cfg.ProdBranch = "does-not-exist"
	if err := store.PutReleaseConfig(ctx, cfg); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	if _, err := store.CutCandidate(ctx, repo, baseTime); err != nil {
		t.Fatalf("cut candidate: %v", err)
	}

	// A candidate-specific failure must not surface as a cycle error --
	// see syncCandidate's own doc comment on why -- only get recorded onto
	// the row for a UI to show, so the next cycle can simply retry it.
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncReleases: %v", err)
	}

	current, err := store.CurrentCandidate(ctx, repo)
	if err != nil || current == nil {
		t.Fatalf("current candidate: %+v %v", current, err)
	}
	if current.Status != model.CandidateCutting {
		t.Fatalf("got status %q, want still cutting", current.Status)
	}
	if current.LastError == "" {
		t.Fatal("expected an error recorded on the candidate")
	}
}

func TestSyncReleasesPromotesTheCandidateAndCutsAReleaseBranch(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if err := store.PutReleaseConfig(ctx, releaseConfig(repo)); err != nil {
		t.Fatalf("put release config: %v", err)
	}
	candidate, err := store.CutCandidate(ctx, repo, baseTime)
	if err != nil {
		t.Fatalf("cut candidate: %v", err)
	}
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("cutting SyncReleases: %v", err)
	}

	// Push a real commit onto the candidate's own branch, straight to the
	// bare repo -- standing in for whatever lands on an rc branch between
	// cutting it and promoting it -- so promoting is a real, visible
	// fast-forward of main rather than a no-op moving it to its own tip.
	pushBranch(t, sim.BareRepo, candidate.Branch)

	promoted, err := store.PromoteCandidate(ctx, repo)
	if err != nil {
		t.Fatalf("promote candidate: %v", err)
	}
	if promoted.ReleaseBranch != "release/3.1" {
		t.Fatalf("got release branch %q, want release/3.1", promoted.ReleaseBranch)
	}

	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("promoting SyncReleases: %v", err)
	}

	candidateHead := mustHead(t, client, "acme", "widgets", candidate.Branch)
	if got := mustHead(t, client, "acme", "widgets", "main"); got != candidateHead {
		t.Fatalf("got prod tip %q, want it moved to the candidate's own tip %q", got, candidateHead)
	}
	if got := mustHead(t, client, "acme", "widgets", "release/3.1"); got != candidateHead {
		t.Fatalf("got release branch tip %q, want %q", got, candidateHead)
	}

	current, err := store.CurrentCandidate(ctx, repo)
	if err != nil || current == nil {
		t.Fatalf("current candidate: %+v %v", current, err)
	}
	if current.Status != model.CandidatePromoted {
		t.Fatalf("got status %q, want promoted", current.Status)
	}
	if current.PromotedAt == nil {
		t.Fatal("expected PromotedAt to be set")
	}

	// The issue's own "it cannot be promoted twice."
	if _, err := store.PromoteCandidate(ctx, repo); err == nil {
		t.Fatal("expected promoting an already-promoted candidate to fail")
	}
}
