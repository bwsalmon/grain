package orchestrator_test

// SyncReleases' own tests (bwsalmon/agents#571), against a real bare git
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

func TestSyncReleasesProvisionsLatestBranchAndCutsTheFirstCandidate(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	release, err := store.CreateRelease(ctx, repo, "myfeat", baseTime)
	if err != nil {
		t.Fatalf("create release: %v", err)
	}

	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncReleases: %v", err)
	}

	mainHead := mustHead(t, client, "acme", "widgets", "main")
	if got := mustHead(t, client, "acme", "widgets", "myfeat.latest"); got != mainHead {
		t.Fatalf("got latest branch tip %q, want it pinned at main's own tip %q", got, mainHead)
	}
	if got := mustHead(t, client, "acme", "widgets", "myfeat.rc.1"); got != mainHead {
		t.Fatalf("got first candidate branch tip %q, want %q", got, mainHead)
	}

	gotRelease, err := store.GetReleaseByID(ctx, release.ID)
	if err != nil || gotRelease == nil {
		t.Fatalf("release: (%+v, %v)", gotRelease, err)
	}
	if gotRelease.Status != model.ReleaseActive {
		t.Fatalf("got release status %q, want active", gotRelease.Status)
	}

	current, err := store.CurrentCandidateForRelease(ctx, release.ID)
	if err != nil || current == nil {
		t.Fatalf("current candidate: %+v %v", current, err)
	}
	if current.Status != model.CandidateActive {
		t.Fatalf("got candidate status %q, want active", current.Status)
	}
	if current.LastError != "" {
		t.Fatalf("got error %q, want none", current.LastError)
	}
	if len(sim.Calls) == 0 {
		t.Fatal("expected SyncReleases to have called out to GitHub")
	}
}

func TestSyncReleasesIsIdempotentOnceProvisioned(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if _, err := store.CreateRelease(ctx, repo, "myfeat", baseTime); err != nil {
		t.Fatalf("create release: %v", err)
	}
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("first SyncReleases: %v", err)
	}
	// A second run finds nothing pending -- the release already advanced
	// to active and its first candidate to active -- and must not error
	// re-creating branches that already exist.
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("second SyncReleases: %v", err)
	}
}

func TestSyncReleasesRecordsAnErrorWhenTheDefaultBranchIsMissing(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if _, err := store.CreateRelease(ctx, repo, "myfeat", baseTime); err != nil {
		t.Fatalf("create release: %v", err)
	}

	// DefaultBranch's own endpoint just reports the configured name
	// (githubsim's own doc comment) without checking it actually exists,
	// so deleting the ref out from under it -- something a human
	// force-pushing or renaming a repo's default branch could do for
	// real -- is what reproduces "the default branch does not exist"
	// without giving CreateRelease itself some separate, made-up branch
	// name to misconfigure the way bwsalmon/agents#398's own
	// ReleaseConfig.ProdBranch let a test do.
	run(t, sim.BareRepo, "git", "update-ref", "-d", "refs/heads/main")

	// A release-specific failure must not surface as a cycle error -- see
	// syncRelease's own doc comment on why -- only get recorded onto the
	// row for a UI to show, so the next cycle can simply retry it.
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncReleases: %v", err)
	}

	current, err := store.GetRelease(ctx, repo, "myfeat")
	if err != nil || current == nil {
		t.Fatalf("current release: %+v %v", current, err)
	}
	if current.Status != model.ReleaseProvisioning {
		t.Fatalf("got status %q, want still provisioning", current.Status)
	}
	if current.LastError == "" {
		t.Fatal("expected an error recorded on the release")
	}
}

func TestSyncReleasesPromotesTheCandidateToTheProdBranch(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	release, err := store.CreateRelease(ctx, repo, "myfeat", baseTime)
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("provisioning SyncReleases: %v", err)
	}
	candidate, err := store.CurrentCandidateForRelease(ctx, release.ID)
	if err != nil || candidate == nil {
		t.Fatalf("candidate: (%+v, %v)", candidate, err)
	}

	// Push a real commit onto the candidate's own branch, straight to the
	// bare repo -- standing in for whatever lands on an rc branch between
	// cutting it and promoting it -- so promoting is a real, visible
	// fast-forward of the prod branch rather than a no-op moving it to
	// its own tip.
	pushBranch(t, sim.BareRepo, candidate.Branch)

	if _, err := store.PromoteCandidate(ctx, repo, "myfeat"); err != nil {
		t.Fatalf("promote candidate: %v", err)
	}

	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("promoting SyncReleases: %v", err)
	}

	candidateHead := mustHead(t, client, "acme", "widgets", candidate.Branch)
	if got := mustHead(t, client, "acme", "widgets", "myfeat"); got != candidateHead {
		t.Fatalf("got prod tip %q, want it moved to the candidate's own tip %q", got, candidateHead)
	}

	current, err := store.CurrentCandidateForRelease(ctx, release.ID)
	if err != nil || current == nil {
		t.Fatalf("current candidate: %+v %v", current, err)
	}
	if current.Status != model.CandidatePromoted {
		t.Fatalf("got status %q, want promoted", current.Status)
	}
	if current.PromotedAt == nil {
		t.Fatal("expected PromotedAt to be set")
	}

	// "It cannot be promoted twice."
	if _, err := store.PromoteCandidate(ctx, repo, "myfeat"); err == nil {
		t.Fatal("expected promoting an already-promoted candidate to fail")
	}
}

func TestSyncReleasesOpensAMergeBackPullRequest(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	release, err := store.CreateRelease(ctx, repo, "myfeat", baseTime)
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("provisioning SyncReleases: %v", err)
	}
	candidate, err := store.CurrentCandidateForRelease(ctx, release.ID)
	if err != nil || candidate == nil {
		t.Fatalf("candidate: (%+v, %v)", candidate, err)
	}
	pushBranch(t, sim.BareRepo, candidate.Branch)
	if _, err := store.PromoteCandidate(ctx, repo, "myfeat"); err != nil {
		t.Fatalf("promote candidate: %v", err)
	}
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("promoting SyncReleases: %v", err)
	}

	if _, err := store.RequestReleaseMerge(ctx, repo, "myfeat"); err != nil {
		t.Fatalf("request merge: %v", err)
	}
	if err := orchestrator.SyncReleases(ctx, store, client, baseTime); err != nil {
		t.Fatalf("merging SyncReleases: %v", err)
	}

	current, err := store.GetRelease(ctx, repo, "myfeat")
	if err != nil || current == nil {
		t.Fatalf("current release: %+v %v", current, err)
	}
	if current.Status != model.ReleaseMerged {
		t.Fatalf("got status %q, want merged", current.Status)
	}
	if current.PullRequestURL == "" {
		t.Fatal("expected a pull request URL to be recorded")
	}
	if current.MergedAt == nil {
		t.Fatal("expected MergedAt to be set")
	}

	// Once merged, the name is free again.
	if _, err := store.CreateRelease(ctx, repo, "myfeat", baseTime); err != nil {
		t.Fatalf("expected the release name to be reusable once merged: %v", err)
	}
}
