package upgrade

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateBranch(t *testing.T) {
	valid := []string{"main", "grain/issue-396", "feature-1", "a", "a.b_c"}
	for _, b := range valid {
		if err := validateBranch(b); err != nil {
			t.Errorf("validateBranch(%q): unexpected error: %v", b, err)
		}
	}

	invalid := []string{"", "-flag", "has space", "trailing/", "/leading", "a..b", "a\nb", "a\tb"}
	for _, b := range invalid {
		if err := validateBranch(b); err == nil {
			t.Errorf("validateBranch(%q): expected an error, got nil", b)
		}
	}
}

// runGit runs a git command against dir, failing the test on error --
// every fixture below is built entirely out of these.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// newFixtureRepo builds a bare "origin" plus a working checkout cloned
// from it, both under t.TempDir(), with one commit on main and one on a
// second branch named branch -- so checkout has somewhere real to fetch
// and reset onto that differs from what the working checkout already
// has.
func newFixtureRepo(t *testing.T, branch string) (origin, checkout string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	checkout = filepath.Join(root, "checkout")

	runGit(t, root, "init", "--quiet", "--bare", origin)

	seed := filepath.Join(root, "seed")
	runGit(t, root, "init", "--quiet", "-b", "main", seed)
	runGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.com",
		"commit", "--quiet", "--allow-empty", "-m", "initial")
	runGit(t, seed, "push", "--quiet", origin, "HEAD:refs/heads/main")
	runGit(t, seed, "checkout", "--quiet", "-b", branch)
	runGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.com",
		"commit", "--quiet", "--allow-empty", "-m", "on "+branch)
	runGit(t, seed, "push", "--quiet", origin, "HEAD:refs/heads/"+branch)

	runGit(t, root, "clone", "--quiet", "-b", "main", origin, checkout)
	return origin, checkout
}

func TestUpgraderStartRunsCheckoutBuildInstallAndRestart(t *testing.T) {
	_, checkout := newFixtureRepo(t, "feature")

	// v2/<build cmd> needs filepath.Join(SrcDir, "v2") to exist as a
	// working directory.
	v2Dir := filepath.Join(checkout, "v2")
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	built := filepath.Join(v2Dir, "bin", "grain")
	installPath := filepath.Join(t.TempDir(), "grain")
	restartMarker := filepath.Join(t.TempDir(), "restarted")
	statusFile := filepath.Join(t.TempDir(), "upgrade-status.json")

	u := New(Config{
		SrcDir: checkout,
		// A fake "build": writes a recognizable binary rather than
		// actually invoking make/docker, so this test needs neither.
		BuildCmd:    []string{"sh", "-c", "mkdir -p bin && printf fake-binary > bin/grain"},
		BuiltBinary: built,
		InstallPath: installPath,
		RestartCmd:  []string{"sh", "-c", "touch " + restartMarker},
		StatusFile:  statusFile,
	})

	if err := u.Start("feature"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForPhase(t, u, PhaseOK)

	installed, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("reading installed binary: %v", err)
	}
	if string(installed) != "fake-binary" {
		t.Errorf("installed binary content = %q, want %q", installed, "fake-binary")
	}
	info, err := os.Stat(installPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable: mode %v", info.Mode())
	}

	// The restart command runs after the "ok" status is already written
	// (deliberately -- see run's own comment), so waitForPhase returning
	// does not guarantee it has finished yet; poll for it too.
	waitForFile(t, restartMarker)

	// The working checkout should have moved onto the branch's own commit.
	logCmd := exec.Command("git", "-C", checkout, "log", "-1", "--format=%s")
	out, err := logCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "on feature\n" {
		t.Errorf("checkout HEAD commit message = %q, want %q", got, "on feature\n")
	}
}

// TestUpgraderReportsOKEvenWhenNewBinaryIsBrokenAtRuntime pins down the
// gap called out in this package's own doc comment: "a build that
// succeeds and then fails at runtime is not this package's problem to
// catch." It builds and installs a "binary" that is a perfectly valid
// executable -- so checkout/build/install all genuinely succeed -- but
// which does nothing useful when actually run, then has RestartCmd run
// that installed binary the same way a real restart would bring it up.
// Today there is no health check anywhere in this path, so Status ends
// up reporting PhaseOK regardless of whether the new binary actually
// works; this test exists so a future health-check/rollback feature has
// a red test to turn green instead of this staying an undocumented gap.
func TestUpgraderReportsOKEvenWhenNewBinaryIsBrokenAtRuntime(t *testing.T) {
	_, checkout := newFixtureRepo(t, "feature")
	v2Dir := filepath.Join(checkout, "v2")
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	built := filepath.Join(v2Dir, "bin", "grain")
	installPath := filepath.Join(t.TempDir(), "grain")
	statusFile := filepath.Join(t.TempDir(), "upgrade-status.json")
	// ranOK is only ever touched by a successful run of the installed
	// binary; attempted is touched unconditionally, after that run (success
	// or not) completes. Since the installed binary always exits 1, ranOK's
	// absence once attempted exists is what confirms the "broken at
	// runtime" setup actually took effect, rather than the test
	// accidentally passing for an unrelated reason.
	ranOK := filepath.Join(t.TempDir(), "ran-ok")
	attempted := filepath.Join(t.TempDir(), "attempted")

	u := New(Config{
		SrcDir: checkout,
		// A fake "build" that produces a real, executable script -- so
		// build and install both succeed -- but one that always exits 1
		// instead of doing anything useful once it's actually running.
		BuildCmd:    []string{"sh", "-c", "mkdir -p bin && printf '#!/bin/sh\\nexit 1\\n' > bin/grain"},
		BuiltBinary: built,
		InstallPath: installPath,
		// Stands in for "sudo systemctl restart grain-daemon.service":
		// actually runs the newly installed binary, same as a real
		// restart would. Because that binary always fails, ranOK is
		// never created, but attempted always is.
		RestartCmd: []string{"sh", "-c", installPath + " && touch " + ranOK + "; touch " + attempted},
		StatusFile: statusFile,
	})

	if err := u.Start("feature"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	status := waitForPhase(t, u, PhaseOK)
	if status.Detail == "" {
		t.Error("ok status carries no Detail")
	}

	// Give RestartCmd -- which runs after the "ok" status is already
	// written -- time to finish, then confirm the installed binary really
	// did fail, i.e. that this test is actually exercising the runtime
	// failure it claims to.
	waitForFile(t, attempted)
	if _, err := os.Stat(ranOK); err == nil {
		t.Fatal("test bug: the installed binary reported success, so this test isn't exercising a runtime failure")
	}

	// This is the gap issue #418 pins down: even though the new binary is
	// broken and RestartCmd's own attempt to bring it up failed, Status
	// still reports PhaseOK with no trace of that failure. There is no
	// health check of the new binary and no automatic rollback -- an
	// operator is left running a broken new binary, and the only sign
	// anything is wrong is outside this package entirely.
	final, err := u.Status()
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != PhaseOK {
		t.Errorf("Status phase = %q, want %q (today's behavior: a broken-at-runtime binary is invisible to this package)", final.Phase, PhaseOK)
	}
}

func TestUpgraderStartRejectsConcurrentUpgrade(t *testing.T) {
	_, checkout := newFixtureRepo(t, "feature")
	v2Dir := filepath.Join(checkout, "v2")
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(Config{
		SrcDir: checkout,
		// Slow enough that the second Start below reliably lands while
		// this one is still "running".
		BuildCmd:    []string{"sh", "-c", "sleep 1 && mkdir -p bin && printf x > bin/grain"},
		BuiltBinary: filepath.Join(v2Dir, "bin", "grain"),
		InstallPath: filepath.Join(t.TempDir(), "grain"),
		StatusFile:  filepath.Join(t.TempDir(), "upgrade-status.json"),
	})

	if err := u.Start("feature"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := u.Start("feature"); err != ErrUpgradeInProgress {
		t.Errorf("second Start: got %v, want ErrUpgradeInProgress", err)
	}

	waitForPhase(t, u, PhaseOK)
}

func TestUpgraderStartReportsFailure(t *testing.T) {
	_, checkout := newFixtureRepo(t, "feature")
	v2Dir := filepath.Join(checkout, "v2")
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(Config{
		SrcDir:      checkout,
		BuildCmd:    []string{"sh", "-c", "exit 1"},
		BuiltBinary: filepath.Join(v2Dir, "bin", "grain"),
		InstallPath: filepath.Join(t.TempDir(), "grain"),
		StatusFile:  filepath.Join(t.TempDir(), "upgrade-status.json"),
	})

	if err := u.Start("feature"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	status := waitForPhase(t, u, PhaseFailed)
	if status.Detail == "" {
		t.Error("failed status carries no Detail")
	}

	// A fresh Upgrader over the same StatusFile (standing in for the
	// next process, since RestartCmd's whole point is to replace this
	// one) reports the same failure back.
	fresh := New(Config{StatusFile: u.cfg.StatusFile})
	again, err := fresh.Status()
	if err != nil {
		t.Fatal(err)
	}
	if again.Phase != PhaseFailed {
		t.Errorf("fresh Upgrader's Status phase = %q, want %q", again.Phase, PhaseFailed)
	}
}

func TestUpgraderStartRejectsInvalidBranch(t *testing.T) {
	u := New(Config{StatusFile: filepath.Join(t.TempDir(), "upgrade-status.json")})
	if err := u.Start("-not-a-branch"); err == nil {
		t.Error("expected an error for an invalid branch, got nil")
	}
	status, err := u.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != "" {
		t.Errorf("a rejected Start should never have written a status; got phase %q", status.Phase)
	}
}

func TestStatusReportsIdleWhenNeverRun(t *testing.T) {
	u := New(Config{StatusFile: filepath.Join(t.TempDir(), "upgrade-status.json")})
	status, err := u.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != "" {
		t.Errorf("Phase = %q, want empty (never run)", status.Phase)
	}
}

// waitForPhase polls Status until it reports want or a phase later than
// "running" that isn't want (a test bug), or times out.
func waitForPhase(t *testing.T, u *Upgrader, want Phase) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := u.Status()
		if err != nil {
			t.Fatal(err)
		}
		if status.Phase == want {
			return status
		}
		if status.Phase == PhaseFailed && want != PhaseFailed {
			t.Fatalf("upgrade failed: %s", status.Detail)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for phase %q", want)
	return Status{}
}

// waitForFile polls for path to exist, failing the test if it never
// shows up -- for asserting on a side effect (the restart command) that
// happens after Status already reports the phase that triggers it.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to exist", path)
}
