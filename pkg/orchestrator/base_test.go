package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// A task's Base is the branch its pull request opens against, and a
// branch can be gone by the time the pull request is opened: New task
// prefills Base from the repo's last task (bwsalmon/agents#641), so a
// branch that merges between one task being filed and the next being
// dispatched leaves a task pointed at nothing. GitHub answers a base
// branch that does not exist with a 422 every time, so such a task could
// never be finished at all -- the agent cloned, worked, committed and
// pushed, and the one call that would turn that branch into a pull
// request was refused on every attempt, forever.
//
// These tests are about what happens instead. githubsim refuses an
// unknown base exactly as GitHub does, so a base left unchecked here
// fails these tests the way it failed on the real deployment.

// taskBasedOn files a task whose pull request is meant to open against
// base -- directives.go's `/base`, and every merge queue fix task.
func taskBasedOn(t *testing.T, ctx context.Context, store *model.Store,
	id string, repo model.RepoRef, base string) model.Task {

	t.Helper()
	task := filedTask(t, ctx, store, id, repo)
	task.Base = base
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("filing task with a base: %v", err)
	}
	return task
}

// baseOf is the branch GitHub itself says a pull request is open
// against -- read back off the pull request rather than trusted from the
// call that opened it.
func baseOf(t *testing.T, client github.Client, repo model.RepoRef, number int) string {
	t.Helper()
	detail, err := client.GetPullRequest(repo.Owner, repo.Name, number)
	if err != nil {
		t.Fatalf("reading pull request %d: %v", number, err)
	}
	return detail.BaseRef
}

// The ordinary case, first: a base that is still a branch is the base,
// and nothing is said about it.
func TestAPullRequestOpensAgainstABaseThatIsStillThere(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := taskBasedOn(t, ctx, store, "t1", repo, "release")

	pushBranch(t, sim.BareRepo, "release")
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	pr, err := orchestrator.EnsurePullRequest(ctx, store, client, task, baseTime)
	if err != nil {
		t.Fatalf("EnsurePullRequest: %v", err)
	}
	if got := baseOf(t, client, repo, pr.Number); got != "release" {
		t.Errorf("pull request base = %q, want the task's own %q", got, "release")
	}
	if bodies := commentBodies(t, ctx, store, task.ID); len(bodies) != 0 {
		t.Errorf("comments = %q, want none: nothing happened to this task's base", bodies)
	}
}

// The case this exists for: the base merged and was deleted while the
// task was waiting. The work is already committed and pushed by the time
// anything here runs, so it is opened against the default branch rather
// than refused -- and the retarget is written down, on the task's own row
// and in its conversation, because a base that went missing for some
// other reason (a typo, work that was abandoned) means the pull request
// is aimed at the wrong branch and only a human can tell which it was.
func TestAPullRequestIsRetargetedWhenItsBaseBranchIsGone(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := taskBasedOn(t, ctx, store, "t1", repo, "grain/task-641")
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	pr, err := orchestrator.EnsurePullRequest(ctx, store, client, task, baseTime)
	if err != nil {
		t.Fatalf("EnsurePullRequest against a base that no longer exists: %v", err)
	}
	if got := baseOf(t, client, repo, pr.Number); got != "main" {
		t.Errorf("pull request base = %q, want the repo's default branch", got)
	}

	updated, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Base != "main" {
		t.Errorf("task base = %q, want it retargeted to match the pull request that exists", updated.Base)
	}

	bodies := commentBodies(t, ctx, store, task.ID)
	if len(bodies) != 1 {
		t.Fatalf("comments = %q, want one saying the base was retargeted", bodies)
	}
	for _, want := range []string{"grain/task-641", "main", "acme/widgets"} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("comment = %q; want it to name %q", bodies[0], want)
		}
	}
}

// The whole point of the retarget: a task whose base vanished finishes.
// It used to be un-completable -- left queued after every attempt, and
// offered again after its backoff to push more commits onto the same
// branch and be refused the same way.
func TestAFinishedRunWhoseBaseVanishedStillCompletesItsTask(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := taskBasedOn(t, ctx, store, "t1", repo, "grain/task-641")
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	result := toolResult(agent.ToolCall{Name: "run_command", Text: "pushed"})
	if err := orchestrator.ProcessResult(ctx, store, client, task, result, "run-1", baseTime); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	st, err := store.State(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateAwaitingSubmit {
		t.Fatalf("state = %q, want awaiting_submit: the branch is pushed and its pull request is open, waiting on a Submit click", st)
	}
	if len(sim.PullRequests) != 1 {
		t.Fatalf("pull requests = %+v, want the one this finish opened", sim.PullRequests)
	}
}

// A retried finish -- this one's pull request refused by GitHub for some
// unrelated reason, the next attempt getting through -- says it once. The
// row is what makes that true: the second attempt reads a base that is
// already the default branch, so there is nothing left to retarget.
func TestARetargetedBaseIsSaidOnce(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := taskBasedOn(t, ctx, store, "t1", repo, "grain/task-641")
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	refusing := refusingPullRequestClient{Client: client}
	if _, err := orchestrator.EnsurePullRequest(ctx, store, refusing, task, baseTime); err == nil {
		t.Fatal("expected the refused pull request to be reported")
	}

	// The redispatch reads the task back out of the store, exactly as the
	// cycle does.
	retried, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.EnsurePullRequest(ctx, store, client, *retried, baseTime); err != nil {
		t.Fatalf("EnsurePullRequest on the retry: %v", err)
	}
	if bodies := commentBodies(t, ctx, store, task.ID); len(bodies) != 1 {
		t.Fatalf("comments = %q, want exactly one across both attempts", bodies)
	}
}

// blindClient sees no branch at all and cannot read the repo either --
// the shape of a client aimed at the wrong host, holding a token that
// lost its scope, or looking at a repo that has been made private. Every
// one of those 404s exactly as a branch that is not there does.
type blindClient struct {
	github.Client
}

func (blindClient) BranchExists(owner, repo, branch string) (bool, error) { return false, nil }

func (blindClient) DefaultBranch(owner, repo string) (string, error) {
	return "", errors.New("404 Not Found")
}

// So a client that cannot see the repo must not be read as a base that
// merged: the failure is reported, and the task's own base is left
// exactly as its author set it.
func TestABaseIsNotRetargetedByAClientThatCannotSeeTheRepo(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := taskBasedOn(t, ctx, store, "t1", repo, "release")
	pushBranch(t, sim.BareRepo, model.BranchName(task.ID))

	if _, err := orchestrator.EnsurePullRequest(ctx, store, blindClient{Client: client}, task, baseTime); err == nil {
		t.Fatal("expected a client that cannot read the repo to report that, not to retarget")
	}
	updated, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Base != "release" {
		t.Errorf("task base = %q, want it untouched", updated.Base)
	}
	if bodies := commentBodies(t, ctx, store, task.ID); len(bodies) != 0 {
		t.Errorf("comments = %q, want none", bodies)
	}
}
