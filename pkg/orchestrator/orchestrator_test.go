package orchestrator_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/github/githubsim"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

var baseTime = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

// TestMain turns off two waits for every test in this package.
//
// branchExistsSettled's re-check backoff -- see
// orchestrator.DisableBranchExistsSleep for why it is dead time here
// specifically. The retry behaviour itself is covered by
// TestBranchExistsSettledReChecksANegative, which stubs the same sleep
// itself and asserts the call count, so nothing here is left untested by
// skipping the wall clock.
//
// And the check-registration window -- the wait that keeps an empty check
// list from reading as clean until CI has had time to register (see
// orchestrator.SetCheckRegistrationWindow). Every test here drives a
// githubsim with no CI in it whatsoever, so leaving the window on would
// mean each one either waiting two minutes of real time for a check run
// that is never coming, or seeding a clock jump into an assertion about
// something else entirely. The tests that are about the window set it
// themselves and restore it: the sync_internal_test.go group around
// TestEmptyChecksSettledWaitsOutTheWindow, and
// TestSyncPullRequestsWaitsForCiToRegisterBeforeMergingAFreshPullRequest.
func TestMain(m *testing.M) {
	restoreSleep := orchestrator.DisableBranchExistsSleep()
	restoreWindow := orchestrator.SetCheckRegistrationWindow(0)
	code := m.Run()
	restoreWindow()
	restoreSleep()
	os.Exit(code)
}

// openStore opens a real embedded SQLite store in a temp directory -- the
// same discipline every other package's own tests hold to (model/
// simulate_test.go's own doc comment: "Nothing here is a fake standing in
// for the store").
func openStore(t *testing.T) (*model.Store, context.Context) {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	return store, ctx
}

// newSim is githubsim's own test helper, restated here (package-private,
// deliberately duplicated -- see tests/e2e/harness_test.go's own
// comment on why the same choice is made throughout this codebase): a
// real bare git repo behind a Sim, wired into a real github.RESTClient.
func newSim(t *testing.T, owner, repo, branch string) (*githubsim.Sim, *github.RESTClient) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "repo.git")
	run(t, dir, "git", "init", "--bare", "-q", "-b", branch, bare)

	seed := filepath.Join(dir, "seed")
	run(t, dir, "git", "clone", "-q", bare, seed)
	run(t, seed, "git", "config", "user.email", "seed@example.com")
	run(t, seed, "git", "config", "user.name", "seed")
	run(t, seed, "git", "commit", "-q", "--allow-empty", "-m", "seed")
	run(t, seed, "git", "push", "-q", "origin", branch)

	sim := githubsim.New(owner, repo, bare, branch)
	return sim, github.NewClient(sim, nil)
}

// pushBranch pushes an empty commit on branch straight to bare, standing
// in for a real dispatch's own git push -- this package's own tests care
// about what happens once a branch is real, not about how it got there.
func pushBranch(t *testing.T, bare, branch string) {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "clone", "-q", bare, "work")
	wd := filepath.Join(dir, "work")
	run(t, wd, "git", "config", "user.email", "agent@example.com")
	run(t, wd, "git", "config", "user.name", "agent")
	run(t, wd, "git", "checkout", "-q", "-b", branch)
	// Named after the branch, so two branches pushed off the same base in
	// the same test never share a commit sha. They used to: both are an
	// empty commit with the same message, the same author and -- inside
	// one second, which is all git's timestamps resolve -- the same
	// timestamp, so git hashed them to the same object. That was harmless
	// until the merge queue started reading checks by head sha rather
	// than by branch, at which point one branch's check runs
	// (Sim.CheckRuns, keyed by sha) were served for the other branch's
	// pull request too, and a test holding one pull request's CI queued
	// silently held every other pull request in the same sim queued with
	// it.
	run(t, wd, "git", "commit", "-q", "--allow-empty", "-m", "agent commit on "+branch)
	run(t, wd, "git", "push", "-q", "origin", branch)
}

// branchSHA is what bare has branch pointing at, or "" if it has no such
// branch -- the same resolution a Sim does when it turns a pull
// request's head branch into the sha it keys check runs by.
func branchSHA(t *testing.T, bare, branch string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// The helper's own contract above, checked rather than trusted: every
// merge-queue test here that seeds one branch's check runs depends on no
// other branch it pushed answering to the same sha, and two pushes
// inside one second is exactly the case a fixed commit message used to
// collapse into a single object. githubsim's own copy of pushBranch
// carries the same guard, for the same reason.
func TestPushBranchGivesEachBranchItsOwnSHA(t *testing.T) {
	sim, _ := newSim(t, "acme", "widgets", "main")
	pushBranch(t, sim.BareRepo, "grain/task-1")
	pushBranch(t, sim.BareRepo, "grain/task-2")

	first, second := branchSHA(t, sim.BareRepo, "grain/task-1"), branchSHA(t, sim.BareRepo, "grain/task-2")
	if first == "" || second == "" {
		t.Fatalf("both branches should be on the remote: %q, %q", first, second)
	}
	if first == second {
		t.Fatalf("two branches share a commit sha (%s): the check runs a test seeds for one pull request are served for every other pull request in the sim, and a test holding one queue head's CI queued silently holds the whole queue with it", first)
	}
}

// pushAnotherCommit lands one more commit on a branch that already
// exists -- pushBranch creates one, this moves it, which is what a push
// arriving while the merge queue is mid-cycle does.
func pushAnotherCommit(t *testing.T, bare, branch string) {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "clone", "-q", "--branch", branch, bare, "work")
	wd := filepath.Join(dir, "work")
	run(t, wd, "git", "config", "user.email", "agent@example.com")
	run(t, wd, "git", "config", "user.name", "agent")
	run(t, wd, "git", "commit", "-q", "--allow-empty", "-m", "a later push")
	run(t, wd, "git", "push", "-q", "origin", branch)
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}
