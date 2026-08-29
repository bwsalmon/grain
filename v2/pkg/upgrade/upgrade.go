// Package upgrade drives an in-place upgrade of a running v2 deployment:
// given a branch name, it fetches and checks out that branch in a git
// checkout already on disk, rebuilds bin/grain (v2/Makefile's
// containerised `make container-build`, so nothing about the Go/Node
// toolchain the build needs has to be installed on the host beyond
// docker), installs the result, and -- if the deployment configured one
// -- runs a restart command to bring the new binary up.
//
// bwsalmon/agents#396 (filed "For v2"): "the UI should allow you to
// target a specific branch. When the upgrade button is pushed it should
// download the newest code from the branch, build it locally and start
// running the new version. For now this can restart the host if
// needed." This is deliberately the naive version that issue asked for:
// one upgrade at a time, no rollback, and no health check of the new
// binary before cutting over to it -- a failed build or a failed
// install leaves the old binary running untouched (Start never reaches
// RestartCmd unless every earlier step succeeded), but a build that
// succeeds and then fails at runtime is not this package's problem to
// catch.
package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Phase is where an upgrade (or the deployment's last one) stands.
type Phase string

const (
	PhaseIdle    Phase = "idle"
	PhaseRunning Phase = "running"
	PhaseOK      Phase = "ok"
	PhaseFailed  Phase = "failed"
)

// Status is what Upgrader.Status reports: the deployment's last (or
// current) upgrade attempt. A zero Status (Phase "") means none has ever
// run against this StatusFile.
type Status struct {
	Branch     string     `json:"branch"`
	Phase      Phase      `json:"phase"`
	Detail     string     `json:"detail"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// Config is what an Upgrader needs to know to actually perform one.
type Config struct {
	// SrcDir is a git checkout of bwsalmon/grain already on disk --
	// v2/scripts/setup.sh's own GRAIN_SRC_DIR -- that Start fetches,
	// checks out, and builds in place.
	SrcDir string
	// BuildCmd is run with filepath.Join(SrcDir, "v2") as its working
	// directory to produce a new binary -- []string{"make",
	// "container-build"} on a real deployment (v2/Makefile's
	// containerised build). Overridable so a test can swap in something
	// that runs in milliseconds and needs no docker daemon.
	BuildCmd []string
	// BuiltBinary is where BuildCmd leaves the binary it built --
	// filepath.Join(SrcDir, "v2", "bin", "grain") on a real deployment.
	BuiltBinary string
	// InstallPath is where that binary is copied to run from. Start
	// writes it atomically (a temp file in the same directory, then
	// rename) so a reader (or a service restarting mid-copy) never sees
	// a partial binary.
	InstallPath string
	// RestartCmd, given, is run after a successful build and install to
	// bring the new binary up -- []string{"sudo", "systemctl", "restart",
	// "grain-daemon.service"} on a real deployment
	// (v2/scripts/setup.sh's own unit; see that script for the narrow
	// sudoers grant this needs). Nil skips the restart step entirely:
	// the build and install still happen, but nothing brings the new
	// binary up on its own -- useful for a deployment with no restart
	// mechanism of its own, and for tests.
	RestartCmd []string
	// StatusFile persists Status across the very restart RestartCmd
	// triggers -- an in-memory field would be lost the moment the new
	// binary's own process replaces this one. Required.
	StatusFile string
}

// Upgrader runs at most one upgrade at a time against a Config.
type Upgrader struct {
	cfg Config

	mu      sync.Mutex
	running bool
}

// New builds an Upgrader. It does not validate cfg -- Start reports
// anything wrong the first time it is actually asked to do something,
// the same as most of this package's callers.
func New(cfg Config) *Upgrader {
	return &Upgrader{cfg: cfg}
}

// ErrUpgradeInProgress is what Start returns when an upgrade this same
// process started is still running. It does not detect an upgrade
// another process started -- see Start's own doc comment.
var ErrUpgradeInProgress = errors.New("an upgrade is already in progress")

// branchPattern is deliberately conservative rather than a full port of
// git-check-ref-format(1): letters, digits, '.', '_', '/' and '-',
// starting and ending on an alphanumeric. That is enough to reject
// anything that could be mistaken for a flag by the git/make commands
// Start shells out to (nothing here reaches a shell, so this is a
// clarity guard against a confusing git error, not primarily an
// injection defense) while still accepting every ordinary branch name.
var branchPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._/-]*[A-Za-z0-9])?$`)

func validateBranch(branch string) error {
	if branch == "" {
		return errors.New("branch is required")
	}
	if !branchPattern.MatchString(branch) || strings.Contains(branch, "..") {
		return fmt.Errorf("branch %q is not a valid git ref name", branch)
	}
	return nil
}

// Start validates branch and, if no upgrade started by this same
// Upgrader is currently running, kicks one off in the background and
// returns immediately -- Status reports how it's going. It does not
// detect an upgrade a different process (a previous daemon, before a
// restart) left running: that process's own in-memory "running" flag
// died with it, so a fresh process always allows a new Start. What
// StatusFile survives a restart for is display continuity, not mutual
// exclusion across processes -- see the package doc comment's "one
// upgrade at a time" for the scope that claim is actually made at.
func (u *Upgrader) Start(branch string) error {
	branch = strings.TrimSpace(branch)
	if err := validateBranch(branch); err != nil {
		return err
	}

	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return ErrUpgradeInProgress
	}
	u.running = true
	u.mu.Unlock()

	now := time.Now().UTC()
	status := Status{Branch: branch, Phase: PhaseRunning, StartedAt: &now}
	if err := writeStatus(u.cfg.StatusFile, status); err != nil {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
		return fmt.Errorf("recording upgrade status: %w", err)
	}

	go u.run(branch, status)
	return nil
}

// Status reads the deployment's last (or current) upgrade attempt back
// off disk -- not from memory, so it reports correctly even from a
// process that did not start it (the one this same upgrade's own
// RestartCmd brings up, most notably).
func (u *Upgrader) Status() (Status, error) {
	return readStatus(u.cfg.StatusFile)
}

// run performs the actual checkout/build/install/restart sequence.
// Every step short of RestartCmd itself failing stops here and records
// PhaseFailed with that step's error as Detail; RestartCmd, if it
// succeeds, is expected to end this process before run ever returns.
func (u *Upgrader) run(branch string, status Status) {
	defer func() {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
	}()

	ctx := context.Background()

	fail := func(err error) {
		finished := time.Now().UTC()
		status.Phase = PhaseFailed
		status.Detail = err.Error()
		status.FinishedAt = &finished
		_ = writeStatus(u.cfg.StatusFile, status)
	}

	if err := u.checkout(ctx, branch); err != nil {
		fail(fmt.Errorf("checkout: %w", err))
		return
	}
	if err := u.build(ctx); err != nil {
		fail(fmt.Errorf("build: %w", err))
		return
	}
	if err := u.install(); err != nil {
		fail(fmt.Errorf("install: %w", err))
		return
	}

	finished := time.Now().UTC()
	status.Phase = PhaseOK
	status.Detail = "built and installed"
	status.FinishedAt = &finished
	if len(u.cfg.RestartCmd) > 0 {
		status.Detail = "built and installed; restarting"
	}
	if err := writeStatus(u.cfg.StatusFile, status); err != nil {
		// Nothing else to do with this error: the upgrade itself
		// succeeded, and there is no caller left on the other end of an
		// HTTP request to report a status-file write failure to.
		return
	}

	if len(u.cfg.RestartCmd) == 0 {
		return
	}
	// Best-effort, and its result is never observed: on a real
	// deployment this command replaces the process running this very
	// goroutine (systemctl restart on the unit this binary runs under),
	// so "it ran and this line never got to return" is the success case,
	// not a bug.
	cmd := exec.CommandContext(ctx, u.cfg.RestartCmd[0], u.cfg.RestartCmd[1:]...)
	_ = cmd.Run()
}

// checkout fetches branch from origin and hard-resets the checkout onto
// it -- mirroring v2/scripts/setup.sh's own sync_repo, including its
// refusal to run against a dirty tree (this checkout is meant to be
// managed entirely by this pipeline; local changes in it are either a
// mistake or another operator's in-progress work, and either way this is
// not the thing that should decide to discard them).
func (u *Upgrader) checkout(ctx context.Context, branch string) error {
	dirty, err := u.dirty(ctx)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("%s has uncommitted changes -- refusing to overwrite them", u.cfg.SrcDir)
	}
	if err := u.git(ctx, "fetch", "--quiet", "origin", branch); err != nil {
		return err
	}
	if err := u.git(ctx, "checkout", "--quiet", branch); err != nil {
		return err
	}
	return u.git(ctx, "reset", "--quiet", "--hard", "origin/"+branch)
}

func (u *Upgrader) dirty(ctx context.Context) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", u.cfg.SrcDir, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func (u *Upgrader) git(ctx context.Context, args ...string) error {
	full := append([]string{"-C", u.cfg.SrcDir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (u *Upgrader) build(ctx context.Context) error {
	if len(u.cfg.BuildCmd) == 0 {
		return errors.New("no build command configured")
	}
	cmd := exec.CommandContext(ctx, u.cfg.BuildCmd[0], u.cfg.BuildCmd[1:]...)
	cmd.Dir = filepath.Join(u.cfg.SrcDir, "v2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(u.cfg.BuildCmd, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// install copies BuiltBinary to InstallPath, atomically: a temp file
// alongside InstallPath, chmod'd executable, then renamed over it -- so
// nothing ever reads (or execs) a half-written binary.
func (u *Upgrader) install() error {
	data, err := os.ReadFile(u.cfg.BuiltBinary)
	if err != nil {
		return err
	}
	tmp := u.cfg.InstallPath + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, u.cfg.InstallPath)
}

func readStatus(path string) (Status, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil {
		return Status{}, err
	}
	return status, nil
}

// writeStatus persists status atomically, the same reason install
// copies a binary that way: a concurrent Status read must never see a
// half-written file.
func writeStatus(path string, status Status) error {
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
