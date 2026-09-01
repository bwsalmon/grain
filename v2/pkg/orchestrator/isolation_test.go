// Failure isolation between a cycle's reconcilers, and between the items
// each one works through.
//
// The property every test here pins down is the same one: work that
// could have been done was done, even though something else in the same
// cycle failed. Before Reconcilers() existed, RunCycle was a pipeline --
// the first error returned, and everything after it simply did not
// happen -- so one failure stalled a merge that was ready and a queue
// that needed advancing. Each test below fails one specific thing and
// asserts that the unrelated work still landed.
package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/github/githubsim"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

var errInjected = errors.New("injected failure")

// failingClient wraps a real github.Client and fails exactly the calls a
// test names, delegating everything else. Embedding the interface rather
// than implementing all 20 methods keeps each test's injection to the one
// method it cares about -- and means a method added to github.Client
// later does not need touching here.
type failingClient struct {
	github.Client
	getPRFor int // GetPullRequest fails for this PR number
}

func (c failingClient) GetPullRequest(owner, repo string, number int) (github.PullRequestDetail, error) {
	if c.getPRFor != 0 && number == c.getPRFor {
		return github.PullRequestDetail{}, errInjected
	}
	return c.Client.GetPullRequest(owner, repo, number)
}

// failingSandboxes fails ToolsFor for one slot, so exactly one of a
// cycle's dispatches cannot run while the others can.
type failingSandboxes struct {
	inner orchestrator.Sandboxes
	slot  string
}

func (s failingSandboxes) ToolsFor(ctx context.Context, slot string) ([]mcp.Tool, error) {
	if slot == s.slot {
		return nil, errInjected
	}
	return s.inner.ToolsFor(ctx, slot)
}

// stubFramework is agent.Framework's one method returning a fixed result,
// with no model, MCP server or sandbox behind it -- enough for a test
// about which dispatches ran, which is all the ones here ask.
type stubFramework struct{ result *agent.Result }

func (f stubFramework) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	return f.result, nil
}

// completesWithAComment is a run that ends by commenting rather than
// pushing: ProcessResult observes CompletedAt for it (see finish.go)
// without any branch having to exist, so a test can tell a dispatch that
// ran from one that did not.
func completesWithAComment() func() agent.Framework {
	result := toolResult(agent.ToolCall{
		Name:      "comment_on_issue",
		Arguments: map[string]any{"comment": "done"},
	})
	return func() agent.Framework { return stubFramework{result: result} }
}

// mergedPullRequestTask files a task whose run has finished and whose
// pull request GitHub has since merged out of band -- the exact state
// SyncPullRequests exists to notice, so a test can assert on whether it
// got the chance to.
func mergedPullRequestTask(t *testing.T, ctx context.Context, store *model.Store, sim *githubsim.Sim,
	client github.Client, id string, repo model.RepoRef, merge bool) model.Task {

	t.Helper()
	task := filedTask(t, ctx, store, id, repo)
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	pr, err := orchestrator.EnsurePullRequest(client, task)
	if err != nil {
		t.Fatalf("opening a pull request for %s: %v", id, err)
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
	if merge {
		if err := client.MergePullRequest(repo.Owner, repo.Name, pr.Number); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

func stateOf(t *testing.T, ctx context.Context, store *model.Store, id string) model.State {
	t.Helper()
	st, err := store.State(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A cycle whose intake cannot reach GitHub still closes out the pull
// requests it already knows about. This is the headline regression: poll
// runs first, and used to take the whole cycle down with it.
// A cycle whose dispatch cannot build a sandbox still closes out the
// pull requests it already knows about. Dispatch runs first, and used to
// take the whole cycle down with it.
func TestRunCycleSyncsPullRequestsEvenWhenDispatchFails(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	// One task with a merged PR waiting to be closed out, and one queued
	// task whose dispatch will fail.
	done := mergedPullRequestTask(t, ctx, store, sim, client, "t1", repo, true)
	filedTask(t, ctx, store, "t2", repo)

	deps := orchestrator.Deps{
		Store:  store,
		Client: client,
		Sandboxes: failingSandboxes{
			inner: orchestrator.NewHostSandboxes(t.TempDir()),
			slot:  "bad",
		},
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	err := orchestrator.RunCycle(ctx, deps, baseTime)
	if !errors.Is(err, errInjected) {
		t.Fatalf("RunCycle error = %v, want it to carry the injected dispatch failure", err)
	}
	if st := stateOf(t, ctx, store, done.ID); st != model.StateClosed {
		t.Fatalf("state = %q, want closed: sync should have run despite dispatch failing", st)
	}
}

// And the other way round: a cycle whose pull-request sync cannot reach
// GitHub still dispatches the work that was ready.
func TestRunCycleDispatchesEvenWhenPullRequestSyncFails(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	watched := mergedPullRequestTask(t, ctx, store, sim, client, "t1", repo, false)
	queued := filedTask(t, ctx, store, "t2", repo)

	deps := orchestrator.Deps{
		Store:         store,
		Client:        failingClient{Client: client, getPRFor: pullRequestNumber(t, watched)},
		Sandboxes:     orchestrator.NewHostSandboxes(t.TempDir()),
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	err := orchestrator.RunCycle(ctx, deps, baseTime)
	if !errors.Is(err, errInjected) {
		t.Fatalf("RunCycle error = %v, want it to carry the injected sync failure", err)
	}
	if st := stateOf(t, ctx, store, queued.ID); st != model.StateCompleted {
		t.Fatalf("state = %q, want completed: dispatch should have run despite sync failing", st)
	}
}

// pullRequestNumber reads the PR a task's own LinkFixes link names.
func pullRequestNumber(t *testing.T, task model.Task) int {
	t.Helper()
	for _, l := range task.Links {
		if l.Kind == model.LinkFixes {
			ref, err := model.ParsePullRequestRef(l.Target)
			if err != nil {
				t.Fatal(err)
			}
			return ref.Number
		}
	}
	t.Fatal("task has no pull request link")
	return 0
}

// Every reconciler's failure reaches the caller, not just the first --
// graind logs one line per cycle, so a failure hidden behind another is a
// failure nobody sees.
func TestRunCycleReportsEveryFailingReconcilerNotJustTheFirst(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	watched := mergedPullRequestTask(t, ctx, store, sim, client, "t1", repo, false)
	filedTask(t, ctx, store, "t2", repo)

	deps := orchestrator.Deps{
		Store:  store,
		Client: failingClient{Client: client, getPRFor: pullRequestNumber(t, watched)},
		Sandboxes: failingSandboxes{
			inner: orchestrator.NewHostSandboxes(t.TempDir()),
			slot:  "bad",
		},
		Framework:     completesWithAComment(),
		MaxConcurrent: 1,
	}

	err := orchestrator.RunCycle(ctx, deps, baseTime)
	if err == nil {
		t.Fatal("RunCycle returned nil, want both failures")
	}
	// Both reconcilers failed for the same injected reason, so the join is
	// what carries them; naming each one is what tells them apart.
	msg := err.Error()
	for _, want := range []string{"dispatch:", "sync:"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("RunCycle error = %q, want it to name the %q reconciler", msg, want)
		}
	}
}

// involved -- this is purely about the per-entry loop.
func TestSyncPullRequestsClosesOutEveryTaskWhenOneFails(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	first := mergedPullRequestTask(t, ctx, store, sim, client, "t1", repo, true)
	second := mergedPullRequestTask(t, ctx, store, sim, client, "t2", repo, true)

	firstPR := pullRequestNumber(t, first)

	err := orchestrator.SyncPullRequests(ctx, store,
		failingClient{Client: client, getPRFor: firstPR}, baseTime)
	if !errors.Is(err, errInjected) {
		t.Fatalf("SyncPullRequests error = %v, want the injected read failure", err)
	}

	if st := stateOf(t, ctx, store, second.ID); st != model.StateClosed {
		t.Fatalf("second task state = %q, want closed: one unreadable PR should not strand the others", st)
	}
	if st := stateOf(t, ctx, store, first.ID); st != model.StateCompleted {
		t.Fatalf("first task state = %q, want it left completed for the next tick to retry", st)
	}
}

// A slot whose sandbox cannot be built does not cost the other slots
// their dispatch: dispatch.Cycle has already durably recorded a run for
// each, so skipping the rest would idle them for a tick over a failure
// that is not theirs.
func TestRunCycleRunsEveryDispatchWhenOneSlotFails(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	first := filedTask(t, ctx, store, "t1", repo)
	second := filedTask(t, ctx, store, "t2", repo)

	deps := orchestrator.Deps{
		Store:  store,
		Client: client,
		Sandboxes: failingSandboxes{
			inner: orchestrator.NewHostSandboxes(t.TempDir()),
			slot:  "bad",
		},
		Framework:     completesWithAComment(),
		MaxConcurrent: 2,
	}

	err := orchestrator.RunCycle(ctx, deps, baseTime)
	if !errors.Is(err, errInjected) {
		t.Fatalf("RunCycle error = %v, want the injected sandbox failure", err)
	}

	// Which task lands in which slot is dispatch.Cycle's business, not
	// this test's -- what matters is that the good slot's dispatch ran
	// rather than being abandoned along with the bad one's.
	completed := 0
	for _, id := range []string{first.ID, second.ID} {
		if stateOf(t, ctx, store, id) == model.StateCompleted {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("completed %d of 2 dispatches, want exactly 1: the good slot's run should still have happened", completed)
	}
}
