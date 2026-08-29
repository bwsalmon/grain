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
	// exists from a previous run.
	if err := os.Remove(r.cfg.APISocket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale api socket: %w", err)
	}

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
