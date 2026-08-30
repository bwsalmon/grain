// Package dockervm implements konturctl's staticpod.BackendDocker: instead
// of rendering a static pod manifest for the standalone kubelet (see
// internal/staticpod) to pick up, it launches the same netshim/VM
// container pair directly against a local docker daemon by shelling out to
// the docker CLI. This is for hosts that just want to run kontur VMs with
// "docker run" -- no containerd, CNI or kubelet installed at all.
//
// A pod's containers share a network namespace that's held open by the
// pod sandbox for as long as the pod exists, which is what lets a
// netshim-mode init container set up taps/bridge/NAT before the VM
// container that uses them ever starts. Plain docker containers have no
// such sandbox, and a container's network namespace disappears with it,
// so this package starts a third, otherwise-idle container per VM --
// the same kontur image, entrypoint overridden to its own "sleep" mode
// (see cmd/kontur; the image ships from "scratch" with no coreutils
// "sleep" binary of its own to exec instead) -- purely to hold that
// namespace open for netshim and the VM container to share, standing in
// for the pod sandbox.
package dockervm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/bwsalmon/kontur/internal/staticpod"
)

// Docker runs the docker CLI to implement Create/Delete below. "vm update"
// has no separate implementation here: internal/cli drives it as a
// Delete followed by a Create, since docker containers have no equivalent
// of re-submitting a pod manifest for the kubelet to reconcile.
type Docker struct {
	// BinaryPath is the docker binary to exec, resolved via PATH if it
	// contains no slash. Defaults to "docker"; tests point it at a fake
	// so this package can be exercised without a real docker daemon.
	BinaryPath string
}

func (d *Docker) binary() string {
	if d.BinaryPath == "" {
		return "docker"
	}
	return d.BinaryPath
}

// run execs "docker <args...>", streaming stdout to the given writer and
// folding stderr into the returned error.
func (d *Docker) run(ctx context.Context, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, d.binary(), args...)
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// isNotFound reports whether err came from a docker CLI invocation that
// failed because the named container doesn't exist -- the docker CLI's
// only way of expressing that is this stderr substring.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such container")
}

// remove force-removes a container, treating "already gone" as success.
func (d *Docker) remove(ctx context.Context, name string) error {
	if err := d.run(ctx, io.Discard, "rm", "-f", name); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// stopAndRemove stops a container (SIGTERM, then SIGKILL after
// graceSeconds -- the same shape as kubelet's terminationGracePeriodSeconds
// handling, which is what lets "kontur run"'s own
// SIGTERM->power-button->forced-shutdown sequence, see the top-level
// README's "Shutdown" section, play out the same way here as under a real
// kubelet) and removes it, treating "already gone" as success throughout.
func (d *Docker) stopAndRemove(ctx context.Context, name string, graceSeconds int) error {
	if err := d.run(ctx, io.Discard, "stop", "-t", strconv.Itoa(graceSeconds), name); err != nil && !isNotFound(err) {
		return err
	}
	return d.remove(ctx, name)
}

// vmContainerName and netnsContainerName are the docker container names
// for a VM's two containers -- deterministic from the VM name alone, so
// Delete doesn't need a saved VMSpec to know what to remove.
func vmContainerName(name string) string { return staticpod.PodName(name) }
func netnsContainerName(name string) string {
	return staticpod.PodName(name) + "-netns"
}

// Create starts a VM's containers: a netns-holder container standing in
// for a pod sandbox, then netshim (run to completion, wiring up that
// namespace's tap/bridge/NAT), then the VM container itself -- the same
// shape staticpod.Render's manifest describes, run directly here instead
// of handed to a kubelet. If netshim or the VM container fails to start,
// Create tears down whatever it already created.
func Create(ctx context.Context, d *Docker, spec staticpod.VMSpec, stdout io.Writer) error {
	netnsName := netnsContainerName(spec.Name)
	vmName := vmContainerName(spec.Name)

	if err := d.run(ctx, io.Discard, "run", "-d",
		"--name", netnsName,
		"--label", "kontur.dev/vm="+spec.Name,
		"--entrypoint", "/usr/local/bin/kontur",
		spec.KonturImage, "sleep",
	); err != nil {
		return fmt.Errorf("starting network namespace holder %s: %w", netnsName, err)
	}

	netshimArgs := []string{
		"run", "--rm",
		"--network", "container:" + netnsName,
		// The static pod manifest only grants netshim NET_ADMIN/NET_RAW
		// (see staticpod's manifestTemplateSrc), which is enough under a
		// real kubelet's CRI runtime. Plain docker is stricter: it masks
		// /proc/sys/net read-only in a container's own mount namespace
		// regardless of capabilities or --sysctl (confirmed by hand:
		// NET_ADMIN alone still gets "read-only file system" writing
		// net.ipv4.ip_forward, the write internal/netshim/setup.go needs
		// for its NAT/MASQUERADE rules to actually forward VM traffic),
		// so netshim needs --privileged here to do the same work docker
		// would otherwise refuse regardless of which capabilities are
		// added.
		"--privileged",
		"-e", "NETSHIM_VMS=" + fmt.Sprintf("%s:%s:%d", spec.Name, spec.IP, spec.Port),
		"-e", "NETSHIM_BRIDGE=" + spec.Bridge,
		"-e", "NETSHIM_BRIDGE_CIDR=" + spec.BridgeCIDR,
		"-e", "NETSHIM_EXTERNAL_IFACE=" + spec.ExternalIface,
		"-e", "NETSHIM_GUEST_PORT=" + strconv.Itoa(spec.GuestPort),
		spec.KonturImage, "netshim",
	}
	if err := d.run(ctx, stdout, netshimArgs...); err != nil {
		_ = d.remove(ctx, netnsName)
		return fmt.Errorf("running netshim for %q: %w", spec.Name, err)
	}

	vmArgs := []string{
		"run", "-d",
		"--name", vmName,
		"--label", "kontur.dev/vm=" + spec.Name,
		"--network", "container:" + netnsName,
		"--privileged",
		"--device", "/dev/kvm",
		"-v", spec.ImagesHostPath + ":/images:ro",
		"-e", "CHV_DISK_IMAGE=" + spec.DiskImage,
		"-e", "CHV_DISK_READONLY=" + strconv.FormatBool(spec.DiskReadOnly),
	}
	if spec.Kernel != "" {
		vmArgs = append(vmArgs, "-e", "CHV_KERNEL="+spec.Kernel)
	}
	if spec.Initramfs != "" {
		vmArgs = append(vmArgs, "-e", "CHV_INITRAMFS="+spec.Initramfs)
	}
	if spec.Firmware != "" {
		vmArgs = append(vmArgs, "-e", "CHV_FIRMWARE="+spec.Firmware)
	}
	if spec.Cmdline != "" {
		vmArgs = append(vmArgs, "-e", "CHV_CMDLINE="+spec.Cmdline)
	}
	vmArgs = append(vmArgs,
		"-e", "CHV_NET=tap=tap-"+spec.Name,
		"-e", "CHV_CPUS="+strconv.Itoa(spec.CPUs),
		"-e", "CHV_MEMORY_MB="+strconv.Itoa(spec.MemoryMB),
		"-e", "CHV_SHUTDOWN_TIMEOUT="+spec.ShutdownTimeout,
		spec.KonturImage, "run",
	)
	if err := d.run(ctx, io.Discard, vmArgs...); err != nil {
		_ = d.remove(ctx, netnsName)
		return fmt.Errorf("starting VM container %s: %w", vmName, err)
	}

	fmt.Fprintf(stdout, "started %s (VM) attached to %s (network namespace)\n", vmName, netnsName)
	return nil
}

// Delete stops and removes a VM's containers. Safe to call when they
// don't exist at all (e.g. a retried delete, or a VM that never finished
// Create), and safe to call concurrently with nothing else touching the
// same VM name.
func Delete(ctx context.Context, d *Docker, name string, terminationGracePeriodSeconds int, stdout io.Writer) error {
	vmName := vmContainerName(name)
	netnsName := netnsContainerName(name)

	if err := d.stopAndRemove(ctx, vmName, terminationGracePeriodSeconds); err != nil {
		return fmt.Errorf("removing VM container %s: %w", vmName, err)
	}
	if err := d.remove(ctx, netnsName); err != nil {
		return fmt.Errorf("removing network namespace holder %s: %w", netnsName, err)
	}
	fmt.Fprintf(stdout, "removed %s and %s\n", vmName, netnsName)
	return nil
}
