package githubsim

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/github"
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
// deliberately duplicated -- see v2/e2e/harness_test.go's own comment on
// why) from pkg/orchestrator's own helper of the same name, since a Sim
// test that merges a pull request now needs the head branch to actually
// exist for mergeIntoBase to merge.
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
	// Closing a PR without merging it (PATCH .../pulls/{number}, state:
	// closed) is real GitHub API surface neither Sim nor github.Client
	// implements yet -- nothing in this project declines a PR today, only
	// merges one (MergePullRequest) -- so this is called directly against
	// Sim rather than through client, which has no method for it. A test
	// exercising an endpoint this double doesn't yet answer for should
	// fail loudly, the same as v1's own RealGitHubMock raising
	// AssertionError.
	sim.Request("PATCH", "/repos/acme/widgets/pulls/1", nil, []byte(`{"state":"closed"}`))
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
