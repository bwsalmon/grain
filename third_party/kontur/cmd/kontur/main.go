// Command kontur is the container-facing binary for kontur: it either
// boots a single cloud-hypervisor VM as PID 1 of a container ("run", the
// default), sets up pod-local networking as a Kubernetes init container
// ("netshim"), or blocks forever ("sleep", used by -backend docker to
// hold a network namespace open in place of a pod sandbox -- see
// internal/dockervm), configured entirely from the environment where
// applicable. All three modes live in the same binary and the same OCI
// image -- which, since internal/netshim talks to the kernel directly via
// netlink/nftables rather than exec'ing external CLIs, ships from
// "scratch" with no shell or coreutils of its own, so "sleep" exists
// here rather than relying on one being present in the image -- invoked
// with different args per role. See README.md for the environment
// variables each mode understands.
//
// This is distinct from cmd/konturctl, which is the operator-facing CLI
// that runs on the node itself (not inside a container) to manage those
// pods.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/bwsalmon/kontur/internal/config"
	"github.com/bwsalmon/kontur/internal/hypervisor"
	"github.com/bwsalmon/kontur/internal/netshim"
)

func main() {
	log.SetFlags(0)

	mode := "run"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "run":
		log.SetPrefix("kontur: ")
		if err := runVM(); err != nil {
			log.Fatalf("%v", err)
		}
	case "netshim":
		log.SetPrefix("kontur: netshim: ")
		if err := runNetshim(); err != nil {
			log.Fatalf("%v", err)
		}
	case "sleep":
		// A bare "select {}" here would make Go's deadlock detector kill
		// the process immediately (nothing else could ever wake the sole
		// goroutine), so block on an explicit signal instead: "docker
		// stop" sends SIGTERM (then SIGKILL after its grace period if
		// this doesn't exit first), matching how "sleep infinity" would
		// have responded.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
	default:
		log.Fatalf("kontur: unknown mode %q (want \"run\", \"netshim\" or \"sleep\")", mode)
	}
}

// runVM boots a single cloud-hypervisor VM and forwards Kubernetes
// lifecycle signals to it, exactly as the standalone chv-runtime binary
// used to. Its own stdout/stderr become the VM's serial console output,
// so "kubectl logs" on the container is the VM's console.
func runVM() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	runner := hypervisor.New(cfg)
	if err := runner.Start(os.Stdout, os.Stderr); err != nil {
		return err
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("received %s, shutting down", sig)
		runner.Shutdown(context.Background())
	}()

	err = runner.Wait()
	if err == nil {
		log.Printf("cloud-hypervisor exited cleanly")
		return nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		log.Printf("cloud-hypervisor exited with status %d", exitErr.ExitCode())
		os.Exit(exitErr.ExitCode())
	}
	return err
}

// runNetshim sets up the shared-IP network shim for the VM containers that
// follow it in the same pod, exactly as the standalone netshim binary
// used to.
func runNetshim() error {
	cfg, err := netshim.FromEnv()
	if err != nil {
		return err
	}

	if err := netshim.Setup(cfg); err != nil {
		return err
	}

	log.Printf("network shim ready: bridge %s (%s), %d VM(s)", cfg.Bridge, cfg.BridgeNet, len(cfg.VMs))
	return nil
}
