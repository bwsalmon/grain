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

func runVM(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("vm: expected a subcommand (create, update, delete, list)")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		return runVMCreate(ctx, rest, stdout, stderr)
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
	diskImage                     *string
	diskReadOnly                  *bool
	diskMode                      *string
	diskSizeMB                    *int
	kernel                        *string
	initramfs                     *string
	firmware                      *string
	cmdline                       *string
	cpus                          *int
	memoryMB                      *int
	shutdownTimeout               *string
	ip                            *string
	port                          *int
	guestPort                     *int
	guestUser                     *string
	bridge                        *string
	bridgeCIDR                    *string
	externalIface                 *string
	netMode                       *string
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
	v := &vmFlags{}
	v.diskImage = fs.String("disk", d.DiskImage, "path to the VM's disk image under -images-hostpath, as seen inside the kontur container (e.g. /images/disk.img)")
	v.diskMode = fs.String("disk-mode", d.DiskMode, `how the VM attaches its disk: "overlay" (the default -- the guest writes into a thin qcow2 of its own, created in its container, leaving the image untouched and shared), "persistent" (the guest writes through to the image itself) or "readonly"`)
	v.diskReadOnly = fs.Bool("disk-readonly", d.DiskReadOnly, "deprecated, use -disk-mode: true means -disk-mode=readonly, false means -disk-mode=overlay (which is what it always did: a private writable disk per VM)")
	v.diskSizeMB = fs.Int("disk-size-mb", d.DiskSizeMB, "size of the VM's own writable overlay, in MiB; 0 means the disk image's own size, and a larger value grows an overlay an earlier boot already created (-disk-mode=overlay only, and the guest still has to grow its filesystem into the space)")
	v.kernel = fs.String("kernel", d.Kernel, "path to a kernel for direct boot, as seen inside the container (mutually exclusive with -firmware)")
	v.initramfs = fs.String("initramfs", d.Initramfs, "path to an initramfs, used with -kernel")
	v.firmware = fs.String("firmware", d.Firmware, "path to firmware for firmware boot, as seen inside the container (mutually exclusive with -kernel)")
	v.cmdline = fs.String("cmdline", d.Cmdline, "kernel command line, used with -kernel (default: derived from -ip so the guest's netshim address matches)")
	v.cpus = fs.Int("cpus", d.CPUs, "boot vCPU count")
	v.memoryMB = fs.Int("memory-mb", d.MemoryMB, "guest memory, in MiB")
	v.shutdownTimeout = fs.String("shutdown-timeout", d.ShutdownTimeout, "how long to wait for a graceful guest shutdown before forcing it (Go duration syntax)")
	v.ip = fs.String("ip", d.IP, "address netshim assigns this VM on its bridge subnet; the guest must configure the same address")
	v.port = fs.Int("port", d.Port, "external port on the pod IP that forwards to this VM")
	v.guestPort = fs.Int("guest-port", d.GuestPort, "port the guest listens on internally")
	v.guestUser = fs.String("guest-user", d.GuestUser, "guest account, besides root, that \"kontur exec\" logs in as: the guest authorizes this boot's generated key for it too (empty means root)")
	v.bridge = fs.String("bridge", d.Bridge, "name of the in-pod bridge netshim creates")
	v.bridgeCIDR = fs.String("bridge-cidr", d.BridgeCIDR, "the bridge's own address and subnet; -ip must fall within it")
	v.externalIface = fs.String("external-iface", d.ExternalIface, "the pod's primary interface")
	v.netMode = fs.String("net", d.NetModeOrDefault(), "how the guest reaches the network: \"nat\" (private subnet behind netshim's DNAT/masquerade) or \"flat\" (spliced onto the sandbox's own segment, taking over its address and MAC)")
	v.controlCIDR = fs.String("control-cidr", d.ControlCIDR, "address netshim holds on the flat-mode control link, the private second NIC that keeps \"kontur exec\" and the memory agent able to reach the guest; empty disables it")
	v.dockerRunOpts.values = append([]string(nil), d.DockerRunOpts...)
	fs.Var(&v.dockerRunOpts, "docker-run-opt", "extra option passed verbatim to the \"docker run\" creating the network namespace holder, repeatable (e.g. -docker-run-opt -p -docker-run-opt 8080:80); -backend docker only")
	v.imagesHostPath = fs.String("images-hostpath", d.ImagesHostPath, "host directory mounted read-only at /images in the kontur container")
	// Accepted and ignored rather than removed: a deployment's own
	// scripts pass it, and failing their "vm create" outright over a flag
	// that now has nothing to configure would break them for no gain.
	v.diskHostPath = fs.String("disk-hostpath", d.DiskHostPath, "deprecated and ignored: each VM's writable overlay is created inside its own container now, so there is no host directory to place it in")
	v.konturImage = fs.String("kontur-image", d.KonturImage, "kontur image reference, used for both the netshim init container and the VM container")
	v.terminationGracePeriodSeconds = fs.Int("termination-grace-period", d.TerminationGracePeriodSeconds, "pod terminationGracePeriodSeconds; must comfortably exceed -shutdown-timeout")
	v.staticPodPath = fs.String("static-pod-path", d.StaticPodPath, "directory the standalone kubelet watches for static pod manifests")
	return v
}

func (v *vmFlags) toSpec(name string) staticpod.VMSpec {
	return staticpod.VMSpec{
		Name:                          name,
		DiskImage:                     *v.diskImage,
		DiskReadOnly:                  *v.diskReadOnly,
		DiskMode:                      *v.diskMode,
		DiskSizeMB:                    *v.diskSizeMB,
		Kernel:                        *v.kernel,
		Initramfs:                     *v.initramfs,
		Firmware:                      *v.firmware,
		Cmdline:                       *v.cmdline,
		CPUs:                          *v.cpus,
		MemoryMB:                      *v.memoryMB,
		ShutdownTimeout:               *v.shutdownTimeout,
		IP:                            *v.ip,
		Port:                          *v.port,
		GuestPort:                     *v.guestPort,
		GuestUser:                     *v.guestUser,
		Bridge:                        *v.bridge,
		BridgeCIDR:                    *v.bridgeCIDR,
		ExternalIface:                 *v.externalIface,
		NetMode:                       *v.netMode,
		ControlCIDR:                   *v.controlCIDR,
		DockerRunOpts:                 v.dockerRunOpts.values,
		ImagesHostPath:                *v.imagesHostPath,
		DiskHostPath:                  *v.diskHostPath,
		KonturImage:                   *v.konturImage,
		TerminationGracePeriodSeconds: *v.terminationGracePeriodSeconds,
		StaticPodPath:                 *v.staticPodPath,
	}
}

func splitNameAndFlags(args []string, cmd string) (name string, rest []string, err error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("%s: a VM name is required", cmd)
	}
	return args[0], args[1:], nil
}

// peekStateDir scans args by hand for -state-dir/--state-dir, in any of
// flag's accepted forms ("-name value" or "-name=value"), without
// registering or consuming any other flag. Returns defaultStateDir if not
// present.
func peekStateDir(args []string) string {
	for i, a := range args {
		switch {
		case a == "-state-dir" || a == "--state-dir":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "-state-dir="):
			return strings.TrimPrefix(a, "-state-dir=")
		case strings.HasPrefix(a, "--state-dir="):
			return strings.TrimPrefix(a, "--state-dir=")
		}
	}
	return defaultStateDir
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
	v := registerVMFlags(fs, staticpod.Defaults())
	if err := fs.Parse(rest); err != nil {
		return err
	}

	if _, err := staticpod.Load(*stateDir, name); err == nil {
		return fmt.Errorf("VM %q already exists (use \"konturctl vm update\" to change it)", name)
	}

	spec := v.toSpec(name)
	spec.Backend = *backend
	return submitVM(ctx, spec, *stateDir, stdout)
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
	existing, err := staticpod.Load(peekStateDir(rest), name)
	if err != nil {
		return fmt.Errorf("VM %q not found (use \"konturctl vm create\" first): %w", name, err)
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

	backend := staticpod.BackendStaticPod
	gracePeriod := staticpod.Defaults().TerminationGracePeriodSeconds
	podDir := *staticPodPath
	existing, err := staticpod.Load(*stateDir, name)
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
		manifestPath := filepath.Join(podDir, staticpod.ManifestFileName(name))
		if rmErr := os.Remove(manifestPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("removing manifest %s: %w", manifestPath, rmErr)
		}
		fmt.Fprintf(stdout, "removed %s\n", manifestPath)
	}

	if err := staticpod.Delete(*stateDir, name); err != nil {
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
	fmt.Fprintln(w, "NAME\tIP\tPORT\tCPUS\tMEMORY_MB\tDISK_MB\tIMAGE")
	for _, s := range specs {
		// A VM that asked for no particular disk size takes the image's
		// own, which isn't a number konturctl knows out here (it's a file
		// inside the VM's container), so say so rather than print "0".
		diskMB := "image"
		if s.DiskSizeMB > 0 {
			diskMB = strconv.Itoa(s.DiskSizeMB)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\t%s\n", s.Name, s.IP, s.Port, s.CPUs, s.MemoryMB, diskMB, s.KonturImage)
	}
	return w.Flush()
}
