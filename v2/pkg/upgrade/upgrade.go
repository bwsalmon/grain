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
// one upgrade at a time -- a failed build or a failed install leaves
// the old binary running untouched (Start never reaches RestartCmd
// unless every earlier step succeeded).
//
// bwsalmon/agents#418/#422: a build that succeeds can still be broken
// at runtime (a panic during init, a missing dynamic dependency, a
// binary that just exits immediately), and that used to be entirely
// invisible here -- Status would report "ok" forever with no trace of
// it. If Config.HealthCheckArgs is set, Start now runs the newly
// installed binary itself (InstallPath, not BuiltBinary) with those
// args right after install and before RestartCmd; a nonzero exit or a
// timeout there rolls the install back to whatever binary was at
// InstallPath before (or removes it, if this was the very first
// install) and reports PhaseFailed instead of cutting over to
// RestartCmd at all. This still cannot catch a binary that starts up
// fine but misbehaves only once it is actually serving real traffic --
// that class of failure needs a real health check against the running
// service, which is out of this package's reach (see HealthCheckArgs's
// own doc comment).
//
// bwsalmon/agents#633: checkout and build both used to run under
// context.Background(), so a git fetch against a stalled connection or a
// docker build stuck on an unresponsive registry never returned at all --
// Status sat on PhaseRunning forever, and nothing short of restarting the
// whole daemon process cleared u.running to let a retry through. Config.
// Timeout (defaultTimeout, 45 minutes, if left unset) now bounds both.
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
	"syscall"
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
	// HealthCheckArgs, if non-empty, are run as exec.Command(InstallPath,
	// HealthCheckArgs...) right after install and before RestartCmd --
	// []string{"schema-version"} on a real deployment: a subcommand that
	// touches no store, needs no config, and exits immediately, so
	// failing to even run it means the newly built binary is broken
	// outright (a panic in an init(), a missing dynamic library, a
	// build that produced something that isn't a working executable at
	// all) rather than that its actual serving behavior is somehow
	// wrong -- that narrower kind of failure only shows up once the
	// binary is actually handling real traffic, which is beyond what a
	// short-lived subprocess here can check. A nonzero exit or a
	// 10-second timeout rolls the install back (see install's own doc
	// comment) and reports PhaseFailed without ever running RestartCmd.
	// Empty skips the check and installs unconditionally -- the
	// previous behavior, and what every test with no real "grain"
	// binary of its own relies on.
	HealthCheckArgs []string
	// RestartCmd, given, is run after a successful build and install to
	// bring the new binary up -- []string{"sudo", "systemctl", "restart",
	// "grain-daemon.service"} on a real deployment
	// (v2/scripts/setup.sh's own unit; see that script for the narrow
	// sudoers grant this needs). Nil skips the restart step entirely:
	// the build and install still happen, but nothing brings the new
	// binary up on its own -- useful for a deployment with no restart
	// mechanism of its own, and for tests.
	RestartCmd []string
	// Timeout bounds checkout and build together -- the only two steps
	// here that touch the network (a git fetch against origin) or a cold
	// docker build (pulling a builder image plus every Go module and npm
	// package), either of which can, on a bad day, simply stop rather
	// than return an error: a stalled TCP connection, an unresponsive
	// registry, a wedged Docker daemon. Neither used to be bounded at
	// all -- run's own ctx was context.Background() -- so that failure
	// mode did not fail: Status sat on PhaseRunning forever, u.running
	// never cleared, the UI's Upgrade button stayed on "Upgrading…"
	// indefinitely, and a second click just got ErrUpgradeInProgress
	// (bwsalmon/agents#633, "v2 Deploys are hanging"). Zero uses
	// defaultTimeout -- the same 45 minutes
	// terraform/gcp-v2/files/config-sync.sh's own DEPLOY_TIMEOUT_SECS
	// budgets for this identical cold-build cost on the other path to
	// this same build, for the same reason.
	Timeout time.Duration
	// StatusFile persists Status across the very restart RestartCmd
	// triggers -- an in-memory field would be lost the moment the new
	// binary's own process replaces this one. Required.
	StatusFile string
	// Image, when non-nil, replaces the checkout/build/install steps
	// with pulling the image CI published for the branch and pointing
	// the deployment at it -- what "upgrade" means on a host that runs
	// grain from a container rather than from a binary it built itself
	// (image.go, bwsalmon/agents#645). SrcDir, BuildCmd, BuiltBinary and
	// InstallPath are unused when it is set; every other field means
	// exactly what it does otherwise.
	Image *ImageConfig
}

// defaultTimeout is Config.Timeout's fallback when left at its zero
// value -- see that field's own doc comment.
const defaultTimeout = 45 * time.Minute

// waitDelay bounds how long Wait (via CombinedOutput/Output, below)
// waits for a command's own stdio to actually finish once ctx cancels it
// and newCommand's own Cancel func has run. Context cancellation alone --
// even exec.CommandContext's default Cancel, which kills only the direct
// child -- does not unblock a Read against a pipe some surviving
// descendant still holds open, and that is exactly the shape a hung
// `make container-build` takes: make forks docker/npm/go, each of which
// inherits make's own stdout/stderr, so killing make itself leaves
// nothing to make the read end of that pipe see EOF. Verified live
// against `sh -c "sleep 300"` standing in for a wedged build: without
// WaitDelay, CombinedOutput blocked for the full 300 seconds despite a
// 50ms ctx deadline. newCommand's own process-group kill (below) reaches
// every descendant directly and makes this bound almost never the thing
// that actually fires; it exists as the backstop for whatever that kill
// still somehow misses.
const waitDelay = 5 * time.Second

// newCommand builds an *exec.Cmd whose cancellation -- ctx's Timeout
// expiring, most notably -- reliably bounds it even when the command
// forks children of its own that inherit its stdout/stderr pipes, the
// shape make/docker/npm take and exactly what checkout and build run.
// Setpgid puts the whole tree in a process group of its own; Cancel
// kills that group (a negative pid to syscall.Kill) rather than
// exec.CommandContext's own default of the direct child alone, so a
// grandchild cannot simply outlive its parent's death and keep running,
// orphaned, in the background; WaitDelay is waitDelay's own backstop.
func newCommand(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay
	return cmd
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

	timeout := u.cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	fail := func(err error) {
		finished := time.Now().UTC()
		status.Phase = PhaseFailed
		status.Detail = err.Error()
		status.FinishedAt = &finished
		_ = writeStatus(u.cfg.StatusFile, status)
	}

	detail, err := u.stage(ctx, branch)
	if err != nil {
		fail(err)
		return
	}

	finished := time.Now().UTC()
	status.Phase = PhaseOK
	status.Detail = detail
	status.FinishedAt = &finished
	if len(u.cfg.RestartCmd) > 0 {
		status.Detail = detail + "; restarting"
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

// stage does everything an upgrade does short of announcing success and
// restarting: whichever of the two pipelines this deployment is
// configured for, run to completion. What it returns is the Detail line
// Status carries afterwards -- "built and installed", or the image now
// recorded -- since the two paths leave behind genuinely different
// things and a status saying "built" on a host that never built
// anything would be a lie an operator has to decode.
func (u *Upgrader) stage(ctx context.Context, branch string) (string, error) {
	if u.cfg.Image != nil {
		return u.stageImage(ctx, branch)
	}
	return u.stageBinary(ctx, branch)
}

// stageImage is the container path (image.go): pull, health-check, and
// only then point the deployment's ref file at what was pulled.
func (u *Upgrader) stageImage(ctx context.Context, branch string) (string, error) {
	ref, err := u.imageRef(branch)
	if err != nil {
		return "", err
	}
	if err := u.pullImage(ctx, ref); err != nil {
		return "", fmt.Errorf("pull: %w", err)
	}
	if len(u.cfg.HealthCheckArgs) > 0 {
		if err := u.healthCheckImage(ctx, ref); err != nil {
			// Nothing to roll back: the ref file below is still
			// untouched, so this deployment goes on running exactly the
			// image it was already running.
			return "", fmt.Errorf("health check failed on the pulled image, leaving this deployment on the image it already runs: %w", err)
		}
	}
	if err := u.writeImageRef(ref); err != nil {
		return "", fmt.Errorf("recording %s: %w", u.cfg.Image.RefFile, err)
	}
	return "pulled " + ref, nil
}

// stageBinary is the original path: fetch the branch into a checkout on
// disk, build it, and install the result over the running binary.
func (u *Upgrader) stageBinary(ctx context.Context, branch string) (string, error) {
	if err := u.checkout(ctx, branch); err != nil {
		return "", fmt.Errorf("checkout: %w", err)
	}
	if err := u.build(ctx); err != nil {
		return "", fmt.Errorf("build: %w", err)
	}
	hadPrevious, err := u.backupInstalled()
	if err != nil {
		return "", fmt.Errorf("backing up previous binary: %w", err)
	}
	if err := u.install(); err != nil {
		return "", fmt.Errorf("install: %w", err)
	}
	if len(u.cfg.HealthCheckArgs) > 0 {
		if err := u.healthCheck(ctx); err != nil {
			if rerr := u.rollbackInstalled(hadPrevious); rerr != nil {
				return "", fmt.Errorf("health check failed (%w) and rollback also failed: %v", err, rerr)
			}
			if hadPrevious {
				return "", fmt.Errorf("health check failed on newly installed binary, rolled back to the previous one: %w", err)
			}
			return "", fmt.Errorf("health check failed on newly installed binary, removed it (no previous binary to roll back to): %w", err)
		}
	}
	u.removeBackup()
	return "built and installed", nil
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
	cmd := newCommand(ctx, "", "git", "-C", u.cfg.SrcDir, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func (u *Upgrader) git(ctx context.Context, args ...string) error {
	full := append([]string{"-C", u.cfg.SrcDir}, args...)
	cmd := newCommand(ctx, "", "git", full...)
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
	cmd := newCommand(ctx, filepath.Join(u.cfg.SrcDir, "v2"), u.cfg.BuildCmd[0], u.cfg.BuildCmd[1:]...)
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

// backupPath is where backupInstalled keeps whatever was at InstallPath
// before this upgrade's own install overwrites it, so rollbackInstalled
// has something to restore if the newly installed binary fails its
// health check.
func (u *Upgrader) backupPath() string {
	return u.cfg.InstallPath + ".prev"
}

// backupInstalled copies whatever is currently at InstallPath (an
// earlier upgrade's binary, or the one setup.sh first installed) aside
// before install overwrites it. hadPrevious is false, with no error,
// when there is nothing there yet -- the very first install onto a
// fresh deployment -- which rollbackInstalled uses to know it has
// nothing to restore.
func (u *Upgrader) backupInstalled() (hadPrevious bool, err error) {
	data, err := os.ReadFile(u.cfg.InstallPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(u.backupPath(), data, 0o755); err != nil {
		return false, err
	}
	return true, nil
}

// rollbackInstalled undoes install: restores the binary backupInstalled
// set aside, or -- if there was none, meaning this was the first ever
// install -- removes the broken one instead of leaving it in place for
// a future restart to pick up.
func (u *Upgrader) rollbackInstalled(hadPrevious bool) error {
	if !hadPrevious {
		if err := os.Remove(u.cfg.InstallPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.Rename(u.backupPath(), u.cfg.InstallPath)
}

// removeBackup discards the backup taken before a health check that
// ended up passing (or was skipped entirely) -- there is nothing left
// to ever roll back to once the newly installed binary is confirmed
// good, and leaving it around would just be a stale file next to
// InstallPath forever.
func (u *Upgrader) removeBackup() {
	_ = os.Remove(u.backupPath())
}

// healthCheck runs the binary install just wrote to disk -- InstallPath
// itself, not BuiltBinary -- with Config.HealthCheckArgs, on the theory
// that a binary broken at runtime (a panic in some init(), a missing
// dynamic library, a build that produced something that plain doesn't
// run) fails this the same way it would fail for real, in well under
// the 10 seconds allotted here; a subcommand like "schema-version" that
// touches no store and needs no config is expected to return almost
// instantly, so the timeout exists only to bound a binary that
// unexpectedly hangs rather than exiting, not to allow for any real
// work.
func (u *Upgrader) healthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := newCommand(ctx, "", u.cfg.InstallPath, u.cfg.HealthCheckArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", u.cfg.InstallPath, strings.Join(u.cfg.HealthCheckArgs, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
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
