package sqlite_test

// Release management's own store tests (bwsalmon/agents#398), against a
// real embedded SQLite database -- the same discipline store_test.go
// holds every other Store method to, for the same reason its own doc
// comment gives: this proves SQLite accepts the DDL and the queries
// answer, not merely that grain's own Go logic is self-consistent.

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

var (
	widgets = model.RepoRef{Owner: "acme", Name: "widgets"}
	gadgets = model.RepoRef{Owner: "acme", Name: "gadgets"}
)

func testReleaseConfig(repo model.RepoRef) model.ReleaseConfig {
	return model.ReleaseConfig{
		Repo: repo, ProdBranch: "main", RCBranch: "rc", ReleaseBranchPrefix: "release/", MajorVersion: 3,
	}
}

func TestGetReleaseConfigReturnsNilOnAFreshDatabase(t *testing.T) {
	store, _, ctx := openStore(t)
	got, err := store.GetReleaseConfig(ctx, widgets)
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) before anything has configured %s, got (%+v, %v)", widgets, got, err)
	}
}

func TestReleaseConfigRoundTrips(t *testing.T) {
	store, _, ctx := openStore(t)
	want := testReleaseConfig(widgets)
	if err := store.PutReleaseConfig(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.GetReleaseConfig(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
}

// PutReleaseConfig replaces a repo's single row wholesale rather than
// accumulating a second one -- the same discipline
// TestPutConfigReplacesRatherThanAccumulating pins for the deployment-wide
// grain_config row.
func TestPutReleaseConfigReplacesRatherThanAccumulating(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("first put: %v", err)
	}
	updated := testReleaseConfig(widgets)
	updated.MajorVersion = 4
	updated.RCBranch = "release-candidate"
	if err := store.PutReleaseConfig(ctx, updated); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err := store.GetReleaseConfig(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("get: (%+v, %v)", got, err)
	}
	if !reflect.DeepEqual(*got, updated) {
		t.Fatalf("got %+v, want %+v", *got, updated)
	}
	configs, err := store.ListReleaseConfigs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("want exactly one configured repo, got %d: %+v", len(configs), configs)
	}
}

func TestListReleaseConfigsOrdersByRepo(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(gadgets)); err != nil {
		t.Fatalf("put gadgets: %v", err)
	}
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put widgets: %v", err)
	}
	got, err := store.ListReleaseConfigs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].Repo != gadgets || got[1].Repo != widgets {
		t.Fatalf("got %+v, want [gadgets, widgets] in that order", got)
	}
}

func TestCutCandidateRequiresReleaseConfig(t *testing.T) {
	store, _, ctx := openStore(t)
	_, err := store.CutCandidate(ctx, widgets, now)
	if !errors.Is(err, model.ErrNoReleaseConfig) {
		t.Fatalf("got %v, want ErrNoReleaseConfig", err)
	}
}

func TestCutCandidateAllocatesTheFirstCandidateAsNumberOne(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put config: %v", err)
	}
	c, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	want := model.Candidate{
		ID: c.ID, Repo: widgets, MajorVersion: 3, Number: 1, Version: 1,
		Branch: "release/3.1-rc1", Status: model.CandidateCutting, CreatedAt: now,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v, want %+v", c, want)
	}

	current, err := store.CurrentCandidate(ctx, widgets)
	if err != nil || current == nil {
		t.Fatalf("current: (%+v, %v)", current, err)
	}
	if !reflect.DeepEqual(*current, want) {
		t.Fatalf("current: got %+v, want %+v", *current, want)
	}
}

// CutCandidate refuses a fresh cut while the repo's current candidate has
// not been promoted yet -- the issue's own "the current rc" is singular.
func TestCutCandidateRefusesASecondCandidateWhileOneIsUnpromoted(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put config: %v", err)
	}
	if _, err := store.CutCandidate(ctx, widgets, now); err != nil {
		t.Fatalf("first cut: %v", err)
	}
	if _, err := store.CutCandidate(ctx, widgets, now); !errors.Is(err, model.ErrCandidateActive) {
		t.Fatalf("got %v, want ErrCandidateActive", err)
	}
}

// Once a candidate promotes, cutting a fresh one allocates the next
// number rather than reusing or resetting it.
func TestCutCandidateAfterPromotionAllocatesTheNextNumber(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put config: %v", err)
	}
	first, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("first cut: %v", err)
	}
	if err := store.MarkCandidateCut(ctx, first.ID); err != nil {
		t.Fatalf("mark cut: %v", err)
	}
	if _, err := store.PromoteCandidate(ctx, widgets); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := store.MarkCandidatePromoted(ctx, first.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("mark promoted: %v", err)
	}

	second, err := store.CutCandidate(ctx, widgets, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("second cut: %v", err)
	}
	if second.Number != 2 {
		t.Fatalf("got number %d, want 2", second.Number)
	}
	if second.Branch != "release/3.2-rc1" {
		t.Fatalf("got branch %q, want release/3.2-rc1", second.Branch)
	}
}

func TestPromoteCandidateRequiresACandidate(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put config: %v", err)
	}
	if _, err := store.PromoteCandidate(ctx, widgets); !errors.Is(err, model.ErrNoCandidate) {
		t.Fatalf("got %v, want ErrNoCandidate", err)
	}
}

// A candidate still cutting (its branch not yet live on GitHub, per the
// releases reconciler) cannot be promoted out from under that in-flight
// step.
func TestPromoteCandidateRequiresItToHaveFinishedCutting(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put config: %v", err)
	}
	if _, err := store.CutCandidate(ctx, widgets, now); err != nil {
		t.Fatalf("cut: %v", err)
	}
	if _, err := store.PromoteCandidate(ctx, widgets); !errors.Is(err, model.ErrCandidateNotReady) {
		t.Fatalf("got %v, want ErrCandidateNotReady", err)
	}
}

// The issue's own "it cannot be promoted twice."
func TestPromoteCandidateRefusesASecondPromotion(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put config: %v", err)
	}
	c, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if err := store.MarkCandidateCut(ctx, c.ID); err != nil {
		t.Fatalf("mark cut: %v", err)
	}
	promoted, err := store.PromoteCandidate(ctx, widgets)
	if err != nil {
		t.Fatalf("first promote: %v", err)
	}
	if promoted.ReleaseBranch != "release/3.1" {
		t.Fatalf("got release branch %q, want release/3.1", promoted.ReleaseBranch)
	}
	if err := store.MarkCandidatePromoted(ctx, c.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("mark promoted: %v", err)
	}

	if _, err := store.PromoteCandidate(ctx, widgets); !errors.Is(err, model.ErrAlreadyPromoted) {
		t.Fatalf("got %v, want ErrAlreadyPromoted", err)
	}
}

func TestPendingCandidatesSpansEveryRepo(t *testing.T) {
	store, _, ctx := openStore(t)
	for _, repo := range []model.RepoRef{widgets, gadgets} {
		if err := store.PutReleaseConfig(ctx, testReleaseConfig(repo)); err != nil {
			t.Fatalf("put config for %s: %v", repo, err)
		}
	}
	w, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("cut widgets: %v", err)
	}
	g, err := store.CutCandidate(ctx, gadgets, now)
	if err != nil {
		t.Fatalf("cut gadgets: %v", err)
	}
	if err := store.MarkCandidateCut(ctx, g.ID); err != nil {
		t.Fatalf("mark gadgets cut: %v", err)
	}
	if _, err := store.PromoteCandidate(ctx, gadgets); err != nil {
		t.Fatalf("promote gadgets: %v", err)
	}

	pending, err := store.PendingCandidates(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("got %d pending, want 2: %+v", len(pending), pending)
	}
	if pending[0].ID != w.ID || pending[0].Status != model.CandidateCutting {
		t.Fatalf("got %+v, want widgets still cutting first", pending[0])
	}
	if pending[1].ID != g.ID || pending[1].Status != model.CandidatePromoting {
		t.Fatalf("got %+v, want gadgets promoting second", pending[1])
	}
}

func TestMarkCandidateErrorRecordsTheMessageWithoutChangingStatus(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put config: %v", err)
	}
	c, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if err := store.MarkCandidateError(ctx, c.ID, "prod branch not found"); err != nil {
		t.Fatalf("mark error: %v", err)
	}
	got, err := store.CurrentCandidate(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("current: (%+v, %v)", got, err)
	}
	if got.Status != model.CandidateCutting {
		t.Fatalf("got status %q, want still cutting", got.Status)
	}
	if got.LastError != "prod branch not found" {
		t.Fatalf("got error %q, want %q", got.LastError, "prod branch not found")
	}

	// A later success clears it.
	if err := store.MarkCandidateCut(ctx, c.ID); err != nil {
		t.Fatalf("mark cut: %v", err)
	}
	got, err = store.CurrentCandidate(ctx, widgets)
	if err != nil || got == nil {
		t.Fatalf("current: (%+v, %v)", got, err)
	}
	if got.LastError != "" {
		t.Fatalf("got error %q, want cleared", got.LastError)
	}
}

func TestListCandidatesReturnsNewestFirst(t *testing.T) {
	store, _, ctx := openStore(t)
	if err := store.PutReleaseConfig(ctx, testReleaseConfig(widgets)); err != nil {
		t.Fatalf("put config: %v", err)
	}
	first, err := store.CutCandidate(ctx, widgets, now)
	if err != nil {
		t.Fatalf("first cut: %v", err)
	}
	if err := store.MarkCandidateCut(ctx, first.ID); err != nil {
		t.Fatalf("mark cut: %v", err)
	}
	if _, err := store.PromoteCandidate(ctx, widgets); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := store.MarkCandidatePromoted(ctx, first.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("mark promoted: %v", err)
	}
	second, err := store.CutCandidate(ctx, widgets, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("second cut: %v", err)
	}

	list, err := store.ListCandidates(ctx, widgets)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("got %+v, want [second, first]", list)
	}
}
