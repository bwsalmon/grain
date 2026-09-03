package githubsim

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/github"
)

// setupBareRepoWithBranch creates a real bare git repo (the same rig
// gitproxy/live_test.go builds) with one commit on branch -- what
// BranchExists checks against for real, rather than trusting a canned
// answer.
func setupBareRepoWithBranch(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "repo.git")
	// -b branch, not the default-named branch renamed after the fact: a
	// bare repo's own HEAD symref is set once at init and a later push of
	// a differently-named branch does not move it, which used to leave
	// HEAD pointing at a branch that was never pushed at all -- harmless
	// as long as nothing ever cloned this repo for real, which changed
	// once mergeIntoBase started doing exactly that.
	run(t, dir, "git", "init", "--bare", "-q", "-b", branch, bare)

	seed := filepath.Join(dir, "seed")
	run(t, dir, "git", "clone", "-q", bare, seed)
	run(t, seed, "git", "config", "user.email", "seed@example.com")
	run(t, seed, "git", "config", "user.name", "seed")
	run(t, seed, "git", "commit", "-q", "--allow-empty", "-m", "seed")
	run(t, seed, "git", "push", "-q", "origin", branch)
	return bare
}

// pushBranch pushes an empty commit on branch straight to bare, standing
// in for a real dispatch's own git push -- restated here (package-private,
// deliberately duplicated -- see tests/e2e/harness_test.go's own
// comment on why) from pkg/orchestrator's own helper of the same name,
// since a Sim test that merges a pull request now needs the head branch
// to actually exist for mergeIntoBase to merge.
func pushBranch(t *testing.T, bare, branch string) {
	t.Helper()
	dir := t.TempDir()
	wd := filepath.Join(dir, "work")
	run(t, dir, "git", "clone", "-q", bare, wd)
	run(t, wd, "git", "config", "user.email", "agent@example.com")
	run(t, wd, "git", "config", "user.name", "agent")
	run(t, wd, "git", "checkout", "-q", "-b", branch)
	run(t, wd, "git", "commit", "-q", "--allow-empty", "-m", "agent commit")
	run(t, wd, "git", "push", "-q", "origin", branch)
}

// pushAnotherCommit lands one more commit on a branch that already
// exists -- pushBranch above creates one, this one moves it, which is
// what a push arriving while grain is mid-cycle does.
func pushAnotherCommit(t *testing.T, bare, branch string) {
	t.Helper()
	dir := t.TempDir()
	wd := filepath.Join(dir, "work")
	run(t, dir, "git", "clone", "-q", "--branch", branch, bare, wd)
	run(t, wd, "git", "config", "user.email", "agent@example.com")
	run(t, wd, "git", "config", "user.name", "agent")
	run(t, wd, "git", "commit", "-q", "--allow-empty", "-m", "a later push")
	run(t, wd, "git", "push", "-q", "origin", branch)
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// newSim wires a Sim in behind a real github.RESTClient -- the exact seam
// a live end-to-end test uses, and what proves RESTClient's own logic
// (path building, status handling, JSON field extraction) runs unmodified
// against Sim, the same as it would against github.FakeTransport or the
// real network.
func newSim(t *testing.T, branch string) (*Sim, *github.RESTClient) {
	bare := setupBareRepoWithBranch(t, branch)
	sim := New("acme", "widgets", bare, branch)
	client := github.NewClient(sim, nil)
	return sim, client
}

func TestSimListIssuesFiltersByLabel(t *testing.T) {
	sim, client := newSim(t, "main")
	sim.Issues[1] = &Issue{Title: "needs a fix", Body: "do the thing", Labels: map[string]struct{}{"grain-agent": {}}}
	sim.Issues[2] = &Issue{Title: "unrelated", Body: "nothing to see", Labels: map[string]struct{}{}}

	issues, err := client.ListIssues("acme", "widgets", "grain-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Number != 1 || issues[0].Title != "needs a fix" {
		t.Fatalf("got %+v", issues)
	}
}

func TestSimGetIssueReturns404ForAnUnknownIssue(t *testing.T) {
	_, client := newSim(t, "main")
	if _, err := client.GetIssue("acme", "widgets", 99); err == nil {
		t.Fatal("expected an error for an unseeded issue")
	}
}

func TestSimAddAndRemoveLabelMutateTheSeededIssue(t *testing.T) {
	sim, client := newSim(t, "main")
	sim.Issues[1] = &Issue{Title: "t", Body: "b", Labels: map[string]struct{}{"grain-agent": {}}}

	if err := client.AddLabel("acme", "widgets", 1, "grain-agent-in-progress"); err != nil {
		t.Fatal(err)
	}
	issue, err := client.GetIssue("acme", "widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !issue.HasLabel("grain-agent") || !issue.HasLabel("grain-agent-in-progress") {
		t.Fatalf("got labels %+v", issue.Labels)
	}

	if err := client.RemoveLabel("acme", "widgets", 1, "grain-agent"); err != nil {
		t.Fatal(err)
	}
	issue, err = client.GetIssue("acme", "widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	if issue.HasLabel("grain-agent") || !issue.HasLabel("grain-agent-in-progress") {
		t.Fatalf("got labels %+v", issue.Labels)
	}
}

func TestSimDefaultBranchReadsTheConfiguredDefault(t *testing.T) {
	_, client := newSim(t, "trunk")
	branch, err := client.DefaultBranch("acme", "widgets")
	if err != nil || branch != "trunk" {
		t.Fatalf("branch=%q err=%v", branch, err)
	}
}

func TestSimBranchExistsChecksTheRealBareRepo(t *testing.T) {
	sim, client := newSim(t, "main")

	// A real commit landed on "main" by the setup above -- BranchExists
	// must answer true from the real repo, not from any bookkeeping Sim
	// itself keeps.
	exists, err := client.BranchExists("acme", "widgets", "main")
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}

	exists, err = client.BranchExists("acme", "widgets", "grain/issue-1")
	if err != nil || exists {
		t.Fatalf("expected grain/issue-1 not to exist yet: exists=%v err=%v", exists, err)
	}

	// Push a real branch straight to the bare repo (standing in for an
	// agent's own push through a real git proxy, which githubsim itself
	// doesn't model) and confirm Sim now reports it as real too.
	seed := t.TempDir()
	run(t, seed, "git", "clone", "-q", sim.BareRepo, seed)
	run(t, seed, "git", "config", "user.email", "agent@example.com")
	run(t, seed, "git", "config", "user.name", "agent")
	run(t, seed, "git", "checkout", "-q", "-b", "grain/issue-1")
	run(t, seed, "git", "commit", "-q", "--allow-empty", "-m", "fix")
	run(t, seed, "git", "push", "-q", "origin", "grain/issue-1")

	exists, err = client.BranchExists("acme", "widgets", "grain/issue-1")
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
}

func TestSimGetBranchHeadReadsTheRealCommit(t *testing.T) {
	sim, client := newSim(t, "main")

	head, err := client.GetBranchHead("acme", "widgets", "main")
	if err != nil || head == nil {
		t.Fatalf("head=%+v err=%v", head, err)
	}
	if head.SHA == "" {
		t.Fatal("expected a real sha, got empty")
	}
	if head.Message != "seed" {
		t.Fatalf("got message %q, want %q", head.Message, "seed")
	}

	shaOut, err := exec.Command("git", "--git-dir", sim.BareRepo, "rev-parse", "refs/heads/main").Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(shaOut)); head.SHA != want {
		t.Fatalf("got sha %q, want %q (the real repo's own tip)", head.SHA, want)
	}

	missing, err := client.GetBranchHead("acme", "widgets", "no-such-branch")
	if err != nil || missing != nil {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
}

// CreateBranch and UpdateBranch are release management's own
// (bwsalmon/agents#398) git-database calls; Sim answers both against the
// real bare repo the same way branchExists does, so a test proving the
// releases reconciler cuts and promotes for real needs no mock beneath
// them either.
func TestSimCreateBranchCreatesARealRef(t *testing.T) {
	sim, client := newSim(t, "main")
	head, err := client.GetBranchHead("acme", "widgets", "main")
	if err != nil || head == nil {
		t.Fatalf("head=%+v err=%v", head, err)
	}

	if err := client.CreateBranch("acme", "widgets", "release/3.1-rc1", head.SHA); err != nil {
		t.Fatal(err)
	}
	if !sim.branchExists("release/3.1-rc1") {
		t.Fatal("expected release/3.1-rc1 to exist in the real bare repo")
	}
	newHead, err := client.GetBranchHead("acme", "widgets", "release/3.1-rc1")
	if err != nil || newHead == nil || newHead.SHA != head.SHA {
		t.Fatalf("got %+v, want it pinned at main's own tip %q", newHead, head.SHA)
	}

	// Creating it again is a caller mistake, the same 422 real GitHub
	// answers with.
	if err := client.CreateBranch("acme", "widgets", "release/3.1-rc1", head.SHA); err == nil {
		t.Fatal("expected an error creating an already-existing branch")
	}
}

func TestSimUpdateBranchMovesARealRef(t *testing.T) {
	sim, client := newSim(t, "main")
	pushBranch(t, sim.BareRepo, "rc")
	before, err := client.GetBranchHead("acme", "widgets", "rc")
	if err != nil || before == nil {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	mainHead, err := client.GetBranchHead("acme", "widgets", "main")
	if err != nil || mainHead == nil {
		t.Fatalf("mainHead=%+v err=%v", mainHead, err)
	}
	if before.SHA == mainHead.SHA {
		t.Fatal("test setup: rc and main should not already share a tip")
	}

	if err := client.UpdateBranch("acme", "widgets", "rc", mainHead.SHA, true); err != nil {
		t.Fatal(err)
	}
	after, err := client.GetBranchHead("acme", "widgets", "rc")
	if err != nil || after == nil || after.SHA != mainHead.SHA {
		t.Fatalf("got %+v, want rc moved to main's own tip %q", after, mainHead.SHA)
	}

	if err := client.UpdateBranch("acme", "widgets", "no-such-branch", mainHead.SHA, true); err == nil {
		t.Fatal("expected an error moving a branch that doesn't exist")
	}
}

// pushFileToBranch lands one commit writing path on an existing branch,
// which is how a Sim test gives two branches something to genuinely
// conflict over -- pushBranch's own empty commits never can.
func pushFileToBranch(t *testing.T, bare, branch, path, content string) {
	t.Helper()
	dir := t.TempDir()
	wd := filepath.Join(dir, "work")
	run(t, dir, "git", "clone", "-q", "--branch", branch, bare, wd)
	run(t, wd, "git", "config", "user.email", "agent@example.com")
	run(t, wd, "git", "config", "user.name", "agent")
	if err := os.WriteFile(filepath.Join(wd, path), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, wd, "git", "add", path)
	run(t, wd, "git", "commit", "-q", "-m", "a change to "+path)
	run(t, wd, "git", "push", "-q", "origin", branch)
}

// The merges endpoint the merge queue brings a stale pull request branch
// up to date with (orchestrator.refreshStaleHead). Its three answers are
// what the queue switches on, so Sim answers all three with real git
// rather than bookkeeping: a merge that says it landed has really moved
// the branch, an up-to-date branch is one git says is up to date, and a
// conflict is a merge git really refused.
func TestSimMergeBranchMergesForReal(t *testing.T) {
	sim, client := newSim(t, "main")
	pushBranch(t, sim.BareRepo, "grain/task-1")
	pushAnotherCommit(t, sim.BareRepo, "main")

	// Behind: main has moved since the branch was cut, so the merge lands
	// and the branch really contains main afterwards.
	got, err := client.MergeBranch("acme", "widgets", "grain/task-1", "main", "catch up")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Merged || got.SHA == "" {
		t.Fatalf("got %+v, want a merge commit", got)
	}
	head, err := client.GetBranchHead("acme", "widgets", "grain/task-1")
	if err != nil || head == nil || head.SHA != got.SHA {
		t.Fatalf("branch head = %+v, want the merge commit %q the endpoint reported", head, got.SHA)
	}
	if !strings.Contains(head.Message, "catch up") {
		t.Errorf("merge commit message = %q, want the caller's own", head.Message)
	}

	// Up to date: the very same call again, now that the branch contains
	// main, writes nothing at all.
	again, err := client.MergeBranch("acme", "widgets", "grain/task-1", "main", "catch up")
	if err != nil {
		t.Fatalf("an up-to-date branch is an answer, not a failure: %v", err)
	}
	if again.Merged {
		t.Fatalf("got %+v, want nothing merged into a branch that is already up to date", again)
	}
	after, err := client.GetBranchHead("acme", "widgets", "grain/task-1")
	if err != nil || after == nil || after.SHA != head.SHA {
		t.Fatalf("branch head = %+v, want it left at %q", after, head.SHA)
	}
}

func TestSimMergeBranchRefusesARealConflict(t *testing.T) {
	sim, client := newSim(t, "main")
	pushBranch(t, sim.BareRepo, "grain/task-1")
	pushFileToBranch(t, sim.BareRepo, "grain/task-1", "CONFIG.md", "setting: from-the-task")
	pushFileToBranch(t, sim.BareRepo, "main", "CONFIG.md", "setting: from-main")

	before, err := client.GetBranchHead("acme", "widgets", "grain/task-1")
	if err != nil || before == nil {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	_, err = client.MergeBranch("acme", "widgets", "grain/task-1", "main", "catch up")
	if !github.IsMergeConflict(err) {
		t.Fatalf("got %v, want a conflict", err)
	}
	after, err := client.GetBranchHead("acme", "widgets", "grain/task-1")
	if err != nil || after == nil || after.SHA != before.SHA {
		t.Fatalf("branch head = %+v, want it untouched at %q by a merge that conflicted", after, before.SHA)
	}
}

// A branch in another repository -- a fork pull request's head -- is a
// 404 here, the same as one that is simply gone, and the caller's cue to
// leave that pull request alone rather than to resolve anything.
func TestSimMergeBranchAnswers404ForAMissingBranch(t *testing.T) {
	_, client := newSim(t, "main")
	_, err := client.MergeBranch("acme", "widgets", "no-such-branch", "main", "")
	var ghErr *github.Error
	if !errors.As(err, &ghErr) || ghErr.Status != 404 {
		t.Fatalf("got %v, want a 404", err)
	}
}

func TestSimCreatePullRequestRecordsThePR(t *testing.T) {
	sim, client := newSim(t, "main")

	pr, err := client.CreatePullRequest("acme", "widgets", "grain/issue-1", "main", "grain: fix #1", "please review")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 9000 {
		t.Fatalf("got %+v", pr)
	}
	if len(sim.PullRequests) != 1 {
		t.Fatalf("got %+v", sim.PullRequests)
	}
	recorded := sim.PullRequests[0]
	if recorded.Head != "grain/issue-1" || recorded.Base != "main" || recorded.Title != "grain: fix #1" {
		t.Fatalf("got %+v", recorded)
	}

	// A second PR gets a distinct, incrementing number -- v1's own mock
	// numbers pull requests the same way, and nothing here needs a real
	// GitHub-shaped id since this repo never sees a real GitHub.
	pr2, err := client.CreatePullRequest("acme", "widgets", "grain/issue-2", "main", "grain: fix #2", "")
	if err != nil {
		t.Fatal(err)
	}
	if pr2.Number != 9001 {
		t.Fatalf("got %+v", pr2)
	}
}

// Real GitHub refuses a pull request whose base is not a branch, every
// time, and that refusal is worth having here: a task whose base merged
// and was deleted between being filed and being dispatched could never be
// finished, because this one call was declined on every attempt -- and a
// sim that accepted any base at all is why nothing ever caught it.
// orchestrator.pullRequestBase is what answers it now.
func TestSimRefusesAPullRequestAgainstABaseThatIsNotABranch(t *testing.T) {
	_, client := newSim(t, "main")

	_, err := client.CreatePullRequest("acme", "widgets", "grain/issue-1", "gone", "grain: fix #1", "")
	var ghErr *github.Error
	if !errors.As(err, &ghErr) || ghErr.Status != 422 {
		t.Fatalf("got %v, want a 422 for a base branch that does not exist", err)
	}
	if !strings.Contains(string(ghErr.Body), "base") {
		t.Errorf("body = %q, want GitHub's own complaint about the base", ghErr.Body)
	}
}

func TestSimPanicsOnAnUnhandledRequest(t *testing.T) {
	sim, _ := newSim(t, "main")
	sim.Issues[1] = &Issue{Title: "t", Body: "b", Labels: map[string]struct{}{}}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic for an endpoint Sim doesn't answer")
		}
		if !strings.Contains(r.(string), "unhandled request") {
			t.Fatalf("got panic %v", r)
		}
	}()
	// Asking for a reviewer on a pull request (POST .../pulls/{number}/
	// requested_reviewers) is real GitHub API surface neither Sim nor
	// github.Client implements -- nothing in this project ever requests
	// one -- so this is called directly against Sim rather than through
	// client, which has no method for it. A test exercising an endpoint
	// this double doesn't yet answer for should fail loudly, the same as
	// v1's own RealGitHubMock raising AssertionError.
	sim.Request("POST", "/repos/acme/widgets/pulls/1/requested_reviewers", nil,
		[]byte(`{"reviewers":["someone"]}`))
}

// Closing a pull request without merging it, which is what a human
// closing a task and asking for its pull request to go with it does
// (github.RESTClient.ClosePullRequest). The state moves and nothing else
// does: the branch is still there, at the same commit, and the pull
// request is closed rather than merged -- which is the whole distinction
// the offer to a human rests on.
func TestSimClosesAPullRequestWithoutMergingIt(t *testing.T) {
	sim, client := newSim(t, "main")
	pushBranch(t, sim.BareRepo, "grain/issue-1")
	pr, err := client.CreatePullRequest("acme", "widgets", "grain/issue-1", "main", "t", "")
	if err != nil {
		t.Fatal(err)
	}
	head, err := client.GetBranchHead("acme", "widgets", "grain/issue-1")
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.GetBranchHead("acme", "widgets", "main")
	if err != nil {
		t.Fatal(err)
	}

	if err := client.ClosePullRequest("acme", "widgets", pr.Number); err != nil {
		t.Fatalf("ClosePullRequest: %v", err)
	}

	detail, err := client.GetPullRequest("acme", "widgets", pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "closed" || detail.Merged {
		t.Fatalf("state = %q, merged = %t, want closed and unmerged", detail.State, detail.Merged)
	}
	// Both branches exactly where they were: the work is still on the
	// head branch, and none of it reached the base. That is what makes
	// this reversible with a click, and what the human ticking the box
	// was promised.
	after, err := client.GetBranchHead("acme", "widgets", "grain/issue-1")
	if err != nil || after.SHA != head.SHA {
		t.Fatalf("head branch = %+v (%v), want the commits left exactly where they were", after, err)
	}
	baseAfter, err := client.GetBranchHead("acme", "widgets", "main")
	if err != nil || baseAfter.SHA != base.SHA {
		t.Fatalf("base branch = %+v (%v), want it untouched", baseAfter, err)
	}
}

// A pull request number nothing opened is a 404, not a silent success --
// a caller that closed nothing must be able to tell.
func TestSimRefusesToCloseAPullRequestThatIsNotThere(t *testing.T) {
	_, client := newSim(t, "main")

	err := client.ClosePullRequest("acme", "widgets", 9999)
	var ghErr *github.Error
	if !errors.As(err, &ghErr) || ghErr.Status != 404 {
		t.Fatalf("got %v, want a 404 for a pull request that does not exist", err)
	}
}

func TestSimListReviewCommentsReadsSeededComments(t *testing.T) {
	sim, client := newSim(t, "main")
	pr, err := client.CreatePullRequest("acme", "widgets", "grain/issue-1", "main", "t", "")
	if err != nil {
		t.Fatal(err)
	}

	comments, err := client.ListReviewComments("acme", "widgets", pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 0 {
		t.Fatalf("expected no review comments before seeding, got %+v", comments)
	}

	line := 42
	sim.ReviewComments[pr.Number] = []github.ReviewComment{
		{ID: 1, User: "alice", Body: "nit: rename this", Path: "main.go", Line: &line},
	}
	comments, err = client.ListReviewComments("acme", "widgets", pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].User != "alice" || comments[0].Path != "main.go" ||
		comments[0].Line == nil || *comments[0].Line != 42 {
		t.Fatalf("got %+v", comments)
	}
}

func TestSimCreateReviewRecordsADraftReviewNotVisibleThroughListReviewComments(t *testing.T) {
	sim, client := newSim(t, "main")
	pr, err := client.CreatePullRequest("acme", "widgets", "grain/issue-1", "main", "t", "")
	if err != nil {
		t.Fatal(err)
	}

	id, err := client.CreateReview("acme", "widgets", pr.Number, "looks good overall", []github.NewReviewComment{
		{Path: "main.go", Line: 10, Body: "consider a guard clause here"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("expected a non-zero review id")
	}
	if len(sim.Reviews) != 1 {
		t.Fatalf("got %+v", sim.Reviews)
	}
	recorded := sim.Reviews[0]
	if recorded.Number != pr.Number || recorded.Body != "looks good overall" || len(recorded.Comments) != 1 ||
		recorded.Comments[0].Path != "main.go" || recorded.Comments[0].Line != 10 {
		t.Fatalf("got %+v", recorded)
	}

	// A draft review's own comments aren't visible through the ordinary
	// review-comments endpoint until a human submits it -- see Review's
	// doc comment on why CreateReview never writes to ReviewComments.
	comments, err := client.ListReviewComments("acme", "widgets", pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 0 {
		t.Fatalf("expected a draft review's comments to stay invisible, got %+v", comments)
	}

	// A second review gets a distinct, incrementing id.
	id2, err := client.CreateReview("acme", "widgets", pr.Number, "one more pass", nil)
	if err != nil {
		t.Fatal(err)
	}
	if id2 == id {
		t.Fatalf("expected a distinct review id, got %d twice", id)
	}
}

func TestSimPanicsWhenAskedAboutARepoItIsNotConfiguredFor(t *testing.T) {
	_, client := newSim(t, "main")
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a repo Sim isn't configured for")
		}
	}()
	client.ListIssues("someone-else", "other-repo", "grain-agent")
}

func TestSimRecordsEveryCall(t *testing.T) {
	sim, client := newSim(t, "main")
	sim.Issues[1] = &Issue{Title: "t", Body: "b", Labels: map[string]struct{}{}}

	if _, err := client.GetIssue("acme", "widgets", 1); err != nil {
		t.Fatal(err)
	}
	if len(sim.Calls) != 1 || sim.Calls[0].Method != "GET" || sim.Calls[0].Path != "/repos/acme/widgets/issues/1" {
		t.Fatalf("got %+v", sim.Calls)
	}
}

// pushCommit lands one commit carrying message on branch, creating the
// branch if it is not there yet -- what a run's own git push does, and
// the only way a test can say what a commit message says.
func pushCommit(t *testing.T, bare, branch, message string) {
	t.Helper()
	dir := t.TempDir()
	wd := filepath.Join(dir, "work")
	run(t, dir, "git", "clone", "-q", bare, wd)
	run(t, wd, "git", "config", "user.email", "agent@example.com")
	run(t, wd, "git", "config", "user.name", "agent")
	if err := exec.Command("git", "-C", wd, "checkout", "-q", branch).Run(); err != nil {
		run(t, wd, "git", "checkout", "-q", "-b", branch)
	}
	run(t, wd, "git", "commit", "-q", "--allow-empty", "-m", message)
	run(t, wd, "git", "push", "-q", "origin", branch)
}

// Compare is answered against the real repository, so what comes back is
// the commits an agent really pushed, in git's own order, with the base
// branch's own history left out.
func TestSimCompareCommitsReturnsTheBranchesOwnCommits(t *testing.T) {
	sim, client := newSim(t, "main")
	pushCommit(t, sim.BareRepo, "grain/task-1", "Add the parser\n\nWhy it was needed.")
	pushCommit(t, sim.BareRepo, "grain/task-1", "Fix the vet warning")

	commits, err := client.CompareCommits("acme", "widgets", "main", "grain/task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("got %+v", commits)
	}
	if commits[0].Message != "Add the parser\n\nWhy it was needed." {
		t.Fatalf("expected the oldest commit first, got %+v", commits)
	}
	if commits[1].Message != "Fix the vet warning" {
		t.Fatalf("got %+v", commits[1])
	}
	if commits[0].Merge || commits[1].Merge {
		t.Fatalf("neither commit is a merge, got %+v", commits)
	}
	if commits[0].SHA == "" || commits[0].SHA == commits[1].SHA {
		t.Fatalf("expected two distinct shas, got %+v", commits)
	}
}

func TestSimCompareCommitsIs404ForAMissingRef(t *testing.T) {
	_, client := newSim(t, "main")
	if _, err := client.CompareCommits("acme", "widgets", "main", "grain/never-pushed"); err == nil {
		t.Fatal("expected an error for a branch that does not exist")
	}
}

func TestSimUpdatePullRequestBodyRewritesItInPlace(t *testing.T) {
	sim, client := newSim(t, "main")
	pushBranch(t, sim.BareRepo, "grain/task-1")
	pr, err := client.CreatePullRequest("acme", "widgets", "grain/task-1", "main", "a title", "opened early")
	if err != nil {
		t.Fatal(err)
	}

	if err := client.UpdatePullRequestBody("acme", "widgets", pr.Number, "the finished change"); err != nil {
		t.Fatal(err)
	}
	detail, err := client.GetPullRequest("acme", "widgets", pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Body != "the finished change" {
		t.Fatalf("got %q", detail.Body)
	}
	// The title it was never sent is the title it still has.
	if detail.Title != "a title" {
		t.Fatalf("got %q", detail.Title)
	}
}

func TestSimUpdatePullRequestBodyIs404ForAnUnknownPullRequest(t *testing.T) {
	_, client := newSim(t, "main")
	if err := client.UpdatePullRequestBody("acme", "widgets", 4242, "x"); err == nil {
		t.Fatal("expected an error for a pull request that does not exist")
	}
}
