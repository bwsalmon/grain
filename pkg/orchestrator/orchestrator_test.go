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

// TestMain turns off branchExistsSettled's re-check backoff for every
// test in this package -- see orchestrator.DisableBranchExistsSleep for
// why it is dead time here specifically. The retry behaviour itself is
// covered by TestBranchExistsSettledReChecksANegative, which stubs the
// same sleep itself and asserts the call count, so nothing here is left
// untested by skipping the wall clock.
func TestMain(m *testing.M) {
	restore := orchestrator.DisableBranchExistsSleep()
	code := m.Run()
	restore()
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
// deliberately duplicated -- see e2e/harness_test.go's own comment on
// why the same choice is made throughout this codebase): a real bare git
// repo behind a Sim, wired into a real github.RESTClient.
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
	run(t, wd, "git", "commit", "-q", "--allow-empty", "-m", "agent commit")
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
