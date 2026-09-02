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

func TestSyncBranchesRecordsAnErrorWhenTheNameAlreadyExistsOnGitHub(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	// "main" already exists as the seeded default branch -- asking to
	// create a fresh branch by that same name is exactly the collision
	// GitHub itself answers 422 to (CreateBranch's own doc comment).
	if _, err := store.CreateBranch(ctx, repo, "main", baseTime); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// A branch-specific failure must not surface as a cycle error -- see
	// syncBranch's own doc comment on why -- only get recorded onto the
	// row for a UI to show, so the next cycle can retry (and keep
	// failing the same way until a human notices and picks another
	// name).
	if err := orchestrator.SyncBranches(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncBranches: %v", err)
	}

	got, err := store.ListBranches(ctx, repo)
	if err != nil || len(got) != 1 {
		t.Fatalf("list branches: (%+v, %v)", got, err)
	}
	if got[0].Status != model.BranchPending {
		t.Fatalf("got status %q, want still pending", got[0].Status)
	}
	if got[0].LastError == "" {
		t.Fatal("expected an error recorded on the branch")
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
