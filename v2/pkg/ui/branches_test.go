package ui_test

// Branch creation's own tests (bwsalmon/agents#638), following
// releases_test.go's own discipline: a real embedded SQLite store, no
// GitHub anywhere in sight -- CreateBranch only ever writes a row;
// pkg/orchestrator.SyncBranches (its own tests) is what actually talks to
// GitHub.

import (
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func TestCreateBranchRecordsAFreshBranch(t *testing.T) {
	client, _, ctx := testClient(t)
	b, err := client.CreateBranch(ctx, widgets, ui.CreateBranchRequest{Name: "myfeat"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Repo != "acme/widgets" || b.Name != "myfeat" || b.Status != "pending" {
		t.Fatalf("got %+v", b)
	}
}

func TestCreateBranchTrimsWhitespaceFromTheName(t *testing.T) {
	client, _, ctx := testClient(t)
	b, err := client.CreateBranch(ctx, widgets, ui.CreateBranchRequest{Name: "  myfeat  "})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "myfeat" {
		t.Fatalf("got name %q, want it trimmed to %q", b.Name, "myfeat")
	}
}

func TestCreateBranchRejectsAnEmptyName(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.CreateBranch(ctx, widgets, ui.CreateBranchRequest{}); err == nil {
		t.Fatal("expected a validation error for an empty name")
	}
}

func TestCreateBranchAllowsRequestingTheSameNameTwice(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.CreateBranch(ctx, widgets, ui.CreateBranchRequest{Name: "myfeat"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateBranch(ctx, widgets, ui.CreateBranchRequest{Name: "myfeat"}); err != nil {
		t.Fatal(err)
	}
}

func TestListBranchesListsEveryBranchForARepoNewestFirst(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.CreateBranch(ctx, widgets, ui.CreateBranchRequest{Name: "myfeat"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateBranch(ctx, widgets, ui.CreateBranchRequest{Name: "otherfeat"}); err != nil {
		t.Fatal(err)
	}
	got, err := client.ListBranches(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "otherfeat" || got[1].Name != "myfeat" {
		t.Fatalf("got %+v, want otherfeat then myfeat", got)
	}
}
