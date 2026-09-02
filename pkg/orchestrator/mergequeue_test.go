package orchestrator_test

import (
	"testing"
	"time"

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
