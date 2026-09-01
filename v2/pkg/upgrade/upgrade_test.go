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

// TestUpgraderRollsBackWhenHealthCheckFails is bwsalmon/agents#418/#422's
// fix: a build that succeeds can still produce a binary that is broken
// at runtime (a panic in some init(), a missing dynamic library, and so
// on), so this builds and installs a "binary" that is a perfectly valid
// executable -- checkout/build/install all genuinely succeed -- but
// which always exits 1 instead of doing anything useful, with
// HealthCheckArgs configured to actually run it. That should be caught
// before ever handing off to RestartCmd: the install gets rolled back
// to the binary that was there before (seeded here the way any upgrade
// after the very first one finds one already in place), and Status
// reports PhaseFailed rather than PhaseOK.
func TestUpgraderRollsBackWhenHealthCheckFails(t *testing.T) {
	_, checkout := newFixtureRepo(t, "feature")
	v2Dir := filepath.Join(checkout, "v2")
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	built := filepath.Join(v2Dir, "bin", "grain")
	installPath := filepath.Join(t.TempDir(), "grain")
	statusFile := filepath.Join(t.TempDir(), "upgrade-status.json")
	restartMarker := filepath.Join(t.TempDir(), "restarted")

	if err := os.WriteFile(installPath, []byte("previous-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(Config{
		SrcDir: checkout,
		// A fake "build" that produces a real, executable script -- so
		// build and install both succeed -- but one that always exits 1
		// instead of doing anything useful once it's actually run, the
		// same way HealthCheckArgs will run it below.
		BuildCmd:        []string{"sh", "-c", "mkdir -p bin && printf '#!/bin/sh\\nexit 1\\n' > bin/grain"},
		BuiltBinary:     built,
		InstallPath:     installPath,
		HealthCheckArgs: []string{"schema-version"},
		// Should never run: a failed health check must stop this before
		// it ever hands off to RestartCmd.
		RestartCmd: []string{"sh", "-c", "touch " + restartMarker},
		StatusFile: statusFile,
	})

	if err := u.Start("feature"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	status := waitForPhase(t, u, PhaseFailed)
	if status.Detail == "" {
		t.Error("failed status carries no Detail")
	}

	// waitForPhase already only returns once the failure status -- which
	// this package only ever writes after rollback and after deciding
	// not to invoke RestartCmd -- is visible, so checking here (rather
	// than polling) is enough.
	if _, err := os.Stat(restartMarker); err == nil {
		t.Error("RestartCmd ran despite a failed health check")
	}

	installed, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("reading installPath after rollback: %v", err)
	}
	if string(installed) != "previous-binary" {
		t.Errorf("installPath after rollback = %q, want the previous binary restored", installed)
	}
	info, err := os.Stat(installPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("restored binary is not executable: mode %v", info.Mode())
	}
}

// TestUpgraderRollsBackToNothingWhenNoPreviousBinaryExisted covers the
// first-ever upgrade on a fresh deployment: there is nothing at
// InstallPath yet for a failed health check to restore, so rollback
// should remove the broken binary it just installed rather than leaving
// it there for a future restart to pick up.
func TestUpgraderRollsBackToNothingWhenNoPreviousBinaryExisted(t *testing.T) {
	_, checkout := newFixtureRepo(t, "feature")
	v2Dir := filepath.Join(checkout, "v2")
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	built := filepath.Join(v2Dir, "bin", "grain")
	installPath := filepath.Join(t.TempDir(), "grain")
	statusFile := filepath.Join(t.TempDir(), "upgrade-status.json")

	u := New(Config{
		SrcDir:          checkout,
		BuildCmd:        []string{"sh", "-c", "mkdir -p bin && printf '#!/bin/sh\\nexit 1\\n' > bin/grain"},
		BuiltBinary:     built,
		InstallPath:     installPath,
		HealthCheckArgs: []string{"schema-version"},
		StatusFile:      statusFile,
	})

	if err := u.Start("feature"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForPhase(t, u, PhaseFailed)

	if _, err := os.Stat(installPath); !os.IsNotExist(err) {
		t.Errorf("installPath after rollback with no previous binary: got err %v, want a not-exist error", err)
	}
	if _, err := os.Stat(installPath + ".prev"); !os.IsNotExist(err) {
		t.Errorf("backup file left behind after rollback: got err %v, want a not-exist error", err)
	}
}

// TestUpgraderRemovesBackupOnceHealthCheckPasses confirms the ".prev"
// backup a passing health check no longer needs doesn't linger forever.
func TestUpgraderRemovesBackupOnceHealthCheckPasses(t *testing.T) {
	_, checkout := newFixtureRepo(t, "feature")
	v2Dir := filepath.Join(checkout, "v2")
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	built := filepath.Join(v2Dir, "bin", "grain")
	installPath := filepath.Join(t.TempDir(), "grain")
	statusFile := filepath.Join(t.TempDir(), "upgrade-status.json")

	if err := os.WriteFile(installPath, []byte("previous-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(Config{
		SrcDir: checkout,
		// A fake "build" that produces a real, executable script that
		// exits 0 -- a health check against it should pass.
		BuildCmd:        []string{"sh", "-c", "mkdir -p bin && printf '#!/bin/sh\\nexit 0\\n' > bin/grain"},
		BuiltBinary:     built,
		InstallPath:     installPath,
		HealthCheckArgs: []string{"schema-version"},
		StatusFile:      statusFile,
	})

	if err := u.Start("feature"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForPhase(t, u, PhaseOK)

	if _, err := os.Stat(installPath + ".prev"); !os.IsNotExist(err) {
		t.Errorf("backup file left behind after a passing health check: got err %v, want a not-exist error", err)
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

// TestUpgraderTimesOutInsteadOfHangingForever is bwsalmon/agents#633's
// regression test ("v2 Deploys are hanging"): before Config.Timeout
// existed, run's ctx was context.Background(), so a build command that
// simply never returned -- a stalled network fetch, a wedged docker
// daemon -- left Status on PhaseRunning forever and u.running permanently
// true, with no way for a second Start to ever get through. A tiny
// Config.Timeout here stands in for that stall: the real default
// (defaultTimeout, 45 minutes) is not something a test can wait out, but
// the mechanism bounding it is the same regardless of the value.
func TestUpgraderTimesOutInsteadOfHangingForever(t *testing.T) {
	_, checkout := newFixtureRepo(t, "feature")
	v2Dir := filepath.Join(checkout, "v2")
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(Config{
		SrcDir: checkout,
		// Far longer than Timeout below -- stands in for a build that
		// hangs rather than one that is merely slow.
		BuildCmd:    []string{"sh", "-c", "sleep 300"},
		BuiltBinary: filepath.Join(v2Dir, "bin", "grain"),
		InstallPath: filepath.Join(t.TempDir(), "grain"),
		StatusFile:  filepath.Join(t.TempDir(), "upgrade-status.json"),
		Timeout:     50 * time.Millisecond,
	})

	if err := u.Start("feature"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// waitForPhase's own 5-second deadline is the assertion: without the
	// fix this never reaches PhaseFailed at all, and the test times out
	// the same way the hang it is guarding against would.
	status := waitForPhase(t, u, PhaseFailed)
	if status.Detail == "" {
		t.Error("failed status carries no Detail")
	}

	// u.running must have cleared too -- otherwise a second Start after
	// the timeout still gets ErrUpgradeInProgress forever, which is the
	// same stuck deployment by another name.
	if err := u.Start("feature"); err != nil {
		t.Errorf("Start after a timed-out upgrade: %v, want nil (u.running should have cleared)", err)
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
