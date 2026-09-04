package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/bwsalmon/kontur/internal/dockervm"
	"github.com/bwsalmon/kontur/internal/staticpod"
)

const defaultStateDir = "/var/lib/kontur/vms"

// ensureStateDir creates stateDir if it isn't there and proves konturctl
// can write into it, so a directory that can't hold a VM's saved state is
// refused before anything is started.
//
// This used to be left to staticpod.Save, which runs *after* the backend
// has submitted the pod or started the containers -- so the default
// /var/lib/kontur/vms, which an unprivileged user cannot create, failed
// with a VM already running and nothing saved to find it by again. Every
// caller who isn't root hits that, which is why every flow in the
// top-level README passes -state-dir.
//
// MkdirAll succeeding is not enough on its own: an existing directory
// owned by someone else is created "successfully" and still cannot be
// written to, hence the probe file.
func ensureStateDir(stateDir string) error {
	const hint = "pass -state-dir to put it somewhere writable (e.g. -state-dir ~/.kontur/vms)"
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("state directory %s cannot be created (%w); %s", stateDir, err, hint)
	}
	probe, err := os.CreateTemp(stateDir, ".konturctl-writable-")
	if err != nil {
		return fmt.Errorf("state directory %s is not writable (%w); %s", stateDir, err, hint)
	}
	probe.Close()
	return os.Remove(probe.Name())
}

// stringList collects a flag that may be repeated, in order. Used for
// -docker-run-opt, where each option and each of its values is passed
// separately so no quoting or splitting convention has to be invented for
// values that may themselves contain spaces or commas.
//
// The first Set clears whatever the list was seeded with, so that passing
// the flag at all replaces the saved value wholesale rather than
// appending to it. That keeps "konturctl vm update" behaving the same way
// for this flag as for every other one -- give it and it replaces, omit
// it and the saved value is kept -- instead of silently accumulating a
// longer list on each update.
type stringList struct {
	values   []string
	replaced bool
}

func (l *stringList) String() string {
	if l == nil {
		return ""
	}
	return strings.Join(l.values, " ")
}

func (l *stringList) Set(v string) error {
	if !l.replaced {
		l.values = nil
		l.replaced = true
	}
	l.values = append(l.values, v)
	return nil
}

func runVM(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("vm: expected a subcommand (create, run, exec, shell, wait, status, update, delete, list)")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		return runVMCreate(ctx, rest, stdout, stderr)
	case "run":
		return runVMRun(ctx, rest, stdin, stdout, stderr)
	case "exec":
		return runVMExec(ctx, rest, stdin, stdout, stderr)
	case "shell":
		return runVMShell(ctx, rest, stdin, stdout, stderr)
	case "wait":
		return runVMWait(ctx, rest, stdout, stderr)
	case "status":
		return runVMStatus(ctx, rest, stdout, stderr)
	case "update":
		return runVMUpdate(ctx, rest, stdout, stderr)
	case "delete":
		return runVMDelete(ctx, rest, stdout, stderr)
	case "list":
		return runVMList(rest, stdout, stderr)
	default:
		return fmt.Errorf("vm: unknown subcommand %q", cmd)
	}
}

// vmFlags binds every VMSpec field (other than Name) to a flag, and the
// resulting values can be read back out via toSpec once Parse has run.
type vmFlags struct {
	// fs is the set these flags were registered on, kept so toSpec can
	// ask which of them the caller actually passed (see
	// resolvedDiskMode).
	fs              *flag.FlagSet
	diskImage       *string
	diskReadOnly    *bool
	diskMode        *string
	diskSizeMB      *int
	kernel          *string
	initramfs       *string
	firmware        *string
	cmdline         *string
	cpus            *int
	memoryMB        *int
	cpusMax         *int
	memoryMaxMB     *int
	shutdownTimeout *string
	guestUser       *string
	// ip, port, guestPort, bridgeCIDR and netMode are the NAT mode's
	// settings, kept only to be accepted and ignored -- see
	// registerVMFlags.
	ip                            *string
	port                          *int
	guestPort                     *int
	bridgeCIDR                    *string
	netMode                       *string
	bridge                        *string
	externalIface                 *string
	dns                           *string
	controlCIDR                   *string
	dockerRunOpts                 stringList
	imagesHostPath                *string
	diskHostPath                  *string
	konturImage                   *string
	terminationGracePeriodSeconds *int
	staticPodPath                 *string
}

// registerVMFlags registers one flag per VMSpec field on fs, defaulting
// each to the matching field of d. Passing staticpod.Defaults() gives the
// usual create-time defaults; passing a previously saved spec makes every
// flag default to "keep the current value", which is what makes "konturctl
// vm update" a partial update instead of requiring every flag to be
// repeated.
func registerVMFlags(fs *flag.FlagSet, d staticpod.VMSpec) *vmFlags {
	v := &vmFlags{fs: fs}
	v.diskImage = fs.String("disk", d.DiskImage, "path to the VM's disk image under -images-hostpath, as seen inside the kontur container (e.g. /images/disk.img)")
	v.diskMode = fs.String("disk-mode", d.DiskMode, `how the VM attaches its disk: "overlay" (the default -- the guest writes into a thin qcow2 of its own, created in its container, leaving the image untouched and shared), "persistent" (the guest writes through to the image itself) or "readonly"`)
	v.diskReadOnly = fs.Bool("disk-readonly", d.DiskReadOnly, "deprecated, use -disk-mode: true means -disk-mode=readonly, false means -disk-mode=overlay (which is what it always did: a private writable disk per VM)")
	v.diskSizeMB = fs.Int("disk-size-mb", d.DiskSizeMB, "size of the VM's own writable overlay, in MiB; 0 means the disk image's own size, and a larger value grows an overlay an earlier boot already created (-disk-mode=overlay only; the bundled guest grows its filesystem onto the space at boot, a guest of your own has to do that itself)")
	v.kernel = fs.String("kernel", d.Kernel, "path to a kernel for direct boot, as seen inside the container (mutually exclusive with -firmware)")
	v.initramfs = fs.String("initramfs", d.Initramfs, "path to an initramfs, used with -kernel")
	v.firmware = fs.String("firmware", d.Firmware, "path to firmware for firmware boot, as seen inside the container (mutually exclusive with -kernel)")
	v.cmdline = fs.String("cmdline", d.Cmdline, "kernel command line, used with -kernel (default: derived from -disk-mode; the guest's own ip= is appended at boot from the address the container runtime assigned)")
	v.cpus = fs.Int("cpus", d.CPUs, "boot vCPU count")
	v.memoryMB = fs.Int("memory-mb", d.MemoryMB, "guest memory, in MiB")
	v.cpusMax = fs.Int("cpus-max", d.CPUsMax, "vCPU ceiling \"kontur resize\" may grow this VM to; 0 means no headroom, and the count is fixed at -cpus for the VM's whole life")
	v.memoryMaxMB = fs.Int("memory-max-mb", d.MemoryMaxMB, "memory ceiling \"kontur resize\" may grow this VM to, in MiB; 0 means no headroom beyond the VM container's own default")
	v.shutdownTimeout = fs.String("shutdown-timeout", d.ShutdownTimeout, "how long to wait for a graceful guest shutdown before forcing it (Go duration syntax)")
	v.guestUser = fs.String("guest-user", d.GuestUser, "guest account, besides root, that \"kontur exec\" logs in as: the guest authorizes this boot's generated key for it too (empty means root)")
	v.bridge = fs.String("bridge", d.Bridge, "name of the in-pod bridge holding the control link's host end")
	v.externalIface = fs.String("external-iface", d.ExternalIface, "the pod's primary interface, whose address and MAC the guest takes over")
	v.dns = fs.String("dns", d.DNS, "nameserver(s) the guest resolves through, comma separated, at most two: they travel on the guest's ip= boot parameter and become its /etc/resolv.conf; empty leaves whatever the guest image ships with")
	v.controlCIDR = fs.String("control-cidr", d.ControlCIDR, "address netshim holds on the control link, the private second NIC that keeps \"kontur exec\" and the memory agent able to reach the guest; empty disables it")
	// The NAT mode these configured is gone: a VM now takes over the
	// address the container runtime assigned its sandbox, so there is no
	// private subnet to place it on and no external port to forward (see
	// the top-level README's "Container networking"). Accepted and
	// ignored rather than removed, the same way -disk-hostpath is: a
	// deployment's own scripts pass them, and failing their "vm create"
	// outright over flags that now have nothing to configure would break
	// them for no gain.
	v.ip = fs.String("ip", "", "deprecated and ignored: the container runtime assigns the address the guest takes over")
	v.port = fs.Int("port", 0, "deprecated and ignored: ports are published on the sandbox itself (-docker-run-opt -p ...), not forwarded per VM")
	v.guestPort = fs.Int("guest-port", 0, "deprecated and ignored: nothing forwards to a fixed in-guest port any more")
	v.bridgeCIDR = fs.String("bridge-cidr", "", "deprecated and ignored: use -control-cidr, the only subnet netshim still creates")
	// -net is the one that cannot simply be ignored: "flat" is what
	// every VM is now, but silently accepting "nat" would hand back a
	// guest on a different network than the caller asked for.
	v.netMode = fs.String("net", "", "deprecated: only the former \"flat\" behaviour exists now, and it is the default")
	v.dockerRunOpts.values = append([]string(nil), d.DockerRunOpts...)
	fs.Var(&v.dockerRunOpts, "docker-run-opt", "extra option passed verbatim to the \"docker run\" creating the network namespace holder, repeatable (e.g. -docker-run-opt -p -docker-run-opt 8080:80); -backend docker only")
	v.imagesHostPath = fs.String("images-hostpath", d.ImagesHostPath, "host directory mounted read-only at /images in the kontur container")
	// Accepted and ignored rather than removed: a deployment's own
	// scripts pass it, and failing their "vm create" outright over a flag
	// that now has nothing to configure would break them for no gain.
	v.diskHostPath = fs.String("disk-hostpath", d.DiskHostPath, "deprecated and ignored: each VM's writable overlay is created inside its own container now, so there is no host directory to place it in")
	v.konturImage = fs.String("kontur-image", d.KonturImage, "kontur image reference, used for both the netshim init container and the VM container; the default follows -backend (the locally built \"kontur:latest\" under docker, the node-local registry the standalone kubelet pulls from under static-pod)")
	v.terminationGracePeriodSeconds = fs.Int("termination-grace-period", d.TerminationGracePeriodSeconds, "pod terminationGracePeriodSeconds; must comfortably exceed -shutdown-timeout")
	v.staticPodPath = fs.String("static-pod-path", d.StaticPodPath, "directory the standalone kubelet watches for static pod manifests")
	return v
}

// wasSet reports whether the caller actually passed a flag, as opposed to
// leaving it at whatever default registerVMFlags gave it.
func (v *vmFlags) wasSet(name string) bool {
	set := false
	v.fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// diskMode returns the disk mode to save, reconciling -disk-mode with the
// deprecated -disk-readonly.
//
// -disk-mode now defaults to a real mode ("overlay") rather than to
// "derive it from -disk-readonly", and an explicit mode always wins over
// a derived one (staticpod.VMSpec.DiskModeOrDerived), so a caller passing
// only -disk-readonly would otherwise have it silently overridden by that
// default. Returning "" when -disk-readonly is the flag that was passed
// hands the decision back to the derivation, which is what the deprecated
// flag has always meant.
func (v *vmFlags) resolvedDiskMode() string {
	if !v.wasSet("disk-mode") && v.wasSet("disk-readonly") {
		return ""
	}
	return *v.diskMode
}

func (v *vmFlags) toSpec(name string) staticpod.VMSpec {
	return staticpod.VMSpec{
		Name:                          name,
		DiskImage:                     *v.diskImage,
		DiskReadOnly:                  *v.diskReadOnly,
		DiskMode:                      v.resolvedDiskMode(),
		DiskSizeMB:                    *v.diskSizeMB,
		Kernel:                        *v.kernel,
		Initramfs:                     *v.initramfs,
		Firmware:                      *v.firmware,
		Cmdline:                       *v.cmdline,
		CPUs:                          *v.cpus,
		MemoryMB:                      *v.memoryMB,
		CPUsMax:                       *v.cpusMax,
		MemoryMaxMB:                   *v.memoryMaxMB,
		ShutdownTimeout:               *v.shutdownTimeout,
		GuestUser:                     *v.guestUser,
		Bridge:                        *v.bridge,
		ExternalIface:                 *v.externalIface,
		DNS:                           *v.dns,
		ControlCIDR:                   *v.controlCIDR,
		DockerRunOpts:                 v.dockerRunOpts.values,
		ImagesHostPath:                *v.imagesHostPath,
		DiskHostPath:                  *v.diskHostPath,
		KonturImage:                   *v.konturImage,
		TerminationGracePeriodSeconds: *v.terminationGracePeriodSeconds,
		StaticPodPath:                 *v.staticPodPath,
	}
}

// checkDeprecated rejects the one deprecated flag that cannot just be
// ignored. -ip/-port/-guest-port/-bridge-cidr described a network that no
// longer exists, and ignoring them leaves the caller with a working VM;
// -net nat asks for a mode that is gone, and ignoring that would hand
// back a guest reachable somewhere else entirely, so say so instead.
func (v *vmFlags) checkDeprecated() error {
	switch *v.netMode {
	case "", "flat":
		return nil
	default:
		return fmt.Errorf("-net %q is no longer supported: a VM always takes over the address the container runtime assigned its sandbox (what -net flat used to select); drop the flag", *v.netMode)
	}
}

// helpName stands in for the VM name when the caller asked for the
// command's flags instead of naming a VM ("konturctl vm create -h").
// The help request stays in the arguments, so the command goes on to
// build its own flag set and let the flag package print it -- which is
// the answer that was asked for, and one that names the flags this
// particular subcommand takes rather than a generic synopsis.
const helpName = "<name>"

// splitNameAndFlags takes the VM name off the front of a subcommand's
// arguments, where every "konturctl vm <verb> <name> [flags]" puts it.
//
// A first argument that looks like a flag is not a name. It used to be
// taken as one anyway: "konturctl vm create -h" tried to create a VM
// literally called "-h" and failed on whatever the spec was missing,
// while "konturctl vm create -state-dir /tmp/vms" created a VM called
// "-state-dir" in the default state directory and reported success. The
// help forms get the flags they were after (see helpName); anything else
// is refused here, where the argument that is actually wrong can still
// be named.
func splitNameAndFlags(args []string, cmd string) (name string, rest []string, err error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("%s: a VM name is required (konturctl %s <name> [flags])", cmd, cmd)
	}
	if first := args[0]; strings.HasPrefix(first, "-") {
		if isHelpFlag(first) {
			return helpName, args, nil
		}
		return "", nil, fmt.Errorf("%s: a VM name comes before the flags, and the first argument is %q (konturctl %s <name> [flags]; \"konturctl %s -h\" lists them)", cmd, first, cmd, cmd)
	}
	return args[0], args[1:], nil
}

// isHelpFlag reports whether arg is one of the spellings the flag
// package itself answers with a usage dump.
func isHelpFlag(arg string) bool {
	switch arg {
	case "-h", "--h", "-help", "--help":
		return true
	}
	return false
}

// peekFlag scans args by hand for a named flag, in any of flag's accepted
// forms ("-name value" or "-name=value"), without registering or
// consuming any other flag. Returns fallback if the flag isn't there.
//
// Two flags have to be read before the real parse can happen: -state-dir,
// because which saved spec seeds "vm update"'s defaults depends on it,
// and -backend, because which image default registerVMFlags is given
// depends on it. flag.FlagSet has no way to parse a subset of its flags
// and ignore the rest, hence doing it by hand.
func peekFlag(args []string, name, fallback string) string {
	for i, a := range args {
		switch {
		case a == "-"+name || a == "--"+name:
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "-"+name+"="):
			return strings.TrimPrefix(a, "-"+name+"=")
		case strings.HasPrefix(a, "--"+name+"="):
			return strings.TrimPrefix(a, "--"+name+"=")
		}
	}
	return fallback
}

// peekStateDir is peekFlag for -state-dir, defaulting to the same
// directory the flag itself does.
func peekStateDir(args []string) string {
	return peekFlag(args, "state-dir", defaultStateDir)
}

func runVMCreate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	name, rest, err := splitNameAndFlags(args, "vm create")
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("konturctl vm create "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", defaultStateDir, "directory kontur stores VM state in")
	backend := fs.String("backend", staticpod.BackendStaticPod, `how to run this VM: "static-pod" (write a manifest for the standalone kubelet to run) or "docker" (run directly against the local docker daemon)`)
	wait := fs.Bool("wait", false, "don't return until the VM's guest answers a command, the way \"konturctl vm wait\" does; without it the VM is still booting when this returns (-backend docker only)")
	readyTimeout := fs.Duration("ready-timeout", defaultReadyTimeout, "how long -wait gives the guest to become reachable")
	// The backend decides one of the defaults registerVMFlags hands out
	// (-kontur-image), and it is a flag on this same set, so it has to be
	// read out of args before the parse -- the same trick "vm update"
	// plays with -state-dir. Whatever is read here is only a default; the
	// parse below is still what sets *backend.
	v := registerVMFlags(fs, staticpod.DefaultsForBackend(peekFlag(rest, "backend", staticpod.BackendStaticPod)))
	if err := fs.Parse(rest); err != nil {
		return err
	}

	if _, err := staticpod.Load(*stateDir, name); err == nil {
		return fmt.Errorf("VM %q already exists (use \"konturctl vm update\" to change it)", name)
	}

	if err := v.checkDeprecated(); err != nil {
		return err
	}
	// Refused here rather than after the VM is up: a -wait konturctl
	// cannot honour is a flag the caller has to take back out, and
	// saying so before anything is created leaves nothing to clean up.
	if *wait && *backend != staticpod.BackendDocker {
		return errNoWaitForBackend(name, *backend)
	}

	spec := v.toSpec(name)
	spec.Backend = *backend
	if err := submitVM(ctx, spec, *stateDir, stdout); err != nil {
		return err
	}
	if !*wait {
		return nil
	}
	// The VM is left in place if the guest never answers: it was asked
	// for as a VM that outlives this command, and "docker logs
	// kontur-vm-<name>" is how to find out why it didn't come up.
	// ("konturctl vm run", whose VM exists only for one command, deletes
	// its own instead -- see -keep-on-failure there.)
	return waitForGuest(ctx, name, spec.Backend, *readyTimeout, stdout)
}

func runVMUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	name, rest, err := splitNameAndFlags(args, "vm update")
	if err != nil {
		return err
	}

	// registerVMFlags needs the existing spec to seed its defaults (so
	// omitted flags keep their current value), but which spec to load
	// depends on -state-dir, which is itself one of those flags. Find it
	// by hand before doing the real parse below, since flag.FlagSet has
	// no way to parse a subset of its flags and ignore the rest.
	// A help request names no VM, so there is no saved spec to seed the
	// flag defaults from and nothing to look up: the plain defaults are
	// what the listing below shows.
	existing := staticpod.Defaults()
	if name != helpName {
		if existing, err = staticpod.Load(peekStateDir(rest), name); err != nil {
			return fmt.Errorf("VM %q not found (use \"konturctl vm create\" first): %w", name, err)
		}
	}
	if existing.CmdlineAuto {
		existing.Cmdline = ""
	}

	fs := flag.NewFlagSet("konturctl vm update "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", defaultStateDir, "directory kontur stores VM state in")
	v := registerVMFlags(fs, existing)
	if err := fs.Parse(rest); err != nil {
		return err
	}

	if err := v.checkDeprecated(); err != nil {
		return err
	}
	// Checked here as well as in submitVM: an update tears the existing
	// VM's containers down first, and doing that only to fail on saving
	// the new spec would leave the caller with neither the old VM nor the
	// new one.
	if err := ensureStateDir(*stateDir); err != nil {
		return err
	}

	spec := v.toSpec(name)
	// The backend a VM runs under isn't one of registerVMFlags' fields --
	// "vm update" always keeps whatever "vm create" chose, since there's
	// no sensible way to migrate a running VM from one to the other.
	spec.Backend = existing.BackendOrDefault()

	switch spec.Backend {
	case staticpod.BackendDocker:
		if err := dockervm.Delete(ctx, &dockervm.Docker{}, name, existing.TerminationGracePeriodSeconds, stdout); err != nil {
			return fmt.Errorf("removing old containers before update: %w", err)
		}
	default:
		oldManifest := filepath.Join(existing.StaticPodPath, staticpod.ManifestFileName(name))
		if spec.StaticPodPath != existing.StaticPodPath {
			if rmErr := os.Remove(oldManifest); rmErr != nil && !os.IsNotExist(rmErr) {
				return fmt.Errorf("removing old manifest %s: %w", oldManifest, rmErr)
			}
		}
	}
	return submitVM(ctx, spec, *stateDir, stdout)
}

// submitVM validates spec, then hands it off to the backend it names
// (staticpod.BackendStaticPod renders a manifest into spec.StaticPodPath
// for the standalone kubelet; staticpod.BackendDocker runs the equivalent
// containers directly via dockervm), and saves spec so a later
// update/delete can find it again.
func submitVM(ctx context.Context, spec staticpod.VMSpec, stateDir string, stdout io.Writer) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	// Before the backend does anything: a VM whose state cannot be saved
	// is a VM nothing can find again to update or delete (see
	// ensureStateDir).
	if err := ensureStateDir(stateDir); err != nil {
		return err
	}
	switch spec.Backend {
	case staticpod.BackendDocker:
		if err := dockervm.Create(ctx, &dockervm.Docker{}, spec, stdout); err != nil {
			return err
		}
	default:
		manifest, err := staticpod.Render(spec)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(spec.StaticPodPath, 0o755); err != nil {
			return fmt.Errorf("creating static pod path %s: %w", spec.StaticPodPath, err)
		}
		path := filepath.Join(spec.StaticPodPath, staticpod.ManifestFileName(spec.Name))
		if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
			return fmt.Errorf("writing manifest %s: %w", path, err)
		}
		fmt.Fprintf(stdout, "wrote %s\n", path)
	}

	if err := staticpod.Save(stateDir, spec); err != nil {
		return fmt.Errorf("saving VM state: %w", err)
	}
	return nil
}

func runVMDelete(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	name, rest, err := splitNameAndFlags(args, "vm delete")
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("konturctl vm delete "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", defaultStateDir, "directory kontur stores VM state in")
	staticPodPath := fs.String("static-pod-path", "", "override the manifest directory to remove from, if the VM's saved state can't be found (ignored for -backend docker)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	return deleteVM(ctx, name, *stateDir, *staticPodPath, stdout)
}

// deleteVM removes a VM's containers (or its static pod manifest) and its
// saved state, whichever backend created it. Split out of runVMDelete so
// "vm run" can tear down the VM it created without going back through
// flag parsing.
func deleteVM(ctx context.Context, name, stateDir, staticPodPath string, stdout io.Writer) error {
	backend := staticpod.BackendStaticPod
	gracePeriod := staticpod.Defaults().TerminationGracePeriodSeconds
	podDir := staticPodPath
	existing, err := staticpod.Load(stateDir, name)
	switch {
	case err == nil:
		backend = existing.BackendOrDefault()
		gracePeriod = existing.TerminationGracePeriodSeconds
		if podDir == "" {
			podDir = existing.StaticPodPath
		}
	case os.IsNotExist(err):
		if podDir == "" {
			podDir = staticpod.Defaults().StaticPodPath
		}
	default:
		return err
	}

	// Nothing to clean up out here any more: a VM's writable overlay
	// lives inside its own container (see config.PrepareOverlay), so
	// removing the container removes it, and there is no host directory
	// left over to delete.

	switch backend {
	case staticpod.BackendDocker:
		if err := dockervm.Delete(ctx, &dockervm.Docker{}, name, gracePeriod, stdout); err != nil {
			return err
		}
	default:
		// A second "vm delete" of the same VM, and one of a VM that only
		// ever ran under -backend docker but whose saved state is
		// already gone, both land here with nothing to remove: say so
		// rather than claim a manifest was removed that was never
		// there.
		manifestPath := filepath.Join(podDir, staticpod.ManifestFileName(name))
		switch rmErr := os.Remove(manifestPath); {
		case rmErr == nil:
			fmt.Fprintf(stdout, "removed %s\n", manifestPath)
		case os.IsNotExist(rmErr):
			fmt.Fprintf(stdout, "no manifest at %s\n", manifestPath)
		default:
			return fmt.Errorf("removing manifest %s: %w", manifestPath, rmErr)
		}
	}

	if err := staticpod.Delete(stateDir, name); err != nil {
		return fmt.Errorf("removing saved state for %q: %w", name, err)
	}
	fmt.Fprintf(stdout, "deleted VM %q\n", name)
	return nil
}

func runVMList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("konturctl vm list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", defaultStateDir, "directory kontur stores VM state in")
	if err := fs.Parse(args); err != nil {
		return err
	}

	specs, err := staticpod.List(*stateDir)
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		fmt.Fprintln(stdout, "no VMs")
		return nil
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	// No address column: the guest takes over whichever address the
	// container runtime assigned its sandbox, which is not something
	// konturctl chose or has saved out here.
	fmt.Fprintln(w, "NAME\tCPUS\tMEMORY_MB\tDISK_MB\tIMAGE")
	for _, s := range specs {
		// A VM that asked for no particular disk size takes the image's
		// own, which isn't a number konturctl knows out here (it's a file
		// inside the VM's container), so say so rather than print "0".
		diskMB := "image"
		if s.DiskSizeMB > 0 {
			diskMB = strconv.Itoa(s.DiskSizeMB)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", s.Name, s.CPUs, s.MemoryMB, diskMB, s.KonturImage)
	}
	return w.Flush()
}
