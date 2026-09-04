package orchestrator_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/github/githubsim"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// A conflicted queue head is not handed to a new task on a new branch
// any more: the task itself goes back to working, on the branch its pull
// request is already open from, so the resolution and the change share
// one pull request and one round of CI.
func TestSyncPullRequestsSendsAConflictedQueueHeadBackForRepair(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task, branch := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)

	setMergeable(sim, false)

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}

	// The task is queued again -- dispatchable by the ordinary path, with
	// nobody's approval asked for -- rather than parked as completed
	// while something else repairs it.
	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateQueued {
		t.Fatalf("state = %q, want queued: the task itself is what repairs its branch now", st)
	}

	obs, err := store.GetObservation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.MergeQueueRepairAt == nil {
		t.Fatalf("observation = %+v, want MergeQueueRepairAt set", obs)
	}
	if obs.CompletedAt != nil {
		t.Fatalf("CompletedAt = %v, want cleared: that is what requeues the task", obs.CompletedAt)
	}
	if !obs.RepairInFlight() {
		t.Fatal("the repair does not read as in flight, so nothing will wait for it")
	}
	// Asking for the repair is asking for the attempt: a task sitting on
	// a failure streak (a salvaged run keeps its "failed" outcome
	// forever) would otherwise read 'failed' rather than 'queued' here.
	if obs.RetryRequestedAt == nil {
		t.Fatal("RetryRequestedAt was not set, so a task with a failure streak could not be repaired")
	}

	// No second task anywhere, and no branch but the one the pull request
	// is already open from.
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected the one task and no separate fix task, got %d", len(tasks))
	}

	// The comment is the whole of what the dispatched run is told, so it
	// has to name the pull request, what is wrong with it, and the branch
	// to push to.
	bodies := commentBodies(t, ctx, store, task.ID)
	if len(bodies) != 1 {
		t.Fatalf("expected one comment asking for the repair, got %q", bodies)
	}
	for _, want := range []string{"acme/widgets#", "conflict", branch} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("the repair comment does not mention %q:\n%s", want, bodies[0])
		}
	}

	// Nothing filed a GitHub issue for any of it: this is a store row and
	// a comment, and dispatch.Cycle picks the task up with no label
	// anywhere.
	if len(sim.Issues) != 0 {
		t.Fatalf("expected no GitHub issues at all, got %+v", sim.Issues)
	}
}

// One repair per pull request, and one only: a cycle finding the repair
// already in flight must not ask again, or a head whose agent is mid-run
// would be re-requeued every tick.
func TestSyncPullRequestsDoesNotAskForASecondRepairWhileOneIsInFlight(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task, _ := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)
	setMergeable(sim, false)

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("first SyncPullRequests: %v", err)
	}
	asked := repairAskedAt(t, ctx, store, task.ID)
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("second SyncPullRequests: %v", err)
	}

	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected no task to have been filed at all, got %d", len(tasks))
	}
	if got := commentBodies(t, ctx, store, task.ID); len(got) != 1 {
		t.Fatalf("expected exactly one repair comment, got %q", got)
	}
	if again := repairAskedAt(t, ctx, store, task.ID); !again.Equal(asked) {
		t.Fatalf("MergeQueueRepairAt moved from %v to %v: the repair was asked for twice", asked, again)
	}
}

// The repair ran, the task completed again, and the pull request is still
// conflicted: the automatic attempt did not stick, and the queue asks a
// person rather than dispatching the same repair a second time.
func TestSyncPullRequestsEscalatesWhenTheRepairFinishesButThePrIsStillBroken(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task, _ := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)
	setMergeable(sim, false)

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("asking for the repair: %v", err)
	}
	if !repairInFlightNow(t, ctx, store, task.ID) {
		t.Fatal("expected a repair to have been asked for")
	}

	// The repair run finishes -- pushing something, or nothing -- and the
	// task completes again, standing in for a run that could not actually
	// resolve the conflict. The pull request is conflicted throughout.
	finished := baseTime.Add(time.Hour)
	if err := store.ObserveField(ctx, task.ID, finished, func(o *model.Observation) {
		o.CompletedAt = &finished
	}); err != nil {
		t.Fatal(err)
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, finished); err != nil {
		t.Fatalf("second SyncPullRequests (should escalate): %v", err)
	}

	obs, err := store.GetObservation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.MergeQueueBlockedAt == nil {
		t.Fatal("expected the task to be marked as needing user input")
	}

	if got := commentBodies(t, ctx, store, task.ID); len(got) != 2 {
		t.Fatalf("expected the repair comment plus one escalation comment, got %q", got)
	}

	// No third cycle asks for another repair -- the task keeps the one
	// MergeQueueRepairAt it already has, and the queue has moved on.
	if err := orchestrator.SyncPullRequests(ctx, store, client, finished.Add(time.Minute)); err != nil {
		t.Fatalf("third SyncPullRequests: %v", err)
	}
	if got := commentBodies(t, ctx, store, task.ID); len(got) != 2 {
		t.Fatalf("the queue said something a third time: %q", got)
	}
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected no fix task to have been filed, got %d tasks", len(tasks))
	}
}

// The repair that never finishes. Once one is asked for, its head does
// nothing at all until the task completes again -- and nothing else times
// the wait: the head reads CONFLICTED (not PENDING) for as long as it is
// waiting, so the check-stall deadline never looks at it. A repair that
// never finishes -- a dispatch that never happens, an agent run that
// never comes back -- would hold the head of its repo's queue for the
// life of the deployment, with everything behind it waiting: the same
// stall the check-stall deadline closes, reached from the other side.
//
// orchestrator.RepairDeadline is the bound, measured from the moment the
// repair was asked for. Past it the queue says so and gets on with the
// next task -- while the head it gave up on still merges the moment it
// reads clean.
func TestSyncPullRequestsGivesUpOnAQueueHeadWhoseRepairNeverFinishes(t *testing.T) {
	deadline := orchestrator.RepairDeadline

	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	stuck, stuckBranch := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)
	behind, _ := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t2", repo)

	// The head is conflicted, so the queue sends it back for repair. The
	// task behind it is perfectly mergeable and is held up only by being
	// second.
	setMergeable(sim, true)
	setBranchMergeable(sim, stuckBranch, false)
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests asking for the repair: %v", err)
	}
	if !repairInFlightNow(t, ctx, store, stuck.ID) {
		t.Fatal("expected a repair to have been asked of the conflicted head")
	}
	// Nothing ever dispatches it, and it never completes. That is the
	// whole of the failure: the task simply sits there, while the head
	// goes on reading conflicted every cycle.
	//
	// Well inside the deadline: the queue is still waiting, and has said
	// nothing beyond the comment asking for the repair. This is the same
	// cycle a repair that is simply taking a while gets.
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(deadline/2)); err != nil {
		t.Fatalf("SyncPullRequests inside the deadline: %v", err)
	}
	if bodies := commentBodies(t, ctx, store, stuck.ID); len(bodies) != 1 {
		t.Fatalf("expected only the repair comment inside the deadline, got %q", bodies)
	}
	obs, err := store.GetObservation(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs != nil && obs.MergeQueueBlockedAt != nil {
		t.Fatal("gave up on the queue head while its repair was still inside the deadline")
	}

	// Past it. The queue gives up, says how long it waited, and asks for
	// no second repair.
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(deadline)); err != nil {
		t.Fatalf("SyncPullRequests past the deadline: %v", err)
	}
	obs, err = store.GetObservation(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.MergeQueueBlockedAt == nil {
		t.Fatal("the queue never gave up on a head whose repair never finished")
	}
	bodies := commentBodies(t, ctx, store, stuck.ID)
	if len(bodies) != 2 {
		t.Fatalf("expected the repair comment plus one escalation, got %q", bodies)
	}
	if !strings.Contains(bodies[1], "never finished") {
		t.Errorf("the escalation does not say the repair never finished:\n%s", bodies[1])
	}
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected the two queued tasks and no fix task at all, got %d", len(tasks))
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

	// Giving up is not abandoning. A person resolves the conflict by
	// hand, and the pull request the queue stopped driving merges on its
	// own -- no second escalation, no further repair, no human step
	// beyond the push. The unfinished repair does not hold that merge
	// back: the queue has already given up on it.
	setBranchMergeable(sim, stuckBranch, true)
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime.Add(deadline+2*time.Minute)); err != nil {
		t.Fatalf("SyncPullRequests once the conflict was resolved: %v", err)
	}
	st, err = store.State(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed: a blocked task still merges the moment it reads clean", st)
	}
	if bodies := commentBodies(t, ctx, store, stuck.ID); len(bodies) != 2 {
		t.Fatalf("expected the queue to say this once, got %q", bodies)
	}
}

// TestSyncPullRequestsOnlyActsOnTheQueueHeadNotLaterEntries pins the
// property that makes this a queue at all: only the task in front of its
// repo's queue is repaired or merged on a cycle. Which one that is comes
// off the backlog (queueOrder) -- t1 sits ahead of t2 there, and was also
// filed first, which is the same answer for any deployment that has not
// reordered anything.
func TestSyncPullRequestsOnlyActsOnTheQueueHeadNotLaterEntries(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	earlier := baseTime
	later := baseTime.Add(time.Hour)

	head := filedTask(t, ctx, store, "t1", repo)
	head.AutoMerge = true
	head.CreatedAt = &earlier
	head.OrderKey = 100
	if err := store.PutTask(ctx, head); err != nil {
		t.Fatal(err)
	}
	pushBranch(t, sim.BareRepo, model.BranchName(head.ID))
	headPR, err := orchestrator.EnsurePullRequest(ctx, store, client, head, baseTime)
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
	second.OrderKey = 200
	if err := store.PutTask(ctx, second); err != nil {
		t.Fatal(err)
	}
	pushBranch(t, sim.BareRepo, model.BranchName(second.ID))
	secondPR, err := orchestrator.EnsurePullRequest(ctx, store, client, second, baseTime)
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

	if !repairInFlightNow(t, ctx, store, head.ID) {
		t.Fatal("expected the queue head to have been sent back for repair")
	}
	if repairInFlightNow(t, ctx, store, second.ID) {
		t.Fatal("did not expect a repair for the second queue entry while the head is still unresolved")
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

// setBranchMergeable is the same for one branch's pull request alone, so
// a test can hold a queue head conflicted while everything behind it
// reads clean.
func setBranchMergeable(sim *githubsim.Sim, branch string, mergeable bool) {
	for i := range sim.PullRequests {
		if sim.PullRequests[i].Head == branch {
			sim.PullRequests[i].Mergeable = &mergeable
		}
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
	// a broken one, so nothing is asked of anyone and nobody is told
	// anything.
	if repairInFlightNow(t, ctx, store, task.ID) {
		t.Fatal("a repair was asked for a pull request whose checks had not finished")
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
	if repairInFlightNow(t, ctx, store, task.ID) {
		t.Fatal("a repair was asked for before the rest of the checks had reported")
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
	if !repairInFlightNow(t, ctx, store, task.ID) {
		t.Fatal("expected the failing queue head to have been sent back for repair")
	}
	bodies := commentBodies(t, ctx, store, task.ID)
	if len(bodies) != 1 {
		t.Fatalf("expected one comment asking for the repair, got %q", bodies)
	}
	for _, want := range []string{"unit tests", "e2e", branch} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("the repair comment does not name %q:\n%s", want, bodies[0])
		}
	}
}

// Naming the failing job is where the queue's own message used to stop,
// and for the agent it dispatches that is barely a starting point: a
// sandbox is not a CI runner, and this deployment's sandboxes reach
// nothing but the git proxy, so an agent told "the `go` job is red"
// cannot go and read what the `go` job said. It has to arrive with the
// task. That is the difference between a repair and a guess, and a repair
// that is a guess costs the queue another cycle.
func TestSyncPullRequestsPutsTheFailingJobsOwnLogIntoTheRepairComment(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task, branch := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)

	setMergeable(sim, true)
	sim.CheckRuns[branch] = []github.CheckRun{
		{Name: "go", Status: "completed", Conclusion: strPtr("failure")},
		{Name: "terraform", Status: "completed", Conclusion: strPtr("success")},
	}
	// Seeded under the branch name, the same key the check runs above
	// use: the Actions endpoints are called with the head sha, which
	// githubsim resolves against its own bare repo.
	sim.WorkflowJobs[branch] = []githubsim.WorkflowJob{
		{Name: "go", Conclusion: "failure", Log: "" +
			"2026-01-02T03:04:05.1234567Z --- FAIL: TestQueueHead (0.01s)\n" +
			"2026-01-02T03:04:05.1234567Z     sync_test.go:42: got 3, want 4\n" +
			"2026-01-02T03:04:05.1234567Z FAIL\tgithub.com/bwsalmon/grain/pkg/orchestrator\n"},
		{Name: "terraform", Conclusion: "success", Log: "terraform: no changes\n"},
	}

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests once CI failed: %v", err)
	}

	if !repairInFlightNow(t, ctx, store, task.ID) {
		t.Fatal("expected the failing queue head to have been sent back for repair")
	}
	bodies := commentBodies(t, ctx, store, task.ID)
	if len(bodies) != 1 {
		t.Fatalf("expected one comment asking for the repair, got %q", bodies)
	}
	for _, want := range []string{
		"--- FAIL: TestQueueHead",
		"sync_test.go:42: got 3, want 4",
	} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("the repair comment does not carry %q from the failing job's log:\n%s", want, bodies[0])
		}
	}
	// Actions stamps every line with the same timestamp. It says nothing
	// about the failure and costs about a quarter of every line.
	if strings.Contains(bodies[0], "2026-01-02T03:04:05") {
		t.Errorf("the repair comment kept Actions' per-line timestamps:\n%s", bodies[0])
	}
	// The job that passed is not evidence of anything, and its log is
	// never fetched: FailedJobLogs filters at every step.
	if strings.Contains(bodies[0], "terraform: no changes") {
		t.Errorf("the repair comment carries a passing job's log:\n%s", bodies[0])
	}
}

// logsUnreadable is a client whose FailedJobLogs is refused -- the shape
// of a deployment whose credential can read checks but not Actions, and
// of a repo whose CI is not Actions at all.
type logsUnreadable struct {
	github.Client
}

func (c logsUnreadable) FailedJobLogs(owner, repo, headSHA string) ([]github.JobLog, error) {
	return nil, errors.New("403 Forbidden")
}

// The log is an annotation on the repair, not the repair. Failing the
// cycle over an unreadable log would cost the queue head the one
// automatic repair it gets, over the part of the comment that is a bonus
// -- so the read is best effort, and its failure leaves exactly the
// message this queue sent before logs were ever fetched.
func TestSyncPullRequestsStillAsksForARepairWhenTheJobLogsCannotBeRead(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task, branch := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)

	setMergeable(sim, true)
	sim.CheckRuns[branch] = []github.CheckRun{
		{Name: "go", Status: "completed", Conclusion: strPtr("failure")},
	}

	if err := orchestrator.SyncPullRequests(ctx, store, logsUnreadable{client}, baseTime); err != nil {
		t.Fatalf("SyncPullRequests with unreadable job logs: %v", err)
	}

	if !repairInFlightNow(t, ctx, store, task.ID) {
		t.Fatal("no repair was asked for when the job logs could not be read")
	}
	bodies := commentBodies(t, ctx, store, task.ID)
	if len(bodies) != 1 {
		t.Fatalf("expected one comment asking for the repair, got %q", bodies)
	}
	if !strings.Contains(bodies[0], "its checks are failing (`go`)") {
		t.Errorf("the repair comment no longer names the failing check:\n%s", bodies[0])
	}
	if strings.Contains(bodies[0], "What CI printed") {
		t.Errorf("the repair comment has an empty log section:\n%s", bodies[0])
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
	if repairInFlightNow(t, ctx, store, task.ID) {
		t.Fatal("a repair was asked for a pull request whose CI had not reported yet")
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
	if repairInFlightNow(t, ctx, store, stuck.ID) {
		t.Error("asked for an automatic repair of a pull request nothing had said was broken")
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

// Reading clean is a verdict about one commit, and the merge that acts on
// it is a second request: a push landing in between -- a human's own "push
// a fix by hand", a fix task merging into this branch, a redispatched task
// pushing again -- moves the branch after the verdict and before the
// merge. Merging then lands a commit whose CI this cycle never read, which
// is the one thing waiting for CI exists to prevent, so the merge names
// the commit it was passed and GitHub refuses it if the branch has moved.
//
// Refusing is cheap: the task keeps its place at the head of the queue and
// the next cycle judges whatever is there now on its own checks.
func TestSyncPullRequestsRefusesToMergeACommitThatLandedAfterTheVerdict(t *testing.T) {
	store, ctx := openStore(t)
	sim, rest := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task, branch := queuedTaskWithPullRequest(t, ctx, store, sim, rest, "t1", repo)

	setMergeable(sim, true)
	sim.CheckRuns[branch] = []github.CheckRun{
		{Name: "tests", Status: "completed", Conclusion: strPtr("success")},
	}

	client := &pushBeforeMerge{Client: rest, t: t, bare: sim.BareRepo, branch: branch}
	err := orchestrator.SyncPullRequests(ctx, store, client, baseTime)
	if err == nil {
		t.Fatal("the merge of a moved head was not refused")
	}
	var ghErr *github.Error
	if !errors.As(err, &ghErr) || ghErr.Status != 409 {
		t.Fatalf("SyncPullRequests: %v, want a 409 from the pinned merge", err)
	}
	if !client.pushed {
		t.Fatal("the test never landed its racing commit")
	}
	for _, pr := range sim.PullRequests {
		if pr.Merged {
			t.Fatal("merged a commit that landed after the health read")
		}
	}
	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state = %q, want still completed and still queued", st)
	}

	// Next cycle, with nothing else landing: the commit that arrived is
	// read, judged on the checks that are there for it, and merged.
	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests on the cycle after the race: %v", err)
	}
	st, err = store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state = %q, want closed once the new commit was judged and merged", st)
	}
}

// pushBeforeMerge is a real client with one instant wedged into it: a
// commit lands on the head branch just as the queue asks for the merge,
// which is the window between syncEntry's health read and its merge
// request. Once, so the cycle after it is an ordinary one.
type pushBeforeMerge struct {
	github.Client
	t      *testing.T
	bare   string
	branch string
	pushed bool
}

func (c *pushBeforeMerge) MergePullRequest(owner, repo string, number int, headSHA string) error {
	if !c.pushed {
		c.pushed = true
		pushAnotherCommit(c.t, c.bare, c.branch)
	}
	return c.Client.MergePullRequest(owner, repo, number, headSHA)
}

// backlogIDs is the backlog as a person reading a task list sees it:
// every task in Store.ListTasks' own order, which is also the order
// Store.Ready dispatches in.
func backlogIDs(t *testing.T, ctx context.Context, store *model.Store) []string {
	t.Helper()
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	ids := make([]string, len(tasks))
	for i, tk := range tasks {
		ids[i] = tk.ID
	}
	return ids
}

// placeInBacklog moves an already-filed task to an explicit position, so
// a test can state the backlog it starts from rather than rely on
// whatever OrderKey its helpers left behind.
func placeInBacklog(t *testing.T, ctx context.Context, store *model.Store, task model.Task, key float64) model.Task {
	t.Helper()
	task.OrderKey = key
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	return task
}

// TestSyncPullRequestsMovesTheQueueToTheFrontOfTheBacklog is the merge
// queue's own ordering made visible. The tasks whose pull requests are
// waiting to land go to the front of the backlog in the order the queue
// will act on them -- so a task list answers "what is grain about to
// finish, and in what order" without anyone opening a task, and answers
// it with the same order Store.Ready dispatches from. That is also what
// dispatches a repair promptly, now that the repair is the head task
// itself going back to work rather than a separate row filed in front of
// it.
func TestSyncPullRequestsMovesTheQueueToTheFrontOfTheBacklog(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	// An ordinary task nobody has run yet, sitting ahead of both queue
	// members to begin with, so the move is visible as a move.
	ordinary := placeInBacklog(t, ctx, store, filedTask(t, ctx, store, "t0", repo), 100)
	head, _ := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)
	head = placeInBacklog(t, ctx, store, head, 200)
	second, _ := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t2", repo)
	second = placeInBacklog(t, ctx, store, second, 300)

	// Both pull requests are conflicted: the head is sent back for repair
	// and neither of them merges, so the whole queue is still there to be
	// looked at afterwards.
	setMergeable(sim, false)

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}
	if !repairInFlightNow(t, ctx, store, head.ID) {
		t.Fatal("expected the queue head to have been sent back for repair")
	}

	want := []string{head.ID, second.ID, ordinary.ID}
	if backlog := backlogIDs(t, ctx, store); !reflect.DeepEqual(backlog, want) {
		t.Fatalf("backlog = %v, want %v -- the queue in order, then ordinary work", backlog, want)
	}
}

// TestSyncPullRequestsTakesItsQueueHeadFromTheBacklogOrder is the other
// direction of the same fact: because the queue's order is the backlog's,
// a human who drags one waiting pull request above another really has
// changed which one merges first. Nothing about the tasks themselves
// differs here -- t1 was filed first and is still the older of the two --
// so a queue ordered by anything but position would go on repairing t1.
func TestSyncPullRequestsTakesItsQueueHeadFromTheBacklogOrder(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	first, _ := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t1", repo)
	first = placeInBacklog(t, ctx, store, first, 100)
	second, _ := queuedTaskWithPullRequest(t, ctx, store, sim, client, "t2", repo)
	second = placeInBacklog(t, ctx, store, second, 200)

	// The drag: t2 dropped at the head of the list, above t1.
	if err := store.Reorder(ctx, []string{second.ID}, nil, strPtr(first.ID)); err != nil {
		t.Fatal(err)
	}
	setMergeable(sim, false)

	if err := orchestrator.SyncPullRequests(ctx, store, client, baseTime); err != nil {
		t.Fatalf("SyncPullRequests: %v", err)
	}

	if !repairInFlightNow(t, ctx, store, second.ID) {
		t.Fatal("expected the repair asked of the task dragged to the front of the backlog")
	}
	if repairInFlightNow(t, ctx, store, first.ID) {
		t.Fatal("did not expect a repair for the task now behind it in the backlog")
	}
}

func strPtr(s string) *string { return &s }

// repairAskedAt is when the merge queue asked this task to repair its own
// branch (model.Observation.MergeQueueRepairAt), and fails the test if it
// never did.
func repairAskedAt(t *testing.T, ctx context.Context, store *model.Store, taskID string) time.Time {
	t.Helper()
	obs, err := store.GetObservation(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil || obs.MergeQueueRepairAt == nil {
		t.Fatalf("no repair was asked of %s", taskID)
	}
	return *obs.MergeQueueRepairAt
}

// repairInFlightNow is model.Observation.RepairInFlight read fresh from
// the store -- "has the queue sent this task back to repair its own
// branch, and has that repair not finished yet", which is what most of
// these tests assert instead of the fix-task link the queue used to
// write.
func repairInFlightNow(t *testing.T, ctx context.Context, store *model.Store, taskID string) bool {
	t.Helper()
	obs, err := store.GetObservation(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return obs.RepairInFlight()
}
