package orchestrator

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
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
	prepared, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task, "")
	if err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	if prepared.Dir != CheckoutDir {
		t.Fatalf("prepareCheckout returned %q, want %q", prepared.Dir, CheckoutDir)
	}
	if prepared.Setup != nil {
		t.Fatalf("a repo with no setup command reported one: %+v", prepared.Setup)
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
	if _, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task, ""); err != nil {
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
	if _, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task, ""); err != nil {
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
	if _, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task, ""); err != nil {
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
	prepared, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), "", task, "")
	if err != nil || prepared.Dir != "" {
		t.Fatalf("prepareCheckout with no remote base = (%q, %v), want (\"\", nil)", prepared.Dir, err)
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
	if _, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), t.TempDir(), task, ""); err == nil {
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
	_, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), t.TempDir(), task, "")
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
	_, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task, "")
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

// The other arm of the same check, which used not to be checked at all:
// from the second attempt onward the run's branch is already on the
// remote, so nothing looked at the base again until GitHub refused the
// pull request at the very end. An existing branch is exactly the case
// where a vanished base has already bitten once.
//
// It is not fatal here, though. The base is not used on this arm -- the
// branch's own history is what the attempt continues -- and failing the
// checkout would strand commits that are already pushed, since a run that
// never reaches its agent never has its branch salvaged into a pull
// request either. So the attempt goes ahead, and the diagnosis goes to
// the journal at the start of it rather than surfacing as a 422 nobody
// sees at the end.
func TestPrepareCheckoutContinuesAnExistingBranchWhenTheBaseIsGone(t *testing.T) {
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

	var journal strings.Builder
	log.SetOutput(&journal)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo, Base: "grain/issue-642"}
	if _, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task, ""); err != nil {
		t.Fatalf("prepareCheckout over a branch that already exists: %v", err)
	}
	work := filepath.Join(root, CheckoutDir)
	if _, err := os.Stat(filepath.Join(work, "ATTEMPT1")); err != nil {
		t.Fatalf("the previous attempt's commit is missing from the redispatch's checkout: %v", err)
	}
	if got := git(t, work, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("checked-out branch = %q, want %q", got, branch)
	}
	for _, want := range []string{"grain/issue-642", "acme/widgets", "does not exist", "default branch"} {
		if !strings.Contains(journal.String(), want) {
			t.Errorf("journal = %q; want it to mention %q", journal.String(), want)
		}
	}
}

// And nothing is said about a base that is where it should be -- the
// journal line above is a diagnosis, not a running commentary.
func TestPrepareCheckoutSaysNothingAboutABaseThatIsStillThere(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedRemote(t, remoteBase, repo)

	var journal strings.Builder
	log.SetOutput(&journal)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo, Base: "main"}
	if _, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task, ""); err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	if journal.String() != "" {
		t.Errorf("journal = %q, want silence", journal.String())
	}
}

// The commits an earlier attempt left on the branch are the other half of
// what a redispatch is told about it (History, previousAttemptsSection):
// the store says how each attempt ended, and this says what it actually
// built. Read here, through the same sandbox tool prepareCheckout clones
// with, because this is the only place in a dispatch that has a
// repository rather than a string.
func TestCheckoutCommitsListsWhatEarlierAttemptsPushed(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seed := seedRemote(t, remoteBase, repo)
	branch := model.BranchName("t1")

	git(t, seed, "checkout", "--quiet", "-b", branch)
	for _, subject := range []string{"add the failing test", "bound the CI answer"} {
		if err := os.WriteFile(filepath.Join(seed, strings.ReplaceAll(subject, " ", "-")), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, seed, "add", ".")
		git(t, seed, "commit", "--quiet", "-m", subject)
	}
	git(t, seed, "push", "--quiet", "origin", branch)

	root := t.TempDir()
	tools := mcp.NewSandboxTools(root)
	task := model.Task{ID: "t1", Target: &repo}
	if _, err := prepareCheckout(context.Background(), tools, remoteBase, task, ""); err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}

	commits := checkoutCommits(context.Background(), tools, task, maxBranchCommits+1)
	if len(commits) != 2 {
		t.Fatalf("commits = %v, want the two the earlier attempt pushed", commits)
	}
	// Newest first, git's own order: the last thing an earlier attempt
	// did is the thing its successor most needs to see.
	if !strings.Contains(commits[0], "bound the CI answer") || !strings.Contains(commits[1], "add the failing test") {
		t.Errorf("commits = %v, want them newest first with their subjects", commits)
	}
	// The abbreviated hash rides along, so a run can `git show` one
	// without going looking for it first.
	if fields := strings.Fields(commits[0]); len(fields) < 2 || len(fields[0]) < 7 {
		t.Errorf("commit line %q does not start with an abbreviated hash", commits[0])
	}
	// Nothing of git's own chatter, and nothing of the run_command
	// framing the marker exists to be picked out of.
	for _, c := range commits {
		if strings.Contains(c, "exit=") || strings.Contains(c, commitMarker) {
			t.Errorf("commit line %q carries the tool's own framing", c)
		}
	}
}

// A first attempt's branch has nothing on it that its base does not, and
// says so with no commits rather than with the base's whole history --
// which is what a `git log` with no range would have given it.
func TestCheckoutCommitsIsEmptyOnAFreshBranch(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedRemote(t, remoteBase, repo)

	root := t.TempDir()
	tools := mcp.NewSandboxTools(root)
	task := model.Task{ID: "t1", Target: &repo}
	if _, err := prepareCheckout(context.Background(), tools, remoteBase, task, ""); err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	if commits := checkoutCommits(context.Background(), tools, task, maxBranchCommits+1); len(commits) != 0 {
		t.Errorf("commits on a branch nobody has pushed to = %v, want none", commits)
	}
}

// A base that has since been merged and deleted is the same survivable
// case baseCheck carries on through -- and the attempt's commits are
// still worth describing, so the range falls back to origin/HEAD rather
// than the read failing with the base.
func TestCheckoutCommitsFallsBackWhenTheBaseIsGone(t *testing.T) {
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

	log.SetOutput(&strings.Builder{})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	root := t.TempDir()
	tools := mcp.NewSandboxTools(root)
	task := model.Task{ID: "t1", Target: &repo, Base: "grain/issue-642"}
	if _, err := prepareCheckout(context.Background(), tools, remoteBase, task, ""); err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	commits := checkoutCommits(context.Background(), tools, task, maxBranchCommits+1)
	if len(commits) != 1 || !strings.Contains(commits[0], "first attempt") {
		t.Errorf("commits = %v, want the one commit the earlier attempt pushed", commits)
	}
}

// Every ref this composes a command out of goes through gitSafe first,
// exactly as prepareCheckout's do: /base accepts any non-space token, and
// a task body must not be able to put shell syntax into a command this
// package runs. A refused base is not an error here -- orientation that
// could fail a dispatch would be a worse trade than the re-diagnosis it
// saves -- it just falls back to the base every clone has.
func TestCheckoutCommitsRefusesAnUnusableBase(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedRemote(t, remoteBase, repo)

	root := t.TempDir()
	tools := mcp.NewSandboxTools(root)
	task := model.Task{ID: "t1", Target: &repo}
	if _, err := prepareCheckout(context.Background(), tools, remoteBase, task, ""); err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	task.Base = "x';touch pwned;'"
	if commits := checkoutCommits(context.Background(), tools, task, maxBranchCommits+1); len(commits) != 0 {
		t.Errorf("commits = %v, want none", commits)
	}
	for _, dir := range []string{root, filepath.Join(root, CheckoutDir)} {
		if _, err := os.Stat(filepath.Join(dir, "pwned")); !os.IsNotExist(err) {
			t.Fatalf("the refused base ran anyway in %s (err=%v)", dir, err)
		}
	}
}

// No checkout at all -- a sandbox whose clone never happened, or a
// deployment running no proxy -- is silence, not a failure: the whole
// section this feeds is orientation, and there is nothing to orient
// against.
func TestCheckoutCommitsSaysNothingWithoutACheckout(t *testing.T) {
	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &model.RepoRef{Owner: "acme", Name: "widgets"}}
	if commits := checkoutCommits(context.Background(), mcp.NewSandboxTools(root), task, maxBranchCommits+1); len(commits) != 0 {
		t.Errorf("commits with no checkout = %v, want none", commits)
	}
}

// A repo's setup command runs in the checkout, not beside it: `make
// deps` written by whoever configured the repo assumes it is standing at
// the top of the tree, which is the one thing that has to be true of it
// wherever grain decided to put the clone.
func TestPrepareCheckoutRunsTheReposSetupCommandInTheCheckout(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedRemote(t, remoteBase, repo)

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo}
	prepared, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task,
		"test -f README.md && echo deps installed > DEPS")
	if err != nil {
		t.Fatalf("prepareCheckout: %v", err)
	}
	if prepared.Setup == nil {
		t.Fatal("nothing reported about the setup command that ran")
	}
	if prepared.Setup.failed() {
		t.Errorf("setup reported as failed: %+v", prepared.Setup)
	}
	if got, err := os.ReadFile(filepath.Join(root, CheckoutDir, "DEPS")); err != nil {
		t.Fatalf("the setup command did not run in the checkout: %v", err)
	} else if strings.TrimSpace(string(got)) != "deps installed" {
		t.Errorf("DEPS = %q, want what the setup command wrote", got)
	}
}

// A setup command that fails does not fail the dispatch: the run goes
// ahead, holding an account of what happened, because grain cannot know
// whether a broken `make deps` is fatal to the task in hand and the run
// can find out in one command -- while a run told nothing is a run
// debugging the repo's toolchain from a failure it has no context for.
func TestPrepareCheckoutReportsAFailedSetupRatherThanFailingTheDispatch(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedRemote(t, remoteBase, repo)

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo}
	prepared, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task,
		"echo no such package >&2; exit 3")
	if err != nil {
		t.Fatalf("a failed setup command failed the dispatch: %v", err)
	}
	if prepared.Dir != CheckoutDir {
		t.Errorf("Dir = %q, want the checkout the run still has %q", prepared.Dir, CheckoutDir)
	}
	if prepared.Setup == nil || !prepared.Setup.failed() {
		t.Fatalf("Setup = %+v, want a failure reported", prepared.Setup)
	}
	if prepared.Setup.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want the command's own 3", prepared.Setup.ExitCode)
	}
	if !strings.Contains(prepared.Setup.Output, "no such package") {
		t.Errorf("Output = %q, want what the command printed as it gave up", prepared.Setup.Output)
	}
}

// The bound is the other half of that rule: a setup command that has not
// finished has told nobody anything, and letting it run would spend the
// run's whole wall-clock budget inside one tool call the agent never
// sees. That one fails the dispatch, which leaves a human a run whose
// detail says so.
func TestPrepareCheckoutFailsTheDispatchWhenSetupOutrunsItsBound(t *testing.T) {
	remoteBase := t.TempDir()
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	seedRemote(t, remoteBase, repo)

	was := setupCommandTimeout
	setupCommandTimeout = time.Second
	t.Cleanup(func() { setupCommandTimeout = was })

	root := t.TempDir()
	task := model.Task{ID: "t1", Target: &repo}
	_, err := prepareCheckout(context.Background(), mcp.NewSandboxTools(root), remoteBase, task, "sleep 30")
	if err == nil {
		t.Fatal("a setup command that outran its bound was reported as a prepared checkout")
	}
	for _, want := range []string{"setup command", "no agent was started"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// The tail, not the head: a build that printed for ten minutes says what
// went wrong in its last lines, and the whole log would cost the run more
// context than the failure is worth.
func TestSetupOutputKeepsTheEnd(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	b.WriteString("the actual error")
	got := tailBytes(b.String(), setupOutputBudget)
	if len(got) > setupOutputBudget+len("[grain] earlier output omitted\n") {
		t.Errorf("kept %d bytes, want no more than the budget plus its notice", len(got))
	}
	if !strings.HasSuffix(got, "the actual error") {
		t.Errorf("tail = %q, want it to end on the last thing printed", got[max(0, len(got)-80):])
	}
	if !strings.Contains(got, "earlier output omitted") {
		t.Error("a cut tail does not say it was cut")
	}
	if strings.Contains(got, "line 0\n") {
		t.Error("the head survived, which is the part worth dropping")
	}
}
