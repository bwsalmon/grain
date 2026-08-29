package hypervisor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/kontur/internal/config"
)

// fakechvPath is built once per test run from testdata/fakechv, which
// stands in for the real cloud-hypervisor binary: it serves the same
// vm.power-button / vmm.shutdown API endpoints and lets each test control
// how it reacts to them via environment variables.
var fakechvPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakechv-build")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	fakechvPath = filepath.Join(dir, "fakechv")
	cmd := exec.Command("go", "build", "-o", fakechvPath, "./testdata/fakechv")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("building fakechv test helper: " + err.Error())
	}

	os.Exit(m.Run())
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Disks:           []config.Disk{{Path: "/nonexistent/disk.img"}},
		Firmware:        "/nonexistent/firmware",
		CPUs:            1,
		MemoryMB:        256,
		MemoryShared:    true,
		APISocket:       filepath.Join(t.TempDir(), "api.sock"),
		BinaryPath:      fakechvPath,
		ShutdownTimeout: 200 * time.Millisecond,
	}
}

func startRunner(t *testing.T, cfg config.Config, env []string) *Runner {
	t.Helper()
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}

	r := New(cfg)
	if err := r.Start(io.Discard, io.Discard); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
	})
	return r
}

func TestRunner_GracefulShutdownOnPowerButton(t *testing.T) {
	cfg := testConfig(t)
	r := startRunner(t, cfg, nil)

	done := make(chan struct{})
	go func() {
		r.Shutdown(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return in time")
	}

	err := r.Wait()
	if err != nil {
		t.Errorf("Wait() error = %v, want nil (guest exited 0 on power button)", err)
	}
}

func TestRunner_EscalatesToVMMShutdown(t *testing.T) {
	oldKillGrace := killGrace
	killGrace = 300 * time.Millisecond
	t.Cleanup(func() { killGrace = oldKillGrace })

	cfg := testConfig(t)
	cfg.ShutdownTimeout = 50 * time.Millisecond
	r := startRunner(t, cfg, []string{"FAKECHV_EXIT_ON_POWER_BUTTON=false"})

	done := make(chan struct{})
	go func() {
		r.Shutdown(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return in time")
	}

	if err := r.Wait(); err != nil {
		t.Errorf("Wait() error = %v, want nil (vmm.shutdown exited 0)", err)
	}
}

func TestRunner_EscalatesToSIGKILL(t *testing.T) {
	oldKillGrace := killGrace
	killGrace = 100 * time.Millisecond
	t.Cleanup(func() { killGrace = oldKillGrace })

	cfg := testConfig(t)
	cfg.ShutdownTimeout = 50 * time.Millisecond
	r := startRunner(t, cfg, []string{
		"FAKECHV_SERVE_API=false",
		"FAKECHV_IGNORE_SIGTERM=true",
	})

	done := make(chan struct{})
	go func() {
		r.Shutdown(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not return in time (SIGKILL fallback never fired)")
	}

	err := r.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Wait() error = %v, want *exec.ExitError from SIGKILL", err)
	}
}

func TestRunner_Start_InvalidBinary(t *testing.T) {
	cfg := testConfig(t)
	cfg.BinaryPath = "/no/such/binary"
	r := New(cfg)
	if err := r.Start(io.Discard, io.Discard); err == nil {
		t.Fatal("expected Start() to fail for a nonexistent binary")
	}
}
