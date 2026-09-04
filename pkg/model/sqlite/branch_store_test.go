package sqlite_test

// Branch creation's own store tests (bwsalmon/agents#638), against a real
// embedded SQLite database -- the same discipline release_store_test.go
// already holds every Store method to.

import (
	"errors"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
)

func TestCreateBranchRejectsAnInvalidName(t *testing.T) {
	store, _, ctx := openStore(t)
	for _, name := range []string{"", "/leading-slash", "trailing-slash/", "double//slash", "dotdot..name", "-leading-dash"} {
		if _, err := store.CreateBranch(ctx, widgets, name, now); !errors.Is(err, model.ErrInvalidBranchName) {
			t.Errorf("CreateBranch(%q): got %v, want ErrInvalidBranchName", name, err)
		}
	}
}

func TestCreateBranchRecordsAPendingBranch(t *testing.T) {
	store, _, ctx := openStore(t)
	b, err := store.CreateBranch(ctx, widgets, "feature/foo", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.Repo != widgets || b.Name != "feature/foo" || b.Status != model.BranchPending || b.CreatedAt != now {
		t.Fatalf("got %+v", b)
	}
}

func TestCreateBranchAllowsTheSameNameTwice(t *testing.T) {
	// Unlike a release name, a branch name is never checked for
	// in-store uniqueness -- whether it collides is a fact only GitHub
	// holds, so two requests for the same name are both recorded, and
	// whichever the reconciler tries second adopts the branch the first
	// one left behind.
	store, _, ctx := openStore(t)
	if _, err := store.CreateBranch(ctx, widgets, "myfeat", now); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := store.CreateBranch(ctx, widgets, "myfeat", now); err != nil {
		t.Fatalf("second create: %v", err)
	}
	got, err := store.ListBranches(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d branches, want 2: %+v", len(got), got)
	}
}

func TestPendingBranchesReturnsOnlyPendingOnes(t *testing.T) {
	store, _, ctx := openStore(t)
	a, err := store.CreateBranch(ctx, widgets, "a", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBranch(ctx, widgets, "b", now); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBranchCreated(ctx, a.ID); err != nil {
		t.Fatalf("mark created: %v", err)
	}

	pending, err := store.PendingBranches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Name != "b" {
		t.Fatalf("got %+v, want only %q pending", pending, "b")
	}
}

func TestMarkBranchCreatedClearsLastError(t *testing.T) {
	store, _, ctx := openStore(t)
	b, err := store.CreateBranch(ctx, widgets, "myfeat", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBranchError(ctx, b.ID, "boom"); err != nil {
		t.Fatalf("mark error: %v", err)
	}
	if err := store.MarkBranchCreated(ctx, b.ID); err != nil {
		t.Fatalf("mark created: %v", err)
	}

	got, err := store.ListBranches(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != model.BranchCreated || got[0].LastError != "" {
		t.Fatalf("got %+v, want created with no error", got)
	}
}

func TestMarkBranchAdoptedIsTerminalAndClearsLastError(t *testing.T) {
	store, _, ctx := openStore(t)
	b, err := store.CreateBranch(ctx, widgets, "myfeat", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBranchError(ctx, b.ID, "boom"); err != nil {
		t.Fatalf("mark error: %v", err)
	}
	if err := store.MarkBranchAdopted(ctx, b.ID); err != nil {
		t.Fatalf("mark adopted: %v", err)
	}

	got, err := store.ListBranches(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != model.BranchAdopted || got[0].LastError != "" {
		t.Fatalf("got %+v, want adopted with no error", got)
	}
	// Adopted is as done as created: the reconciler owes it nothing
	// further.
	pending, err := store.PendingBranches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("got %+v, want nothing left pending", pending)
	}
}

func TestListBranchesIsScopedToItsOwnRepo(t *testing.T) {
	store, _, ctx := openStore(t)
	if _, err := store.CreateBranch(ctx, widgets, "myfeat", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBranch(ctx, gadgets, "myfeat", now); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListBranches(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Repo != widgets {
		t.Fatalf("got %+v, want exactly one branch scoped to %s", got, widgets)
	}
}
