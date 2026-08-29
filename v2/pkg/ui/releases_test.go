package ui_test

// Release management's own tests (bwsalmon/agents#398), following
// client_test.go's own discipline: a real embedded SQLite store, no
// GitHub anywhere in sight -- CutCandidate and PromoteCandidate only ever
// write a model.Candidate row; pkg/orchestrator.SyncReleases (its own
// tests) is what actually talks to GitHub.

import (
	"context"
	"net/http"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

var widgets = model.RepoRef{Owner: "acme", Name: "widgets"}

func TestGetReleaseConfigReportsUnconfigured(t *testing.T) {
	client, _, ctx := testClient(t)
	cfg, err := client.GetReleaseConfig(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Configured {
		t.Fatalf("got %+v, want Configured false on a fresh store", cfg)
	}
}

func TestPutThenGetReleaseConfigRoundTrips(t *testing.T) {
	client, _, ctx := testClient(t)
	saved, err := client.PutReleaseConfig(ctx, widgets, ui.UpdateReleaseConfigRequest{
		ProdBranch: "main", RCBranch: "rc", ReleaseBranchPrefix: "release/", MajorVersion: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Configured || saved.Repo != "acme/widgets" || saved.MajorVersion != 3 {
		t.Fatalf("got %+v", saved)
	}
	got, err := client.GetReleaseConfig(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if got != saved {
		t.Fatalf("got %+v, want %+v", got, saved)
	}
}

func TestPutReleaseConfigRejectsAnEmptyProdBranch(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.PutReleaseConfig(ctx, widgets, ui.UpdateReleaseConfigRequest{
		RCBranch: "rc", MajorVersion: 3,
	}); err == nil {
		t.Fatal("expected a validation error")
	}
}

func TestListReleaseConfigsListsEveryConfiguredRepo(t *testing.T) {
	client, _, ctx := testClient(t)
	gadgets := model.RepoRef{Owner: "acme", Name: "gadgets"}
	for _, repo := range []model.RepoRef{widgets, gadgets} {
		if _, err := client.PutReleaseConfig(ctx, repo, ui.UpdateReleaseConfigRequest{
			ProdBranch: "main", RCBranch: "rc", MajorVersion: 1,
		}); err != nil {
			t.Fatalf("put %s: %v", repo, err)
		}
	}
	got, err := client.ListReleaseConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d configs, want 2: %+v", len(got), got)
	}
}

func TestCutCandidateRequiresAConfiguredRepo(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.CutCandidate(ctx, widgets); err == nil {
		t.Fatal("expected an error cutting a candidate for an unconfigured repo")
	}
}

func TestCutCandidateAllocatesTheFirstOne(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.PutReleaseConfig(ctx, widgets, ui.UpdateReleaseConfigRequest{
		ProdBranch: "main", RCBranch: "rc", ReleaseBranchPrefix: "release/", MajorVersion: 3,
	}); err != nil {
		t.Fatal(err)
	}
	c, err := client.CutCandidate(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if c.Label != "3.1-rc1" || c.Branch != "release/3.1-rc1" || c.Status != "cutting" {
		t.Fatalf("got %+v", c)
	}

	list, err := client.ListCandidates(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("got %+v, want the one candidate just cut", list)
	}
}

// CutCandidate refuses a second cut while the repo's current candidate
// has not been promoted yet -- the issue's own "the current rc" is
// singular.
func TestCutCandidateRefusesASecondWhileOneIsUnpromoted(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.PutReleaseConfig(ctx, widgets, ui.UpdateReleaseConfigRequest{
		ProdBranch: "main", RCBranch: "rc", MajorVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CutCandidate(ctx, widgets); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CutCandidate(ctx, widgets); err == nil {
		t.Fatal("expected an error cutting a second candidate")
	}
}

func TestPromoteCandidateRequiresOneToExist(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.PutReleaseConfig(ctx, widgets, ui.UpdateReleaseConfigRequest{
		ProdBranch: "main", RCBranch: "rc", MajorVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PromoteCandidate(ctx, widgets); err == nil {
		t.Fatal("expected an error promoting with no candidate cut")
	}
}

// The issue's own "it cannot be promoted twice": once the releases
// reconciler marks a candidate promoted, a second PromoteCandidate call
// must fail.
func TestPromoteCandidateRefusesASecondPromotion(t *testing.T) {
	client, store, ctx := testClient(t)
	if _, err := client.PutReleaseConfig(ctx, widgets, ui.UpdateReleaseConfigRequest{
		ProdBranch: "main", RCBranch: "rc", ReleaseBranchPrefix: "release/", MajorVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	c, err := client.CutCandidate(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCandidateCut(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	promoted, err := client.PromoteCandidate(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Status != "promoting" || promoted.ReleaseBranch != "release/1.1" {
		t.Fatalf("got %+v", promoted)
	}
	if err := store.MarkCandidatePromoted(ctx, c.ID, baseTime); err != nil {
		t.Fatal(err)
	}

	if _, err := client.PromoteCandidate(ctx, widgets); err == nil {
		t.Fatal("expected an error promoting an already-promoted candidate")
	}
}

// --- HTTP routes -----------------------------------------------------

func TestReleaseConfigRoutesReadAndWrite(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodGet, "/api/repos/acme/widgets/release-config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := decode[ui.ReleaseConfig](t, rec); got.Configured {
		t.Fatalf("got %+v, want unconfigured", got)
	}

	rec = do(t, srv, http.MethodPut, "/api/repos/acme/widgets/release-config",
		`{"prodBranch":"main","rcBranch":"rc","releaseBranchPrefix":"release/","majorVersion":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200: %s", rec.Code, rec.Body)
	}
	saved := decode[ui.ReleaseConfig](t, rec)
	if !saved.Configured || saved.MajorVersion != 3 {
		t.Fatalf("saved = %+v", saved)
	}

	rec = do(t, srv, http.MethodGet, "/api/release-configs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if list := decode[[]ui.ReleaseConfig](t, rec); len(list) != 1 {
		t.Fatalf("got %+v, want exactly one configured repo", list)
	}
}

func TestReleaseConfigRejectionsAre400(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPut, "/api/repos/acme/widgets/release-config", `{"rcBranch":"rc"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestCandidateRoutesCutAndPromote(t *testing.T) {
	srv, client := testServer(t)
	if _, err := client.PutReleaseConfig(context.Background(), widgets, ui.UpdateReleaseConfigRequest{
		ProdBranch: "main", RCBranch: "rc", ReleaseBranchPrefix: "release/", MajorVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, http.MethodPost, "/api/repos/acme/widgets/candidates", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("cut status = %d, want 201: %s", rec.Code, rec.Body)
	}
	cut := decode[ui.Candidate](t, rec)
	if cut.Status != "cutting" || cut.Label != "1.1-rc1" {
		t.Fatalf("got %+v", cut)
	}

	rec = do(t, srv, http.MethodGet, "/api/repos/acme/widgets/candidates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if list := decode[[]ui.Candidate](t, rec); len(list) != 1 {
		t.Fatalf("got %+v, want one candidate", list)
	}

	// Promoting before the releases reconciler has advanced the candidate
	// out of "cutting" is a caller mistake -- 400, not 500.
	rec = do(t, srv, http.MethodPost, "/api/repos/acme/widgets/candidates/promote", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("premature promote status = %d, want 400: %s", rec.Code, rec.Body)
	}
}
