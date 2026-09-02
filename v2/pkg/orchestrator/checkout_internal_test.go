package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// Internal, not the _test package the rest of this package's tests use:
// prepareCheckout is what RunDispatch calls before an agent's first turn,
// and the point of these tests is the shell script it composes, driven
// through real git against a real bare repo -- there is nothing exported
// to reach it through that would not also be running an agent.

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@localhost",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@localhost")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// seedRemote builds base/owner/name.git as a bare repo with one commit on
// "main", so CloneURL(base, repo) is a path git can actually clone --
// exactly the shape of the URL a real deployment's proxy serves, with a
// directory standing in for the proxy.
func seedRemote(t *testing.T, base string, repo model.RepoRef) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	bare := CloneURL(base, repo)
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(bare), "init", "--quiet", "--bare", "--initial-branch=main", filepath.Base(bare))

	seed := t.TempDir()
	git(t, seed, "clone", "--quiet", bare, "work")
	work := filepath.Join(seed, "work")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "README.md")
	git(t, work, "commit", "--quiet", "-m", "seed")
	git(t, work, "push", "--quiet", "origin", "main")
	return work
}

func TestPrepareCheckoutClonesTheTargetAndCreatesTheBranch(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedRemote(t, remoteBase, repo)

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo}
	dir, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task)
	if err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	if dir != CheckoutDir {
		t.Fatalf("prepareCheckout returned %q, want %q", dir, CheckoutDir)
	}

	work := filepath.Join(root, CheckoutDir)
	if _, err := os.Stat(filepath.Join(work, "README.md")); err != nil {
		t.Fatalf("the seeded file is missing from the checkout: %v", err)
	}
	if got, want := git(t, work, "rev-parse", "--abbrev-ref", "HEAD"), model.BranchName("t1"); got != want {
		t.Fatalf("checked-out branch = %q, want %q -- the agent is told to push that branch", got, want)
	}
}

// A task with a /base directive is rooted there rather than on whatever
// the clone checks out by default: the PR opens against that base
// (finish.go), so building the change anywhere else makes a diff full of
// commits the base already has.
func TestPrepareCheckoutStartsFromTheTasksBase(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seed := seedRemote(t, remoteBase, repo)

	git(t, seed, "checkout", "--quiet", "-b", "release")
	if err := os.WriteFile(filepath.Join(seed, "RELEASE"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "RELEASE")
	git(t, seed, "commit", "--quiet", "-m", "release only")
	git(t, seed, "push", "--quiet", "origin", "release")

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo, Base: "release"}
	if _, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task); err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, CheckoutDir, "RELEASE")); err != nil {
		t.Fatalf("checkout is not rooted at the task's base branch: %v", err)
	}
}

// A redispatch continues the branch its previous attempt pushed rather
// than branching over it: re-rooting on the base would make the next
// push a non-fast-forward of work that is already on the remote.
func TestPrepareCheckoutContinuesAnExistingBranch(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seed := seedRemote(t, remoteBase, repo)
	branch := model.BranchName("t1")

	git(t, seed, "checkout", "--quiet", "-b", branch)
	if err := os.WriteFile(filepath.Join(seed, "ATTEMPT1"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "ATTEMPT1")
	git(t, seed, "commit", "--quiet", "-m", "first attempt")
	git(t, seed, "push", "--quiet", "origin", branch)

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo}
	if _, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task); err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	work := filepath.Join(root, CheckoutDir)
	if _, err := os.Stat(filepath.Join(work, "ATTEMPT1")); err != nil {
		t.Fatalf("the previous attempt's commit is missing from the redispatch's checkout: %v", err)
	}
	if got := git(t, work, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("checked-out branch = %q, want %q", got, branch)
	}
}

// Re-running against a slot that already holds a checkout (HostSandboxes
// directories are long-lived and reused across tasks -- see its own doc
// comment) replaces it rather than failing on "destination path already
// exists", which is what a second task on the same slot used to hit.
func TestPrepareCheckoutReplacesALeftoverCheckout(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedRemote(t, remoteBase, repo)

	root := t.TempDir()
	stale := filepath.Join(root, CheckoutDir)
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "LEFTOVER"), []byte("from the last task\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := model.Task{ID: "t2", Target: &repo}
	if _, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task); err != nil {
		t.Fatalf("prepareCheckout over a leftover checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stale, "LEFTOVER")); !os.IsNotExist(err) {
		t.Fatalf("the previous task's file survived into this task's checkout (err=%v)", err)
	}
}

// No proxy URL configured is not an error: it is every test and any
// deployment running no proxy, and it leaves the sandbox exactly as it
// was before this existed.
func TestPrepareCheckoutSkippedWithoutARemoteBase(t *testing.T) {
	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &model.RepoRef{Owner: "acme", Name: "widgets"}}
	dir, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), "", task)
	if err != nil || dir != "" {
		t.Fatalf("prepareCheckout with no remote base = (%q, %v), want (\"\", nil)", dir, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("sandbox contents = %v (%v), want it untouched", entries, err)
	}
}

// Every ref this composes a command out of is checked first: /repo and
// /base (directives.go) accept any non-space token, so a task body is
// otherwise free to put shell syntax into it.
func TestPrepareCheckoutRefusesAnUnusableRef(t *testing.T) {
	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &model.RepoRef{Owner: "acme", Name: "widgets"}, Base: "x';touch pwned;'"}
	if _, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), t.TempDir(), task); err == nil {
		t.Fatal("prepareCheckout accepted a base branch carrying shell syntax")
	}
	if _, err := os.Stat(filepath.Join(root, "pwned")); !os.IsNotExist(err) {
		t.Fatalf("the refused base ran anyway (err=%v)", err)
	}
}

// A clone that fails is the run's own failure, reported with git's words
// rather than swallowed -- RunDispatch turns this into the run's outcome
// detail, which is what a human reads in the UI.
func TestPrepareCheckoutReportsAFailedClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &model.RepoRef{Owner: "acme", Name: "nope"}}
	_, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), t.TempDir(), task)
	if err == nil {
		t.Fatal("prepareCheckout reported success cloning a repo that does not exist")
	}
	if !strings.Contains(err.Error(), "acme/nope") {
		t.Fatalf("error does not name the repo it failed to clone: %v", err)
	}
}

// A base branch that no longer exists on the remote is the ordinary end
// of a branch's life -- it merged, and GitHub deleted it -- and a task
// can outlive one, since New task prefills Base from the repo's last
// task (bwsalmon/agents#641). git's own answer names neither the base
// nor the repo ("error: pathspec 'x' did not match any file(s) known to
// git"), and reads like a corrupt checkout rather than a branch that is
// simply gone, so prepareCheckout says which branch and where itself.
func TestPrepareCheckoutNamesABaseBranchThatNoLongerExists(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedRemote(t, remoteBase, repo)

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo, Base: "grain/issue-642"}
	_, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task)
	if err == nil {
		t.Fatal("prepareCheckout succeeded against a base branch that does not exist")
	}
	for _, want := range []string{"grain/issue-642", "acme/widgets", "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v; want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "pathspec") {
		t.Errorf("error = %v; want grain's own explanation, not git's raw pathspec message", err)
	}
}
