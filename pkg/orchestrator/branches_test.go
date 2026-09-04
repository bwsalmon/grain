package orchestrator_test

// SyncBranches' own tests (bwsalmon/agents#638), against a real bare git
// repo behind githubsim.Sim -- releases_test.go's own doc comment on why
// branch creation needs a real git repo behind it, not just a
// FakeTransport standing in for GitHub, applies equally here.

import (
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

func TestSyncBranchesCreatesAPendingBranchAtTheDefaultBranchTip(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	b, err := store.CreateBranch(ctx, repo, "myfeat", baseTime)
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	if err := orchestrator.SyncBranches(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncBranches: %v", err)
	}

	mainHead := mustHead(t, client, "acme", "widgets", "main")
	if got := mustHead(t, client, "acme", "widgets", "myfeat"); got != mainHead {
		t.Fatalf("got branch tip %q, want it pinned at main's own tip %q", got, mainHead)
	}

	got, err := store.ListBranches(ctx, repo)
	if err != nil || len(got) != 1 {
		t.Fatalf("list branches: (%+v, %v)", got, err)
	}
	if got[0].ID != b.ID || got[0].Status != model.BranchCreated || got[0].LastError != "" {
		t.Fatalf("got %+v, want created with no error", got[0])
	}
}

func TestSyncBranchesIsIdempotentOnceCreated(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if _, err := store.CreateBranch(ctx, repo, "myfeat", baseTime); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if err := orchestrator.SyncBranches(ctx, store, client, baseTime); err != nil {
		t.Fatalf("first SyncBranches: %v", err)
	}
	// The branch already advanced to created, so a second run finds
	// nothing pending and must not try (and fail) to create it again.
	if err := orchestrator.SyncBranches(ctx, store, client, baseTime); err != nil {
		t.Fatalf("second SyncBranches: %v", err)
	}
}

func TestSyncBranchesAdoptsABranchThatAlreadyExistsOnGitHub(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	// A branch somebody pushed by hand, ahead of main -- grain/task-176's
	// own "if a branch already exists on a repo, we should be able to add
	// it to grain."
	pushBranch(t, sim.BareRepo, "myfeat")
	existing := mustHead(t, client, "acme", "widgets", "myfeat")

	if _, err := store.CreateBranch(ctx, repo, "myfeat", baseTime); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if err := orchestrator.SyncBranches(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncBranches: %v", err)
	}

	got, err := store.ListBranches(ctx, repo)
	if err != nil || len(got) != 1 {
		t.Fatalf("list branches: (%+v, %v)", got, err)
	}
	if got[0].Status != model.BranchAdopted || got[0].LastError != "" {
		t.Fatalf("got %+v, want adopted with no error", got[0])
	}
	// Adoption is a bookkeeping act: the commits the branch already
	// pointed at are still what it points at, not the default branch tip
	// a create would have pinned it to.
	if now := mustHead(t, client, "acme", "widgets", "myfeat"); now != existing {
		t.Fatalf("got branch tip %q, want the existing %q left alone", now, existing)
	}
}

func TestSyncBranchesIsIdempotentOnceAdopted(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	pushBranch(t, sim.BareRepo, "myfeat")
	if _, err := store.CreateBranch(ctx, repo, "myfeat", baseTime); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if err := orchestrator.SyncBranches(ctx, store, client, baseTime); err != nil {
		t.Fatalf("first SyncBranches: %v", err)
	}
	// An adopted branch is as terminal as a created one: the next cycle
	// finds nothing pending and goes nowhere near GitHub for it.
	if err := orchestrator.SyncBranches(ctx, store, client, baseTime); err != nil {
		t.Fatalf("second SyncBranches: %v", err)
	}

	got, err := store.ListBranches(ctx, repo)
	if err != nil || len(got) != 1 {
		t.Fatalf("list branches: (%+v, %v)", got, err)
	}
	if got[0].Status != model.BranchAdopted {
		t.Fatalf("got status %q, want still adopted", got[0].Status)
	}
}

func TestSyncBranchesAdoptsTheDefaultBranchItself(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	// "main" is the seeded default branch, and asking to create a fresh
	// branch by that name used to be the collision GitHub answers 422 to
	// (CreateBranch's own doc comment) and grain recorded as an error
	// forever. It is now the plainest case of adopting what is there.
	if _, err := store.CreateBranch(ctx, repo, "main", baseTime); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if err := orchestrator.SyncBranches(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncBranches: %v", err)
	}

	got, err := store.ListBranches(ctx, repo)
	if err != nil || len(got) != 1 {
		t.Fatalf("list branches: (%+v, %v)", got, err)
	}
	if got[0].Status != model.BranchAdopted || got[0].LastError != "" {
		t.Fatalf("got %+v, want adopted with no error", got[0])
	}
}

func TestSyncBranchesRecordsAnErrorWhenTheDefaultBranchIsMissing(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	if _, err := store.CreateBranch(ctx, repo, "myfeat", baseTime); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	run(t, sim.BareRepo, "git", "update-ref", "-d", "refs/heads/main")

	if err := orchestrator.SyncBranches(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncBranches: %v", err)
	}

	got, err := store.ListBranches(ctx, repo)
	if err != nil || len(got) != 1 {
		t.Fatalf("list branches: (%+v, %v)", got, err)
	}
	if got[0].Status != model.BranchPending || got[0].LastError == "" {
		t.Fatalf("got %+v, want still pending with an error recorded", got[0])
	}
}
