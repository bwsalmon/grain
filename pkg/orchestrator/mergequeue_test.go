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

func TestSyncPullRequestsFilesAnAutomaticFixForAConflictedQueueHead(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.AutoMerge = true
	task.CreatedAt = &baseTime
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	pr, err := orchestrator.EnsurePullRequest(client, task)
	if err != nil {
		t.Fatal(err)
	}
	task.Links = append(task.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: pr.Number}.String(),
	})
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}

	no := false
	for i := range sim.PullRequests {
		sim.PullRequests[i].Mergeable = &no
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}

	// The task itself is untouched -- still completed, PR still open, no
	// approval was asked of anyone.
	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state = %q, want still completed", st)
	}

	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixTaskID, hasFix := "", false
	for _, l := range got.Links {
		if l.Kind == model.LinkFixTask {
			fixTaskID, hasFix = l.Target, true
		}
	}
	if !hasFix {
		t.Fatalf("expected a LinkFixTask on %+v", got.Links)
	}

	fixTask, err := store.GetTask(ctx, fixTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if fixTask == nil {
		t.Fatal("fix task not filed in the store")
	}
	if fixTask.Approval == nil {
		t.Fatal("fix task was not pre-approved -- it should need no human approval")
	}
	if !fixTask.AutoMerge {
		t.Fatal("fix task should carry auto-merge")
	}
	if fixTask.Base != "grain/task-"+task.ID {
		t.Fatalf("fix task base = %q, want the original PR's own branch", fixTask.Base)
	}
	// Named after the task it repairs, not after the pull request that
	// went red -- see fixTaskTitle. This is the fix's pull request title
	// too, since EnsurePullRequest takes Title verbatim.
	if want := "Resolve: " + task.Title; fixTask.Title != want {
		t.Fatalf("fix task title = %q, want %q", fixTask.Title, want)
	}
	// The pull request it is filed for is still identified, in the body.
	if !strings.Contains(fixTask.Body, "acme/widgets#") {
		t.Fatalf("fix task body = %q, want it to name the pull request it repairs", fixTask.Body)
	}
	proposedBy, hasProposedBy := "", false
	for _, l := range fixTask.Links {
		if l.Kind == model.LinkProposedBy {
			proposedBy, hasProposedBy = l.Target, true
		}
	}
	if !hasProposedBy || proposedBy != task.ID {
		t.Fatalf("fix task LinkProposedBy = (%q, %v), want (%q, true): the UI's generated-from reads it", proposedBy, hasProposedBy, task.ID)
	}
	fixState, err := store.State(ctx, fixTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if fixState != model.StateQueued {
		t.Fatalf("fix task state = %q, want queued (ready to dispatch with no approval step)", fixState)
	}

	if got := commentBodies(t, ctx, store, task.ID); len(got) != 1 {
		t.Fatalf("expected one comment announcing the fix, got %q", got)
	}

	// Nothing filed a GitHub issue for the fix: it is a store row, and
	// pre-approved, so dispatch.Cycle picks it up with no label anywhere.
	if len(sim.Issues) != 0 {
		t.Fatalf("expected no GitHub issues at all, got %+v", sim.Issues)
	}
}

func TestSyncPullRequestsDoesNotFileASecondFixWhileOneIsInFlight(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.AutoMerge = true
	task.CreatedAt = &baseTime
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))
	pr, err := orchestrator.EnsurePullRequest(client, task)
	if err != nil {
		t.Fatal(err)
	}
	task.Links = append(task.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: pr.Number}.String(),
	})
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}
	no := false
	for i := range sim.PullRequests {
		sim.PullRequests[i].Mergeable = &no
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("first SyncPullRequests: %v", err)
	}
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("second SyncPullRequests: %v", err)
	}

	// The original task and its one fix -- a second cycle finding the fix
	// already in flight must not file a second one.
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected exactly one fix task filed (two tasks total), got %d", len(tasks))
	}
	if got := commentBodies(t, ctx, store, task.ID); len(got) != 1 {
		t.Fatalf("expected exactly one fix-filed comment, got %q", got)
	}
}

func TestSyncPullRequestsEscalatesWhenTheFixTaskFinishesButThePrIsStillBroken(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.AutoMerge = true
	task.CreatedAt = &baseTime
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))
	pr, err := orchestrator.EnsurePullRequest(client, task)
	if err != nil {
		t.Fatal(err)
	}
	task.Links = append(task.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: pr.Number}.String(),
	})
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}
	no := false
	for i := range sim.PullRequests {
		sim.PullRequests[i].Mergeable = &no
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("filing the fix: %v", err)
	}
	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var fixTaskID string
	for _, l := range got.Links {
		if l.Kind == model.LinkFixTask {
			fixTaskID = l.Target
		}
	}
	if fixTaskID == "" {
		t.Fatal("expected a fix task to have been filed")
	}

	// The fix task's own PR is closed without ever merging -- it ran and
	// gave up, standing in for a run that could not actually resolve the
	// conflict. The original PR is still conflicted throughout.
	if err := store.Observe(ctx, model.Observation{TaskID: fixTaskID, ClosedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("second SyncPullRequests (should escalate): %v", err)
	}

	obs, err := store.GetObservation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.MergeQueueBlockedAt == nil {
		t.Fatal("expected the original task to be marked as needing user input")
	}

	if got := commentBodies(t, ctx, store, task.ID); len(got) != 2 {
		t.Fatalf("expected the fix-filed comment plus one escalation comment, got %q", got)
	}

	// No third SyncPullRequests call refiles another fix -- the task
	// stays linked to exactly the one fix task it already tried.
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("third SyncPullRequests: %v", err)
	}
	got, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixLinks := 0
	for _, l := range got.Links {
		if l.Kind == model.LinkFixTask {
			fixLinks++
		}
	}
	if fixLinks != 1 {
		t.Fatalf("expected exactly one LinkFixTask, got %d: %+v", fixLinks, got.Links)
	}
}

func TestSyncPullRequestsOnlyActsOnTheQueueHeadNotLaterEntries(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	earlier := baseTime
	later := baseTime.Add(time.Hour)

	head := filedTask(t, ctx, store, "t1", repo)
	head.AutoMerge = true
	head.CreatedAt = &earlier
	if err := store.PutTask(ctx, head); err != nil {
		t.Fatal(err)
	}
	pushBranch(t, sim.BareRepo, model.BranchName(head.ID))
	headPR, err := orchestrator.EnsurePullRequest(client, head)
	if err != nil {
		t.Fatal(err)
	}
	head.Links = append(head.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: headPR.Number}.String(),
	})
	if err := store.PutTask(ctx, head); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{TaskID: head.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}

	second := filedTask(t, ctx, store, "t2", repo)
	second.AutoMerge = true
	second.CreatedAt = &later
	if err := store.PutTask(ctx, second); err != nil {
		t.Fatal(err)
	}
	pushBranch(t, sim.BareRepo, model.BranchName(second.ID))
	secondPR, err := orchestrator.EnsurePullRequest(client, second)
	if err != nil {
		t.Fatal(err)
	}
	second.Links = append(second.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: secondPR.Number}.String(),
	})
	if err := store.PutTask(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{TaskID: second.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}

	// Both PRs are conflicted.
	no := false
	for i := range sim.PullRequests {
		sim.PullRequests[i].Mergeable = &no
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}

	gotHead, err := store.GetTask(ctx, head.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fixTaskLinkOf(gotHead); !ok {
		t.Fatal("expected a fix task filed for the queue head")
	}

	gotSecond, err := store.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fixTaskLinkOf(gotSecond); ok {
		t.Fatal("did not expect a fix task for the second queue entry while the head is still unresolved")
	}
}

// queuedTaskWithPullRequest is the four-step setup every merge queue test
// here repeats: file an auto-merge task, push its branch, open its pull
// request, and record it as completed so SyncPullRequests picks it up. It
// returns the task as stored and the pull request's own head branch, which
// is what a test seeds check runs under (Sim.CheckRuns' own doc comment).
func queuedTaskWithPullRequest(t *testing.T, ctx context.Context, store *model.Store,
	sim *githubsim.Sim, client *github.RESTClient, id string, repo model.RepoRef) (model.Task, string) {

	t.Helper()
	task := filedTask(t, ctx, store, id, repo)
	task.AutoMerge = true
	task.CreatedAt = &baseTime
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	branch := model.BranchName(task.ID)
	pushBranch(t, sim.BareRepo, branch)
	pr, err := orchestrator.EnsurePullRequest(client, task)
	if err != nil {
		t.Fatal(err)
	}
	task.Links = append(task.Links, model.Link{
		Kind: model.LinkFixes, Target: model.PullRequestRef{Repo: repo, Number: pr.Number}.String(),
	})
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Observe(ctx, model.Observation{TaskID: task.ID, CompletedAt: &baseTime}); err != nil {
		t.Fatal(err)
	}
	return task, branch
}

// setMergeable is GitHub having finished computing mergeability for every
// pull request the sim knows about.
func setMergeable(sim *githubsim.Sim, mergeable bool) {
	for i := range sim.PullRequests {
		sim.PullRequests[i].Mergeable = &mergeable
	}
}

// A pull request whose tests are still running must not merge, however
// mergeable GitHub says it is: a queue that merges on "no failures
// reported yet" lands changes before CI has said anything about them.
// The task holds its place at the head of the queue and merges on a
// later cycle, once the checks it is waiting for come back green.
func TestSyncPullRequestsWaitsForRunningChecksBeforeMerging(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task, branch := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)

	setMergeable(sim, true)
	sim.CheckRuns[branch] = []github.CheckRun{
		{Name: "lint", Status: "completed", Conclusion: strPtr("success")},
		{Name: "tests", Status: "in_progress"},
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests with CI still running: %v", err)
	}

	for _, pr := range sim.PullRequests {
		if pr.Merged {
			t.Fatal("the pull request was merged while its tests were still running")
		}
	}
	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state = %q, want still completed: nothing has happened yet", st)
	}
	// Waiting is not the same as giving up on: an unfinished check is not
	// a broken one, so nothing is filed and nobody is told anything.
	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fixTaskLinkOf(got); ok {
		t.Fatal("a fix task was filed for a pull request whose checks had not finished")
	}
	if bodies := commentBodies(t, ctx, store, task.ID); len(bodies) != 0 {
		t.Fatalf("expected no comments while simply waiting for CI, got %q", bodies)
	}

	// CI finishes, green -- and the same task, still the head of its
	// queue, merges on the very next cycle.
	sim.CheckRuns[branch] = []github.CheckRun{
		{Name: "lint", Status: "completed", Conclusion: strPtr("success")},
		{Name: "tests", Status: "completed", Conclusion: strPtr("success")},
	}
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests once CI passed: %v", err)
	}
	st, err = store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed once the checks passed and auto-merge landed", st)
	}
}

// The other half of the same rule: once the tests do finish and they are
// red, a failing head is a broken head, and it gets exactly the automatic
// fix task a conflicted one gets -- filed pre-approved, based on the
// branch it repairs, naming the checks that failed so the agent sent to
// repair it knows where to look.
func TestSyncPullRequestsFilesAFixOnceRunningChecksFinishFailing(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task, branch := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)

	setMergeable(sim, true)
	// One job has already gone red, but another is still running: the
	// queue files nothing yet, so the one fix task it is allowed gets
	// CI's whole verdict rather than the first job to report.
	sim.CheckRuns[branch] = []github.CheckRun{
		{Name: "unit tests", Status: "completed", Conclusion: strPtr("failure")},
		{Name: "e2e", Status: "in_progress"},
	}
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests with CI still running: %v", err)
	}
	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fixTaskLinkOf(got); ok {
		t.Fatal("a fix task was filed before the rest of the checks had reported")
	}

	sim.CheckRuns[branch] = []github.CheckRun{
		{Name: "unit tests", Status: "completed", Conclusion: strPtr("failure")},
		{Name: "e2e", Status: "completed", Conclusion: strPtr("failure")},
	}
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests once CI failed: %v", err)
	}

	for _, pr := range sim.PullRequests {
		if pr.Merged {
			t.Fatal("a pull request with failing checks was merged")
		}
	}
	got, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixTaskID, ok := fixTaskLinkOf(got)
	if !ok {
		t.Fatalf("expected a fix task for the failing queue head, links: %+v", got.Links)
	}
	fixTask, err := store.GetTask(ctx, fixTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if fixTask == nil {
		t.Fatal("fix task not filed in the store")
	}
	if fixTask.Approval == nil || !fixTask.AutoMerge || fixTask.Base != branch {
		t.Fatalf("fix task = %+v, want pre-approved, auto-merging, based on %q", fixTask, branch)
	}
	for _, want := range []string{"unit tests", "e2e"} {
		if !strings.Contains(fixTask.Body, want) {
			t.Errorf("fix task body does not name the failing check %q:\n%s", want, fixTask.Body)
		}
	}
	if bodies := commentBodies(t, ctx, store, task.ID); len(bodies) != 1 {
		t.Fatalf("expected one comment announcing the fix, got %q", bodies)
	}
}

// The narrower half of the same race, and the one an empty check list
// hides. GitHub creates a workflow run's check runs asynchronously, after
// it has processed the push, while the pull request exists the instant
// the branch lands -- so a sync landing in that gap sees no checks at
// all, which is also precisely what a repo with no CI looks like. Merging
// on it lands the change before CI has said a word.
//
// The window (orchestrator.SetCheckRegistrationWindow) is what tells the
// two apart, and the only thing that can: this test is the whole shape of
// it, a pull request that does not merge on the cycle it was opened on
// and does merge once its head commit has sat there long enough that CI
// would have shown up by now.
func TestSyncPullRequestsWaitsForCiToRegisterBeforeMergingAFreshPullRequest(t *testing.T) {
	window := 2 * time.Minute
	defer orchestrator.SetCheckRegistrationWindow(window)()

	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task, _ := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)

	// Everything GitHub has answered so far says merge me: mergeability
	// computed and clean, and not one check run reported against the
	// commit. Only time distinguishes that from a green repo with no CI.
	setMergeable(sim, true)

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests on a just-opened pull request: %v", err)
	}
	for _, pr := range sim.PullRequests {
		if pr.Merged {
			t.Fatal("the pull request merged on the cycle it was opened on, before CI could register a check")
		}
	}
	// Waiting, not giving up: an unregistered check is not a broken one.
	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fixTaskLinkOf(got); ok {
		t.Fatal("a fix task was filed for a pull request whose CI had not reported yet")
	}
	if bodies := commentBodies(t, ctx, store, task.ID); len(bodies) != 0 {
		t.Fatalf("expected no comments while simply waiting for CI to register, got %q", bodies)
	}

	// Half a window later it is still waiting, and the entry is still the
	// head of its queue rather than having been passed over.
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(window/2)); err != nil {
		t.Fatalf("SyncPullRequests inside the window: %v", err)
	}
	for _, pr := range sim.PullRequests {
		if pr.Merged {
			t.Fatal("the pull request merged inside the registration window")
		}
	}

	// Nothing ever reported, so there was nothing to report: this repo
	// has no CI, and its pull requests still have to merge.
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(window)); err != nil {
		t.Fatalf("SyncPullRequests once the window had elapsed: %v", err)
	}
	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed: a repo with no CI at all must still merge", st)
	}
}

// The window costs a repo that does have CI nothing. The moment a check
// exists there is a real signal to read, and the answer comes from that
// check rather than from a clock -- so a green one merges without sitting
// out the rest of a window it only ever existed to fill.
func TestSyncPullRequestsMergesOnARegisteredCheckWithoutWaitingOutTheWindow(t *testing.T) {
	window := time.Hour
	defer orchestrator.SetCheckRegistrationWindow(window)()

	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task, branch := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)
	setMergeable(sim, true)

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests before CI registered: %v", err)
	}
	for _, pr := range sim.PullRequests {
		if pr.Merged {
			t.Fatal("merged before CI registered anything")
		}
	}

	// CI registers, runs, and passes -- all well inside the window.
	sim.CheckRuns[branch] = []github.CheckRun{{Name: "tests", Status: "queued"}}
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("SyncPullRequests with CI queued: %v", err)
	}
	for _, pr := range sim.PullRequests {
		if pr.Merged {
			t.Fatal("merged with a queued check")
		}
	}

	sim.CheckRuns[branch] = []github.CheckRun{
		{Name: "tests", Status: "completed", Conclusion: strPtr("success")},
	}
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("SyncPullRequests once CI passed: %v", err)
	}
	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed: a passing check answers on its own, window or no window", st)
	}
}

// Waiting for CI is right up to the point where CI is never going to
// answer. A workflow waiting on an approval nobody gives, a self-hosted
// runner that never picks the job up, a provider that reported "queued"
// and went away: each reads PENDING every cycle forever, and PENDING is
// acted on by neither of the arms that merge or file a fix -- so without
// a deadline the task holds the head of its repo's queue for the life of
// the deployment, with nothing merged, nothing filed, nothing said to
// anyone, and everything queued behind it waiting too.
//
// orchestrator.SetCheckStallDeadline is the bound. Past it the queue says
// so on the task, stops driving it, and gets on with the next one --
// while the stuck pull request still merges the moment its checks do
// finish green, since giving up costs the queue position and the
// automatic fix, not the merge.
func TestSyncPullRequestsGivesUpOnAQueueHeadWhoseChecksNeverFinish(t *testing.T) {
	deadline := 2 * time.Hour
	defer orchestrator.SetCheckStallDeadline(deadline)()

	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	stuck, stuckBranch := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)
	behind, _ := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t2", repo)

	setMergeable(sim, true)
	// The head's own check never leaves "queued". The task behind it has
	// nothing to wait for at all and is only kept from merging by being
	// second in the queue.
	sim.CheckRuns[stuckBranch] = []github.CheckRun{{Name: "deploy approval", Status: "queued"}}

	// Well inside the deadline: still waiting, still nothing said. This is
	// the same cycle a slow-but-honest CI run gets.
	for _, at := range []time.Time{baseTime, baseTime.Add(deadline / 2)} {
		if err := orchestrator.SyncPullRequests(ctx, store, client, at); err != nil {
			t.Fatalf("SyncPullRequests inside the deadline: %v", err)
		}
	}
	for _, pr := range sim.PullRequests {
		if pr.Merged {
			t.Fatal("something merged while the queue head's checks were still unfinished")
		}
	}
	if bodies := commentBodies(t, ctx, store, stuck.ID); len(bodies) != 0 {
		t.Fatalf("expected no comments while inside the deadline, got %q", bodies)
	}
	obs, err := store.GetObservation(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs != nil && obs.MergeQueueBlockedAt != nil {
		t.Fatal("gave up on the queue head inside the deadline")
	}

	// Past it. The queue says what it was waiting on and marks the task
	// as needing a person -- but files no fix task: nothing has failed,
	// and there may be nothing in the pull request to fix at all.
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(deadline)); err != nil {
		t.Fatalf("SyncPullRequests past the deadline: %v", err)
	}
	obs, err = store.GetObservation(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.MergeQueueBlockedAt == nil {
		t.Fatal("the queue never gave up on a head whose checks never finished")
	}
	got, err := store.GetTask(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fixTaskLinkOf(got); ok {
		t.Error("filed an automatic fix for a pull request nothing had said was broken")
	}
	bodies := commentBodies(t, ctx, store, stuck.ID)
	if len(bodies) != 1 {
		t.Fatalf("expected exactly one comment explaining the wait, got %q", bodies)
	}
	if !strings.Contains(bodies[0], "deploy approval") {
		t.Errorf("the comment does not name the check that never finished:\n%s", bodies[0])
	}

	// And the queue has moved on: the task behind the stuck one is head
	// now, and merges.
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(deadline+time.Minute)); err != nil {
		t.Fatalf("SyncPullRequests after the queue moved on: %v", err)
	}
	st, err := store.State(ctx, behind.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state of the task behind the stuck one = %q, want closed: the queue never moved on", st)
	}

	// Giving up is not abandoning. The checks finally report, green, and
	// the pull request the queue stopped driving still merges -- no
	// second comment, no fix task, no human step in between.
	sim.CheckRuns[stuckBranch] = []github.CheckRun{
		{Name: "deploy approval", Status: "completed", Conclusion: strPtr("success")},
	}
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(deadline+2*time.Minute)); err != nil {
		t.Fatalf("SyncPullRequests once the stuck checks finished: %v", err)
	}
	st, err = store.State(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed: a blocked task still merges the moment it reads clean", st)
	}
	// One comment for the whole episode, not one per cycle: the task
	// stopped being its repo's queue head the moment it was blocked, so
	// nothing brings it back through the arm that said this.
	if bodies := commentBodies(t, ctx, store, stuck.ID); len(bodies) != 1 {
		t.Fatalf("expected the queue to say this once, got %q", bodies)
	}
}

// The clock runs per pull request, not per queue position. Every entry's
// checks are timed on every cycle, head or not, so a repo whose CI is
// wedged for everything in it clears in one deadline rather than in one
// deadline per queued task -- which is what timing only the head would
// have cost, on exactly the outage that produces the most queued tasks.
func TestSyncPullRequestsTimesStalledChecksBehindTheQueueHeadToo(t *testing.T) {
	deadline := 2 * time.Hour
	defer orchestrator.SetCheckStallDeadline(deadline)()

	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	first, firstBranch := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)
	second, secondBranch := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t2", repo)

	setMergeable(sim, true)
	stuck := []github.CheckRun{{Name: "tests", Status: "in_progress"}}
	sim.CheckRuns[firstBranch] = stuck
	sim.CheckRuns[secondBranch] = stuck

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests on the cycle both went pending: %v", err)
	}
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(deadline)); err != nil {
		t.Fatalf("SyncPullRequests once the deadline elapsed: %v", err)
	}

	obs, err := store.GetObservation(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.MergeQueueBlockedAt == nil {
		t.Fatal("the queue head was not given up on")
	}
	// The second task was never the head, so nothing has been said about
	// it yet -- but its own clock has been running since the same cycle
	// the head's was, so the very next cycle, with no further waiting,
	// gives up on it too.
	obs, err = store.GetObservation(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs != nil && obs.MergeQueueBlockedAt != nil {
		t.Fatal("gave up on a task that was not the queue head")
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(deadline)); err != nil {
		t.Fatalf("SyncPullRequests on the cycle the second task became head: %v", err)
	}
	obs, err = store.GetObservation(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.MergeQueueBlockedAt == nil {
		t.Fatal("the second task waited out a second deadline of its own after being promoted")
	}
}

func strPtr(s string) *string { return &s }

func fixTaskLinkOf(task *model.Task) (string, bool) {
	if task == nil {
		return "", false
	}
	for _, l := range task.Links {
		if l.Kind == model.LinkFixTask {
			return l.Target, true
		}
	}
	return "", false
}
