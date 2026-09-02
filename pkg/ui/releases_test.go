package ui_test

// Release management's own tests (bwsalmon/agents#571), following
// client_test.go's own discipline: a real embedded SQLite store, no
// GitHub anywhere in sight -- CreateRelease, CutCandidate and
// PromoteCandidate only ever write a row; pkg/orchestrator.SyncReleases
// (its own tests) is what actually talks to GitHub.

import (
	"context"
	"net/http"
	"testing"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

var widgets = model.RepoRef{Owner: "acme", Name: "widgets"}

// cutTestCandidate creates a fresh release named "myfeat" for repo,
// marks it provisioned (standing in for the releases reconciler, which
// none of these store-only tests run), and returns its own first
// candidate -- CreateRelease's own "also cuts its first candidate"
// (bwsalmon/agents#571).
func cutTestCandidate(t *testing.T, ctx context.Context, store *model.Store, repo model.RepoRef) model.Candidate {
	t.Helper()
	r, err := store.CreateRelease(ctx, repo, "myfeat", baseTime)
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	if err := store.MarkReleaseProvisioned(ctx, r.ID); err != nil {
		t.Fatalf("mark provisioned: %v", err)
	}
	c, err := store.CurrentCandidateForRelease(ctx, r.ID)
	if err != nil || c == nil {
		t.Fatalf("current candidate: (%+v, %v)", c, err)
	}
	return *c
}

func TestCreateReleaseRecordsAFreshRelease(t *testing.T) {
	client, _, ctx := testClient(t)
	r, err := client.CreateRelease(ctx, widgets, ui.CreateReleaseRequest{Name: "myfeat"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Repo != "acme/widgets" || r.Name != "myfeat" || r.Status != "provisioning" {
		t.Fatalf("got %+v", r)
	}
	if r.LatestBranch != "myfeat.latest" || r.ProdBranch != "myfeat" {
		t.Fatalf("got %+v, want latestBranch myfeat.latest, prodBranch myfeat", r)
	}
}

func TestCreateReleaseRejectsAnEmptyName(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.CreateRelease(ctx, widgets, ui.CreateReleaseRequest{}); err == nil {
		t.Fatal("expected a validation error for an empty name")
	}
}

func TestCreateReleaseRefusesAnUnmergedNameCollision(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.CreateRelease(ctx, widgets, ui.CreateReleaseRequest{Name: "myfeat"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateRelease(ctx, widgets, ui.CreateReleaseRequest{Name: "myfeat"}); err == nil {
		t.Fatal("expected a validation error cutting a second release under the same unmerged name")
	}
}

func TestListReleasesListsEveryReleaseForARepo(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.CreateRelease(ctx, widgets, ui.CreateReleaseRequest{Name: "myfeat"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateRelease(ctx, widgets, ui.CreateReleaseRequest{Name: "2.1"}); err != nil {
		t.Fatal(err)
	}
	got, err := client.ListReleases(ctx, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d releases, want 2: %+v", len(got), got)
	}
}

func TestGetReleaseReturns404ForAnUnknownName(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.GetRelease(ctx, widgets, "myfeat"); err == nil {
		t.Fatal("expected an error for an unknown release name")
	}
}

func TestCutCandidateRequiresAnActiveRelease(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.CutCandidate(ctx, widgets, "myfeat"); err == nil {
		t.Fatal("expected an error cutting a candidate for a release that does not exist")
	}
}

func TestCutCandidateAllocatesTheFirstOne(t *testing.T) {
	client, store, ctx := testClient(t)
	if _, err := client.CreateRelease(ctx, widgets, ui.CreateReleaseRequest{Name: "myfeat"}); err != nil {
		t.Fatal(err)
	}
	r, err := store.GetRelease(ctx, widgets, "myfeat")
	if err != nil || r == nil {
		t.Fatalf("release: (%+v, %v)", r, err)
	}
	if err := store.MarkReleaseProvisioned(ctx, r.ID); err != nil {
		t.Fatal(err)
	}

	list, err := client.ListCandidates(ctx, widgets, "myfeat")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Branch != "myfeat.rc.1" || list[0].Status != "cutting" {
		t.Fatalf("got %+v, want the one candidate CreateRelease already cut", list)
	}
}

// CutCandidate refuses a second cut while the release's current candidate
// has not been promoted yet -- the issue's own "current rc" is singular.
func TestCutCandidateRefusesASecondWhileOneIsUnpromoted(t *testing.T) {
	client, store, ctx := testClient(t)
	cutTestCandidate(t, ctx, store, widgets)
	if _, err := client.CutCandidate(ctx, widgets, "myfeat"); err == nil {
		t.Fatal("expected an error cutting a second candidate")
	}
}

func TestPromoteCandidateRequiresOneToExist(t *testing.T) {
	client, _, ctx := testClient(t)
	if _, err := client.PromoteCandidate(ctx, widgets, "myfeat"); err == nil {
		t.Fatal("expected an error promoting a release that does not exist")
	}
}

// "It cannot be promoted twice."
func TestPromoteCandidateRefusesASecondPromotion(t *testing.T) {
	client, store, ctx := testClient(t)
	c := cutTestCandidate(t, ctx, store, widgets)
	if err := store.MarkCandidateCut(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	promoted, err := client.PromoteCandidate(ctx, widgets, "myfeat")
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Status != "promoting" {
		t.Fatalf("got %+v", promoted)
	}
	if err := store.MarkCandidatePromoted(ctx, c.ID, baseTime); err != nil {
		t.Fatal(err)
	}

	if _, err := client.PromoteCandidate(ctx, widgets, "myfeat"); err == nil {
		t.Fatal("expected an error promoting an already-promoted candidate")
	}
}

func TestRequestReleaseMergeRequiresAnActiveRelease(t *testing.T) {
	client, store, ctx := testClient(t)
	if _, err := client.RequestReleaseMerge(ctx, widgets, "myfeat"); err == nil {
		t.Fatal("expected an error requesting a merge for a release that does not exist")
	}
	r, err := client.CreateRelease(ctx, widgets, ui.CreateReleaseRequest{Name: "myfeat"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RequestReleaseMerge(ctx, widgets, "myfeat"); err == nil {
		t.Fatal("expected an error requesting a merge while still provisioning")
	}
	got, err := store.GetRelease(ctx, widgets, "myfeat")
	if err != nil || got == nil {
		t.Fatalf("release: (%+v, %v)", got, err)
	}
	if err := store.MarkReleaseProvisioned(ctx, got.ID); err != nil {
		t.Fatal(err)
	}
	merging, err := client.RequestReleaseMerge(ctx, widgets, "myfeat")
	if err != nil {
		t.Fatal(err)
	}
	if merging.Status != "merge_requested" {
		t.Fatalf("got %+v", merging)
	}
	if r.Name != "myfeat" {
		t.Fatalf("sanity: %+v", r)
	}
}

// --- HTTP routes -----------------------------------------------------

func TestReleaseRoutesCreateGetAndList(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/repos/acme/widgets/releases", `{"name":"myfeat"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body)
	}
	created := decode[ui.Release](t, rec)
	if created.Name != "myfeat" || created.Status != "provisioning" {
		t.Fatalf("created = %+v", created)
	}

	rec = do(t, srv, http.MethodGet, "/api/repos/acme/widgets/releases/myfeat", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decode[ui.Release](t, rec)
	if got.Name != "myfeat" {
		t.Fatalf("got = %+v", got)
	}

	rec = do(t, srv, http.MethodGet, "/api/repos/acme/widgets/releases", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if list := decode[[]ui.Release](t, rec); len(list) != 1 {
		t.Fatalf("got %+v, want exactly one release", list)
	}

	rec = do(t, srv, http.MethodGet, "/api/repos/acme/widgets/releases/does-not-exist", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestReleaseCreationRejectionsAre400(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPost, "/api/repos/acme/widgets/releases", `{"name":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestCandidateRoutesCutAndPromote(t *testing.T) {
	srv, client := testServer(t)
	if _, err := client.CreateRelease(context.Background(), widgets, ui.CreateReleaseRequest{Name: "myfeat"}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, srv, http.MethodGet, "/api/repos/acme/widgets/releases/myfeat/candidates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", rec.Code, rec.Body)
	}
	cut := decode[[]ui.Candidate](t, rec)
	if len(cut) != 1 || cut[0].Status != "cutting" || cut[0].Branch != "myfeat.rc.1" {
		t.Fatalf("got %+v", cut)
	}

	// Cutting a second one before the first has promoted is a caller
	// mistake -- 400, not 500.
	rec = do(t, srv, http.MethodPost, "/api/repos/acme/widgets/releases/myfeat/candidates", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("second cut status = %d, want 400: %s", rec.Code, rec.Body)
	}

	// Promoting before the releases reconciler has advanced the candidate
	// out of "cutting" is a caller mistake -- 400, not 500.
	rec = do(t, srv, http.MethodPost, "/api/repos/acme/widgets/releases/myfeat/candidates/promote", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("premature promote status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestRequestReleaseMergeRoute(t *testing.T) {
	srv, client := testServer(t)
	ctx := context.Background()
	if _, err := client.CreateRelease(ctx, widgets, ui.CreateReleaseRequest{Name: "myfeat"}); err != nil {
		t.Fatal(err)
	}

	// Still provisioning -- a caller mistake, 400.
	rec := do(t, srv, http.MethodPost, "/api/repos/acme/widgets/releases/myfeat/merge", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}
