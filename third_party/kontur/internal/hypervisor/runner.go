package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/kontur/internal/config"
)

// killGrace bounds how long the runtime waits after a forced vmm.shutdown
// (and, as a last resort, SIGTERM) before it gives up and sends SIGKILL.
// Overridable (package-private) so tests don't have to wait out the real
// value.
var killGrace = 5 * time.Second

// Runner starts and supervises a single cloud-hypervisor process.
type Runner struct {
	cfg config.Config
	cmd *exec.Cmd
	api *APIClient

	// restored records whether Start booted by restoring cfg.SnapshotPath
	// (see BuildArgs and SnapshotExists) rather than booting fresh. Only
	// meaningful after Start returns.
	restored bool

	// done is closed once the process has exited; exitErr is only safe
	// to read after done is closed.
	done    chan struct{}
	exitErr error

	shutdownOnce sync.Once
}

// New returns a Runner for cfg. Call Start to launch the VM.
func New(cfg config.Config) *Runner {
	return &Runner{
		cfg: cfg,
		api: NewAPIClient(cfg.APISocket),
	}
}

// Start execs cloud-hypervisor with the arguments derived from the config
// and returns once the process has been launched. It does not wait for the
// guest to finish booting.
func (r *Runner) Start(stdout, stderr io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(r.cfg.APISocket), 0o755); err != nil {
		return fmt.Errorf("creating api socket directory: %w", err)
	}
	// cloud-hypervisor refuses to bind an api-socket path that already
	// exists from a previous run -- but one a running VM is still
	// listening on is not this run's to remove. See RemoveStaleSocket.
	if err := RemoveStaleSocket(r.cfg.APISocket, "cloud-hypervisor's API socket"); err != nil {
		return err
	}

	r.restored = SnapshotExists(r.cfg.SnapshotPath)
	args := BuildArgs(r.cfg)
	log.Printf("starting: %s", String(r.cfg.BinaryPath, args))

	cmd := exec.Command(r.cfg.BinaryPath, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", r.cfg.BinaryPath, err)
	}
	r.cmd = cmd
	r.done = make(chan struct{})

	go func() {
		r.exitErr = cmd.Wait()
		close(r.done)
	}()

	return nil
}

// Restored reports whether Start booted this VM by restoring a
// previously suspended snapshot (see Suspend) rather than booting it
// fresh. A restored VM has already been through whatever one-time setup
// produced that snapshot, so callers (cmd/kontur's runVM) skip
// re-running it.
func (r *Runner) Restored() bool {
	return r.restored
}

// Suspend pauses the running guest, writes a full snapshot of its state
// to cfg.SnapshotPath (see BuildArgs, which restores from it on a later
// run instead of booting fresh), and resumes it -- so this run carries
// on exactly as if Suspend had never been called. It only leaves behind
// a checkpoint for the *next* kontur run to pick up.
func (r *Runner) Suspend(ctx context.Context) error {
	if r.cfg.SnapshotPath == "" {
		return fmt.Errorf("no snapshot path configured")
	}

	if err := r.api.Pause(ctx); err != nil {
		return fmt.Errorf("pausing vm: %w", err)
	}

	err := r.snapshot(ctx)

	if resumeErr := r.api.Resume(ctx); resumeErr != nil {
		if err == nil {
			return fmt.Errorf("resuming vm after snapshot: %w", resumeErr)
		}
		log.Printf("resuming vm after failed snapshot also failed: %v", resumeErr)
	}
	return err
}

// snapshot performs the actual vm.snapshot call and publishes its result
// at cfg.SnapshotPath. cloud-hypervisor requires the destination
// directory to already exist, so this snapshots into a fresh sibling
// directory first and renames it into place only once it's complete --
// a reader (BuildArgs's SnapshotExists check) never sees a partial
// snapshot, even if this process is killed mid-write.
func (r *Runner) snapshot(ctx context.Context) error {
	tmp := r.cfg.SnapshotPath + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clearing stale snapshot staging directory %s: %w", tmp, err)
	}
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return fmt.Errorf("creating snapshot staging directory: %w", err)
	}

	if err := r.api.Snapshot(ctx, tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("snapshotting vm: %w", err)
	}

	// cloud-hypervisor's own snapshot is memory and device state; the
	// disk behind it stays wherever it was. In the default disk mode
	// that disk is a qcow2 overlay inside this container's writable
	// layer (see config.PrepareOverlay), which the next run -- a new
	// container, which is the whole point of snapshotting to a volume
	// -- does not have: it would make a fresh one from the base image
	// and resume a guest whose memory believes in files the disk under
	// it no longer holds. So the overlay goes into the snapshot too,
	// while the VM is paused and it is not being written, and
	// RestoreDiskOverlay puts it back before the next boot restores.
	if r.cfg.DiskMode == config.DiskModeOverlay && r.cfg.OverlayPath != "" {
		if err := copyFile(r.cfg.OverlayPath, filepath.Join(tmp, SnapshotDiskOverlayName)); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("saving the guest's disk overlay into the snapshot: %w", err)
		}
	}

	if err := os.Rename(tmp, r.cfg.SnapshotPath); err != nil {
		return fmt.Errorf("publishing snapshot to %s: %w", r.cfg.SnapshotPath, err)
	}
	return nil
}

// SnapshotDiskOverlayName is the file name a snapshot directory carries
// the guest's writable qcow2 overlay under (see Runner.snapshot and
// RestoreDiskOverlay). It sits alongside cloud-hypervisor's own
// config.json/state.json/memory-ranges, which name themselves and ignore
// anything else in the directory.
const SnapshotDiskOverlayName = "kontur-disk-overlay.qcow2"

// RestoreDiskOverlay copies the disk overlay saved inside the snapshot at
// snapshotPath back to dst, reporting whether the snapshot had one to
// put back at all -- a snapshot taken before kontur saved it does not,
// and a guest resumed from one gets whatever disk the container it lands
// in already has (see Runner.snapshot for why that matters).
//
// It writes through a sibling temporary file and renames, so a copy
// interrupted partway through leaves the destination as it was rather
// than a truncated qcow2 that config.PrepareOverlay would go on to reuse
// as if it were whole.
func RestoreDiskOverlay(snapshotPath, dst string) (bool, error) {
	if snapshotPath == "" || dst == "" {
		return false, nil
	}
	src := filepath.Join(snapshotPath, SnapshotDiskOverlayName)
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("checking the disk overlay saved at %s: %w", src, err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, fmt.Errorf("creating the overlay directory for %s: %w", dst, err)
	}
	tmp := dst + ".restoring"
	if err := copyFile(src, tmp); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("restoring the disk overlay saved at %s: %w", src, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("putting the restored disk overlay at %s: %w", dst, err)
	}
	return true, nil
}

// copyFile copies src over dst, creating dst if it isn't there, and
// flushes it to disk: both callers are writing something a *later* run
// (or a later container) has to be able to read back intact.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Wait blocks until the VMM process exits and returns its error, following
// the same convention as exec.Cmd.Wait (nil for a clean exit, *exec.ExitError
// for a non-zero exit code). Safe to call from multiple goroutines.
func (r *Runner) Wait() error {
	<-r.done
	return r.exitErr
}

// Shutdown asks the guest to power off gracefully, escalating to a forced
// VMM stop and finally to SIGKILL if it does not exit in time. It returns
// once the process has exited. Safe to call at most once per Runner.
func (r *Runner) Shutdown(ctx context.Context) {
	r.shutdownOnce.Do(func() { r.shutdown(ctx) })
}

func (r *Runner) shutdown(ctx context.Context) {
	log.Printf("requesting graceful guest shutdown (timeout %s)", r.cfg.ShutdownTimeout)
	pbCtx, cancel := context.WithTimeout(ctx, killGrace)
	// The api-socket is created early in cloud-hypervisor's startup, but
	// Start returns as soon as the process is forked, so there is a
	// small window where it isn't listening yet.
	err := r.api.WaitReady(pbCtx)
	if err == nil {
		err = r.api.PowerButton(pbCtx)
	}
	cancel()
	if err != nil {
		log.Printf("power-button request failed: %v", err)
	} else if r.waitUntil(r.cfg.ShutdownTimeout) {
		return
	}

	log.Printf("guest did not shut down in time, forcing vmm shutdown")
	forceCtx, cancel := context.WithTimeout(ctx, killGrace)
	err = r.api.WaitReady(forceCtx)
	if err == nil {
		err = r.api.ShutdownVMM(forceCtx)
	}
	cancel()
	if err != nil {
		log.Printf("vmm.shutdown request failed: %v", err)
	} else if r.waitUntil(killGrace) {
		return
	}

	if r.cmd.Process != nil {
		log.Printf("vmm still running, sending SIGTERM")
		_ = r.cmd.Process.Signal(syscall.SIGTERM)
		if r.waitUntil(killGrace) {
			return
		}

		log.Printf("vmm still running, sending SIGKILL")
		_ = r.cmd.Process.Kill()
	}
	<-r.done
}

// waitUntil blocks until the process exits or timeout elapses, reporting
// which happened. Safe to call repeatedly / concurrently with Wait.
func (r *Runner) waitUntil(timeout time.Duration) (exited bool) {
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
}
