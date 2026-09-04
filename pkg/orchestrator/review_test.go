package orchestrator_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/github/githubsim"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// reviewTemplate puts a template in the store for a task to name as its
// review -- the instructions a reviewing agent is dispatched with, which
// grain/task-284 keeps in a template rather than repeating on every task
// that wants one.
func reviewTemplate(t *testing.T, ctx context.Context, store *model.Store) model.Template {
	t.Helper()
	tmpl := model.Template{
		ID:        "rt1",
		Name:      "Bug hunt",
		Title:     "Review the proposed change",
		Body:      "Read the diff on this branch and fix the bugs you find.",
		Reads:     []model.RepoRef{{Owner: "acme", Name: "docs"}},
		CreatedAt: baseTime,
	}
	if err := store.PutTemplate(ctx, tmpl); err != nil {
		t.Fatalf("filing template: %v", err)
	}
	return tmpl
}

// completedTask is a task whose run is over and whose pull request is
// open: the state SyncReviews and the merge queue both read a task in.
func completedTask(t *testing.T, ctx context.Context, store *model.Store, sim *githubsim.Sim,
	client github.Client, id string, repo model.RepoRef, autoMerge bool, reviewTemplateID string) model.Task {

	t.Helper()
	task := filedTask(t, ctx, store, id, repo)
	task.AutoMerge = autoMerge
	task.ReviewTemplateID = reviewTemplateID
	task.CreatedAt = &baseTime
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
	completed := baseTime
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &completed}); err != nil {
		t.Fatal(err)
	}
	return task
}

// markMergeable is GitHub having finished computing mergeability for
// every pull request the sim currently holds -- healthFrom reads a nil
// Mergeable as PrUnknown and merges nothing on it, so a test about what
// the queue does with a *clean* pull request has to say so.
func markMergeable(sim *githubsim.Sim) {
	yes := true
	for i := range sim.PullRequests {
		sim.PullRequests[i].Mergeable = &yes
	}
}

// reviewTaskOf returns the task grain filed to review taskID, or fails.
func reviewTaskOf(t *testing.T, ctx context.Context, store *model.Store, taskID string) model.Task {
	t.Helper()
	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range parent.Links {
		if l.Kind != model.LinkReviewTask {
			continue
		}
		reviewTask, err := store.GetTask(ctx, l.Target)
		if err != nil {
			t.Fatal(err)
		}
		if reviewTask == nil {
			t.Fatalf("task %s links to review task %s, which is not in the store", taskID, l.Target)
		}
		return *reviewTask
	}
	t.Fatalf("expected a LinkReviewTask on %+v", parent.Links)
	return model.Task{}
}

func TestSyncReviewsFilesAReviewStackedOnTheBranchItReviews(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	tmpl := reviewTemplate(t, ctx, store)
	task := completedTask(t, ctx, store, sim, client, "t1", repo, true, tmpl.ID)

	if err := orchestrator.SyncReviews(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncReviews: %v", err)
	}

	review := reviewTaskOf(t, ctx, store, task.ID)
	if review.Title != tmpl.Title {
		t.Fatalf("review title = %q, want the template's own %q", review.Title, tmpl.Title)
	}
	if !strings.Contains(review.Body, tmpl.Body) {
		t.Fatalf("review body = %q, want it to carry the template's instructions", review.Body)
	}
	// And the subject the template cannot know: which task, which branch,
	// which pull request.
	for _, want := range []string{task.ID, model.BranchName(task.ID), "acme/widgets#"} {
		if !strings.Contains(review.Body, want) {
			t.Fatalf("review body = %q, want it to name %q", review.Body, want)
		}
	}
	if review.Base != model.BranchName(task.ID) {
		t.Fatalf("review base = %q, want the branch under review", review.Base)
	}
	if !review.AutoMerge {
		t.Fatal("a review must carry auto-merge: it merges back into the branch it reviewed")
	}
	if review.Approval == nil {
		t.Fatal("a review must be pre-approved -- attaching one is the standing approval")
	}
	if review.Origin.Reason != model.ReasonReview {
		t.Fatalf("review origin reason = %q, want %q", review.Origin.Reason, model.ReasonReview)
	}
	if review.ReviewTemplateID != "" {
		t.Fatal("a review must not itself carry a review, or reviews would nest forever")
	}
	if review.Target == nil || *review.Target != repo {
		t.Fatalf("review target = %v, want the reviewed task's own repo", review.Target)
	}
	if len(review.Reads) != 1 || review.Reads[0].Name != "docs" {
		t.Fatalf("review reads = %v, want the template's own", review.Reads)
	}
	proposedBy := ""
	for _, l := range review.Links {
		if l.Kind == model.LinkProposedBy {
			proposedBy = l.Target
		}
	}
	if proposedBy != task.ID {
		t.Fatalf("review LinkProposedBy = %q, want %q", proposedBy, task.ID)
	}
	state, err := store.State(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateQueued {
		t.Fatalf("review state = %q, want queued (dispatchable with no approval step)", state)
	}
	if got := commentBodies(t, ctx, store, task.ID); len(got) != 1 || !strings.Contains(got[0], review.ID) {
		t.Fatalf("comments on the reviewed task = %q, want one naming the review task", got)
	}
}

func TestSyncReviewsFilesExactlyOneReviewPerTask(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	tmpl := reviewTemplate(t, ctx, store)
	task := completedTask(t, ctx, store, sim, client, "t1", repo, true, tmpl.ID)

	for i := 0; i < 3; i++ {
		if err := orchestrator.SyncReviews(ctx, store, baseTime.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("SyncReviews: %v", err)
		}
	}

	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	links := 0
	for _, l := range got.Links {
		if l.Kind == model.LinkReviewTask {
			links++
		}
	}
	if links != 1 {
		t.Fatalf("LinkReviewTask count = %d after three cycles, want exactly 1", links)
	}
	if bodies := commentBodies(t, ctx, store, task.ID); len(bodies) != 1 {
		t.Fatalf("comments = %q, want the review announced once", bodies)
	}
}

func TestSyncReviewsLeavesATaskWithNoReviewAttachedAlone(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	reviewTemplate(t, ctx, store)
	task := completedTask(t, ctx, store, sim, client, "t1", repo, true, "")

	if err := orchestrator.SyncReviews(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncReviews: %v", err)
	}

	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range got.Links {
		if l.Kind == model.LinkReviewTask {
			t.Fatal("a task with no review attached must not get one")
		}
	}
}

// The wait is on the declaration, not on the review task having been
// filed, so a cycle whose "sync" runs before its "reviews" cannot merge
// the change out from under a review that was about to be filed.
func TestSyncPullRequestsHoldsTheMergeUntilTheReviewHasLanded(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	tmpl := reviewTemplate(t, ctx, store)
	task := completedTask(t, ctx, store, sim, client, "t1", repo, true, tmpl.ID)
	markMergeable(sim)

	// Owed, not yet filed: clean, at the head of its queue, and not
	// merged.
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}
	if sim.PullRequests[0].Merged {
		t.Fatal("a task waiting on a review it has been promised must not merge")
	}

	// Filed and running: still held.
	if err := orchestrator.SyncReviews(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncReviews: %v", err)
	}
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}
	if sim.PullRequests[0].Merged {
		t.Fatal("a task whose review is still in flight must not merge")
	}
	review := reviewTaskOf(t, ctx, store, task.ID)

	// The review is over -- its own pull request merged back into the
	// branch it reviewed, which is what closes it out.
	closed := baseTime.Add(2 * time.Minute)
	if err := store.Observe(ctx, model.Observation{TaskID: review.ID, ClosedAt: &closed}); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(3*time.Minute)); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}
	if !sim.PullRequests[0].Merged {
		t.Fatal("a reviewed task must merge once its review has landed")
	}
}

// The other side of the same wait: a review that never finishes must not
// hold a finished change (and everything queued behind it) forever.
func TestSyncPullRequestsGivesUpOnAReviewThatNeverFinishes(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	tmpl := reviewTemplate(t, ctx, store)
	task := completedTask(t, ctx, store, sim, client, "t1", repo, true, tmpl.ID)
	markMergeable(sim)

	if err := orchestrator.SyncReviews(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncReviews: %v", err)
	}
	review := reviewTaskOf(t, ctx, store, task.ID)

	late := baseTime.Add(7 * time.Hour)
	if err := orchestrator.SyncPullRequests(ctx, store, client, late); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}
	if sim.PullRequests[0].Merged {
		t.Fatal("the cycle that gives up on the review should say so, not merge in the same breath")
	}
	obs, err := store.GetObservation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.MergeQueueBlockedAt == nil {
		t.Fatal("giving up on a review must mark the task blocked, so the tasks behind it move")
	}
	bodies := commentBodies(t, ctx, store, task.ID)
	if len(bodies) != 2 || !strings.Contains(bodies[1], review.ID) {
		t.Fatalf("comments = %q, want the second one naming the review it gave up on", bodies)
	}

	// Blocked, not abandoned: it still merges the moment it reads clean.
	if err := orchestrator.SyncPullRequests(ctx, store, client, late.Add(time.Minute)); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}
	if !sim.PullRequests[0].Merged {
		t.Fatal("a task the queue gave up waiting on a review for still merges once clean")
	}
}

// A pull request that has already merged is past the point a review can
// hold anything back, so the cycle that closes the task out must not
// also announce that its review is overdue.
func TestSyncPullRequestsClosesOutAMergedTaskWithoutEscalatingItsReview(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	tmpl := reviewTemplate(t, ctx, store)
	task := completedTask(t, ctx, store, sim, client, "t1", repo, true, tmpl.ID)
	if err := orchestrator.SyncReviews(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncReviews: %v", err)
	}

	// Merged by hand, long after the review should have finished.
	for i := range sim.PullRequests {
		sim.PullRequests[i].State = "closed"
		sim.PullRequests[i].Merged = true
	}
	late := baseTime.Add(7 * time.Hour)
	if err := orchestrator.SyncPullRequests(ctx, store, client, late); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}

	obs, err := store.GetObservation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.ClosedAt == nil {
		t.Fatal("a merged pull request must still close its task out")
	}
	if obs.MergeQueueBlockedAt != nil {
		t.Fatal("a task whose pull request has merged must not be escalated over its review")
	}
	if bodies := commentBodies(t, ctx, store, task.ID); len(bodies) != 1 {
		t.Fatalf("comments = %q, want only the one announcing the review", bodies)
	}
}

// A review is not a queue entry in its own right -- it merges into the
// branch it reviewed, not into the repo's base -- so it neither takes the
// head position from an ordinary task nor waits for one.
func TestSyncPullRequestsMergesAReviewBackWithoutQueueingIt(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	tmpl := reviewTemplate(t, ctx, store)
	reviewed := completedTask(t, ctx, store, sim, client, "t1", repo, true, tmpl.ID)
	if err := orchestrator.SyncReviews(ctx, store, baseTime); err != nil {
		t.Fatalf("SyncReviews: %v", err)
	}
	review := reviewTaskOf(t, ctx, store, reviewed.ID)

	// The review's own run pushed and opened a pull request against the
	// branch it reviewed.
	pushBranch(t, sim.BareRepo, model.BranchName(review.ID))
	pr, err := orchestrator.EnsurePullRequest(ctx, store, client, review, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	review.Links = append(review.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: pr.Number}.String(),
	})
	if err := store.PutTask(ctx, review); err != nil {
		t.Fatal(err)
	}
	completed := baseTime
	if err := store.Observe(ctx, model.Observation{TaskID: review.ID, CompletedAt: &completed}); err != nil {
		t.Fatal(err)
	}
	markMergeable(sim)

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}

	merged := map[int]bool{}
	for _, p := range sim.PullRequests {
		merged[p.Number] = p.Merged
	}
	if !merged[pr.Number] {
		t.Fatal("a review's own pull request merges into the branch it reviewed as soon as it is clean")
	}
	if merged[1] {
		t.Fatal("the reviewed task must still be waiting -- its review has not closed yet")
	}
}
