// Command kontur is the container-facing binary for kontur: it either
// boots a single cloud-hypervisor VM as PID 1 of a container ("run", the
// default), sets up pod-local networking as a Kubernetes init container
// ("netshim"), blocks forever ("sleep", used by -backend docker to hold
// a network namespace open in place of a pod sandbox -- see
// internal/dockervm), runs a command inside the VM guest over SSH
// ("exec", see internal/guestexec), or live-resizes an already-running
// VM's memory and/or vCPU count via cloud-hypervisor's API ("resize",
// see internal/hypervisor's APIClient.Resize/ResizeCPUs) -- the latter
// two meant to be invoked as `kubectl exec`'s own command, so they
// reach the guest/VMM of the already-running container rather than this
// otherwise-empty one.
// All five modes live in the same binary and the same OCI image -- which,
// since internal/netshim talks to the kernel directly via netlink/nftables
// rather than exec'ing external CLIs, ships from "scratch" with no shell
// or coreutils of its own, so "sleep" exists here rather than relying on
// one being present in the image -- invoked with different args per role.
// See README.md for the environment variables each mode understands.
//
// The same binary also answers to "sh" and "bash": the Dockerfile's
// "final" stage symlinks /bin/sh and /bin/bash (and their /usr/bin
// equivalents) to it, so a plain `kubectl exec -it <pod> -- sh` (or
// "docker exec -it <container> bash") resolves to this binary too, and
// forwards into the guest the same way "exec" mode above does --
// docker/kubectl's own "exec" machinery always runs a command already
// present in the container by name, never through the mode dispatch
// below, so this is the only way for a plain sh/bash invocation (as
// opposed to one that already knows to run "kontur exec") to end up
// reaching the guest at all. main tells these two apart from the five
// real modes by argv[0] rather than an argument, since that's the only
// thing docker/kubectl's own exec machinery lets a container control
// about how it's invoked. See ShellCommandLine for exactly which
// "sh"/"bash" invocations this supports.
//
// This is distinct from cmd/konturctl, which is the operator-facing CLI
// that runs on the node itself (not inside a container) to manage those
// pods.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bwsalmon/kontur/internal/config"
	"github.com/bwsalmon/kontur/internal/guestexec"
	"github.com/bwsalmon/kontur/internal/guestkey"
	"github.com/bwsalmon/kontur/internal/hypervisor"
	"github.com/bwsalmon/kontur/internal/memagent"
	"github.com/bwsalmon/kontur/internal/netshim"
)

func main() {
	log.SetFlags(0)

	argv0 := filepath.Base(os.Args[0])
	mode := "run"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if argv0 == "sh" || argv0 == "bash" {
		mode = "shell"
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
	case "resize":
		log.SetPrefix("kontur: resize: ")
		if err := runResize(os.Args[2:]); err != nil {
			log.Fatalf("%v", err)
		}
	case "shell":
		log.SetPrefix("kontur: " + argv0 + ": ")
		if err := runShell(os.Args[1:]); err != nil {
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
		log.Fatalf("kontur: unknown mode %q (want \"run\", \"netshim\", \"exec\", \"resize\" or \"sleep\")", mode)
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

	// Before anything boots: in the default disk mode the guest writes
	// into a qcow2 of its own rather than into the image, and this is
	// what creates it and repoints the VM at it. See config.PrepareOverlay.
	if err := cfg.PrepareOverlay(); err != nil {
		return err
	}

	if err := ensureGuestKey(&cfg, guestexec.DefaultKeyPath); err != nil {
		return err
	}

	if netshim.FlatEnabled() {
		if err := applyFlatNet(&cfg); err != nil {
			return err
		}
	}

	runner := hypervisor.New(cfg)
	if err := runner.Start(os.Stdout, os.Stderr); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("received %s, shutting down", sig)
		cancel()
		runner.Shutdown(context.Background())
	}()

	if cfg.MemAgent {
		startMemAgent(ctx, cfg)
	}

	if cfg.SetupScript != "" {
		if runner.Restored() {
			log.Printf("resumed from suspended state at %s, skipping setup script", cfg.SnapshotPath)
		} else if err := runSetupScript(ctx, cfg, runner); err != nil {
			runner.Shutdown(context.Background())
			return err
		}
	}

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

// ensureGuestKey makes sure a keypair "kontur exec" can authenticate with
// exists for the guest this run is about to boot, and that the guest will
// authorize it.
//
// On a fresh boot that means generating one and appending its public half
// to the kernel command line, which the guest installs before starting
// sshd (deploy/guest-image's kontur-authorized-key). The key lives only
// in this container, only for this guest's boot -- see internal/guestkey
// for why that replaced a keypair baked into the image.
//
// A restore is the exception, and it is not a small one. BuildArgs passes
// no command line at all when resuming from a snapshot: cloud-hypervisor
// replays the entire machine from the snapshot's own config.json, and the
// guest never boots a kernel, so nothing would read a freshly generated
// key. Its authorized_keys still holds whatever the boot that was
// suspended installed. Generating a new key here would therefore not
// "rotate" anything -- it would lock this container out of the guest it
// just resumed. So a restore reuses that boot's key instead.
//
// Which is why the key is also saved beside the snapshot whenever one is
// configured: the container's own writable layer does not survive being
// recreated, and a snapshot exists precisely to be resumed by a later
// container than the one that took it.
func ensureGuestKey(cfg *config.Config, keyPath string) error {
	saved := savedKeyPath(cfg.SnapshotPath)

	if hypervisor.SnapshotExists(cfg.SnapshotPath) {
		if _, err := os.Stat(keyPath); err == nil {
			return nil
		}
		if err := copyKey(saved, keyPath); err != nil {
			return fmt.Errorf("recovering the resumed guest's exec key from %s: %w", saved, err)
		}
		log.Printf("resuming from %s: reusing the exec key that boot installed", cfg.SnapshotPath)
		return nil
	}

	authorized, err := guestkey.Generate(keyPath)
	if err != nil {
		return fmt.Errorf("generating this boot's guest exec key: %w", err)
	}
	cfg.Cmdline = guestkey.WithParams(cfg.Cmdline, authorized, guestexec.UserFromEnv())

	if saved != "" {
		if err := copyKey(keyPath, saved); err != nil {
			return fmt.Errorf("saving the exec key for a later resume: %w", err)
		}
	}
	return nil
}

// savedKeyPath returns where this VM's exec key is kept so that a
// container recreated around the same snapshot can still reach the guest,
// or "" when no snapshot is configured and the question cannot arise.
func savedKeyPath(snapshotPath string) string {
	if snapshotPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(snapshotPath), "exec_id_ed25519")
}

func copyKey(src, dst string) error {
	if src == "" {
		return fmt.Errorf("no snapshot directory to read it from")
	}
	key, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, key, 0o600)
}

// startMemAgent starts internal/memagent's listener in the background so
// the guest-side agent baked into the disk image (see
// deploy/guest-image's kontur-mem-agent) can ask this already-running VM
// to grow its own memory when it observes pressure -- the automatic
// counterpart to an operator's manual "kontur resize". It talks to
// cloud-hypervisor over its own APIClient rather than reusing runner's
// (unexported) one: both just dial the same api socket, so there's no
// shared state to coordinate. Runs until ctx is cancelled (i.e. until
// runVM's own shutdown signal handling fires); errors are logged rather
// than fatal, since a guest that can't ask for more memory should still
// boot and run at whatever it already has.
func startMemAgent(ctx context.Context, cfg config.Config) {
	server := memagent.New(memagent.Config{
		Addr:     cfg.MemAgentAddr,
		StartMB:  cfg.MemoryMB,
		MaxMB:    cfg.MemoryMaxMB,
		StepMB:   cfg.MemAgentStepMB,
		Cooldown: cfg.MemAgentCooldown,
	}, hypervisor.NewAPIClient(cfg.APISocket))

	go func() {
		if err := server.Serve(ctx); err != nil && ctx.Err() == nil {
			log.Printf("mem-agent listener: %v", err)
		}
	}()
}

// runSetupScript runs cfg.SetupScript once inside the guest over SSH
// (see internal/guestexec, the same machinery "kontur exec" uses below).
// If cfg.SnapshotPath is set, it then -- once the script exits zero --
// suspends the VM to it (see hypervisor.Runner.Suspend) so a future
// kontur run resumes this exact post-setup state instead of booting
// fresh and re-running the script; left unset, the script will simply
// run again on the next fresh boot. Only called for a freshly booted
// VM: runVM skips this when runner.Restored() is already true.
func runSetupScript(ctx context.Context, cfg config.Config, runner *hypervisor.Runner) error {
	gcfg, err := guestexec.FromEnv()
	if err != nil {
		return fmt.Errorf("configuring guest exec for the setup script: %w", err)
	}

	log.Printf("running setup script in guest")
	code, err := guestexec.RunLine(ctx, gcfg, cfg.SetupScript, nil, os.Stdout, os.Stderr)
	if err != nil {
		return fmt.Errorf("running setup script: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("setup script exited with status %d", code)
	}

	if cfg.SnapshotPath == "" {
		log.Printf("setup script complete")
		return nil
	}

	log.Printf("setup script complete, suspending to %s", cfg.SnapshotPath)
	if err := runner.Suspend(ctx); err != nil {
		return fmt.Errorf("suspending vm: %w", err)
	}
	log.Printf("suspended: future runs will resume from this state instead of rerunning the setup script")
	return nil
}

// runExec connects to the VM guest over SSH and runs args as a command
// there (or, given none, an interactive login shell), proxying
// stdin/stdout/stderr and, for a TTY, window resizes -- see
// internal/guestexec. Meant to be invoked as `kubectl exec`'s own
// command (e.g. `kubectl exec -it <pod> -c <container> -- kontur exec --
// <command>`), since the container itself ships no shell of its own to
// exec into (see the "final" stage of the Dockerfile).
func runExec(args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}

	return runGuestSession(func(ctx context.Context, cfg guestexec.Config) (int, error) {
		return guestexec.Run(ctx, cfg, args, os.Stdin, os.Stdout, os.Stderr)
	})
}

// runResize live-resizes this container's own already-running VM's
// guest memory and/or vCPU count via the cloud-hypervisor API (see
// internal/hypervisor's APIClient.Resize/ResizeCPUs), within the ranges
// CHV_MEMORY_MB..CHV_MEMORY_MAX_MB and CHV_CPUS..CHV_CPUS_MAX that were
// configured at boot (see internal/config) -- it does not, and cannot,
// change those ranges itself. Meant to be invoked the same way "exec"
// is, e.g. `kubectl exec <pod> -c <container> -- kontur resize
// -memory-mb=1024` or `-cpus=4` (either or both may be given in one
// call), since reaching this container's API socket from outside it has
// no other path. Requires CHV_MEMORY_HOTPLUG (for -memory-mb) or a
// CHV_CPUS_MAX above CHV_CPUS (for -cpus) to have been set at boot;
// cloud-hypervisor rejects the corresponding resize otherwise. A -cpus
// value below the current count asks cloud-hypervisor to remove vCPUs,
// which only completes once the guest acknowledges the removal -- a
// second -cpus call made before that finishes fails with a 429 from
// cloud-hypervisor ("a cpu removal is still pending"); see
// APIClient.ResizeCPUs and the README's "CPU hotplug" section.
func runResize(args []string) error {
	fs := flag.NewFlagSet("kontur resize", flag.ContinueOnError)
	memoryMB := fs.Int("memory-mb", 0, "desired guest memory size, in MiB")
	cpus := fs.Int("cpus", 0, "desired vCPU count")
	timeout := fs.Duration("timeout", 10*time.Second, "how long to wait for cloud-hypervisor's API to respond")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *memoryMB <= 0 && *cpus <= 0 {
		return fmt.Errorf("at least one of -memory-mb or -cpus is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	api := hypervisor.NewAPIClient(config.APISocket())
	if *memoryMB > 0 {
		if err := api.Resize(ctx, uint64(*memoryMB)*1024*1024); err != nil {
			return fmt.Errorf("resizing memory: %w", err)
		}
		log.Printf("requested resize to %d MiB", *memoryMB)
	}
	if *cpus > 0 {
		if err := api.ResizeCPUs(ctx, uint32(*cpus)); err != nil {
			return fmt.Errorf("resizing cpus: %w", err)
		}
		log.Printf("requested resize to %d vCPU(s)", *cpus)
	}
	return nil
}

// runShell implements the "sh"/"bash" shim described in this file's own
// doc comment: args is os.Args[1:] exactly as a real sh/bash invoked the
// same way would have received it. See guestexec.ShellCommandLine for
// which invocations are actually supported.
func runShell(args []string) error {
	line, err := guestexec.ShellCommandLine(args)
	if err != nil {
		return err
	}

	return runGuestSession(func(ctx context.Context, cfg guestexec.Config) (int, error) {
		return guestexec.RunLine(ctx, cfg, line, os.Stdin, os.Stdout, os.Stderr)
	})
}

// runGuestSession is the shared machinery behind runExec and runShell:
// it loads guestexec's config, wires SIGTERM/SIGINT into cancelling the
// session the same way runVM turns them into a VM shutdown, runs the
// guest session via runSession, and exits directly with its exit code on
// success (mirroring runVM's own handling of cloud-hypervisor's exit
// code) rather than returning nil, since 0 is itself a meaningful,
// common outcome the caller (main) would otherwise have no way to
// distinguish from "never ran".
func runGuestSession(runSession func(ctx context.Context, cfg guestexec.Config) (int, error)) error {
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

	code, err := runSession(ctx, cfg)
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

	if cfg.Mode == netshim.ModeFlat {
		if err := netshim.SetupFlat(cfg); err != nil {
			return err
		}
		log.Printf("network shim ready: %s spliced onto %s", cfg.VMs[0].TapName(), cfg.ExternalIface)
		return nil
	}

	if err := netshim.Setup(cfg); err != nil {
		return err
	}

	log.Printf("network shim ready: bridge %s (%s), %d VM(s)", cfg.Bridge, cfg.BridgeNet, len(cfg.VMs))
	return nil
}

// applyFlatNet fills in the guest configuration a flat-mode VM cannot be
// told in advance: the address, MAC and MTU the container runtime chose
// for this network namespace, which are only knowable once the sandbox
// exists.
//
// It is deliberately done here, from inside the namespace, rather than
// computed by whoever created the sandbox and passed down as environment.
// Reading them back off the external interface works identically whether
// the sandbox came from "docker run", a kubelet, or something else, so
// the same image behaves the same way under all of them -- and there is
// no second copy of the identity to drift out of date.
//
// This relies on netshim having left that interface addressed, which
// SetupFlat documents it does.
func applyFlatNet(cfg *config.Config) error {
	ncfg, err := netshim.FromEnv()
	if err != nil {
		return err
	}
	id, err := netshim.DiscoverIdentity(ncfg.ExternalIface)
	if err != nil {
		return err
	}

	guest := netshim.FlatGuestConfig(ncfg, id)
	cfg.Nets = guest.Nets
	cfg.Cmdline = netshim.WithIPParam(cfg.Cmdline, guest.IPParam)

	log.Printf("flat mode: guest takes over %s as %s (mac %s, mtu %d)",
		id.Iface, id.IP, id.MAC, id.MTU)
	if guest.ControlIP != "" {
		log.Printf("flat mode: control link expects the guest at %s on its second NIC", guest.ControlIP)
	}
	return nil
}
