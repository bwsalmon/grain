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
	run(t, dir, "git", "init", "--bare", "-q", bare)

	seed := filepath.Join(dir, "seed")
	run(t, dir, "git", "clone", "-q", bare, seed)
	run(t, seed, "git", "config", "user.email", "seed@example.com")
	run(t, seed, "git", "config", "user.name", "seed")
	run(t, seed, "git", "commit", "-q", "--allow-empty", "-m", "seed")
	run(t, seed, "git", "branch", "-M", branch)
	run(t, seed, "git", "push", "-q", "origin", branch)
	return bare
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
	sim, client := newSim(t, "main")
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
	// MergePullRequest is real GitHub API surface Sim deliberately doesn't
	// implement (nothing in the live-test path this package exists for
	// ever merges a PR) -- a test that exercised it in error should fail
	// loudly, the same as v1's own RealGitHubMock raising AssertionError.
	client.MergePullRequest("acme", "widgets", 1)
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
