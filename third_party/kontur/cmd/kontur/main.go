// Command kontur is the container-facing binary for kontur: it either
// boots a single cloud-hypervisor VM as PID 1 of a container ("run", the
// default), sets up pod-local networking as a Kubernetes init container
// ("netshim"), blocks forever ("sleep", used by -backend docker to hold
// a network namespace open in place of a pod sandbox -- see
// internal/dockervm), or runs a command inside the VM guest over SSH
// ("exec", see internal/guestexec) -- meant to be invoked as `kubectl
// exec`'s own command, so that ends up in the guest rather than this
// otherwise-empty container. All four modes live in the same binary and
// the same OCI image -- which, since internal/netshim talks to the
// kernel directly via netlink/nftables rather than exec'ing external
// CLIs, ships from "scratch" with no shell or coreutils of its own, so
// "sleep" exists here rather than relying on one being present in the
// image -- invoked with different args per role. See README.md for the
// environment variables each mode understands.
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
	"github.com/bwsalmon/kontur/internal/guestexec"
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
	case "exec":
		log.SetPrefix("kontur: exec: ")
		if err := runExec(os.Args[2:]); err != nil {
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
		log.Fatalf("kontur: unknown mode %q (want \"run\", \"netshim\", \"exec\" or \"sleep\")", mode)
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

// runExec connects to the VM guest over SSH and runs args as a command
// there (or, given none, an interactive login shell), proxying
// stdin/stdout/stderr and, for a TTY, window resizes -- see
// internal/guestexec. Meant to be invoked as `kubectl exec`'s own
// command (e.g. `kubectl exec -it <pod> -c <container> -- kontur exec --
// <command>`), since the container itself ships no shell of its own to
// exec into (see the "final" stage of the Dockerfile).
//
// Unlike runVM/runNetshim, it exits directly with the remote command's
// own exit code on success (mirroring runVM's own handling of
// cloud-hypervisor's exit code) rather than returning nil, since 0 is
// itself a meaningful, common outcome that the caller (main) would
// otherwise have no way to distinguish from "never ran".
func runExec(args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}

	cfg, err := guestexec.FromEnv()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		if sig, ok := <-sigCh; ok {
			log.Printf("received %s, closing guest session", sig)
			cancel()
		}
	}()

	code, err := guestexec.Run(ctx, cfg, args, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
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
