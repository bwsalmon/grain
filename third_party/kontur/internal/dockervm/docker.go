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

	"github.com/bwsalmon/kontur/internal/netshim"
	"github.com/bwsalmon/kontur/internal/staticpod"
)

// netshimPrivilegeArgs returns the docker flags netshim needs to do its
// work, which differ by net mode.
//
// NAT mode needs --privileged for one specific reason: it writes
// net.ipv4.ip_forward, and docker masks /proc/sys/net read-only in a
// container's mount namespace regardless of capabilities or --sysctl
// (confirmed by hand: NET_ADMIN alone still gets "read-only file system"
// on that write, without which its NAT/MASQUERADE rules never forward
// anything). That turned out to be true of a real kubelet/containerd CRI
// pod too, which is why the static pod manifest is privileged as well.
//
// Flat mode does no routing and installs no NAT rules, so it never makes
// that write. What it does need is CAP_NET_ADMIN for the tap, the tc
// splice and the control link, plus /dev/net/tun itself -- the netlink
// library creates a tap by opening that device rather than over
// rtnetlink, and docker's device cgroup denies it unless it is granted
// explicitly.
func netshimPrivilegeArgs(spec staticpod.VMSpec) []string {
	if spec.NetModeOrDefault() == netshim.ModeNAT {
		return []string{"--privileged"}
	}
	return []string{
		"--cap-add", "NET_ADMIN",
		"--cap-add", "NET_RAW",
		"--device", "/dev/net/tun",
	}
}

// netshimEnvArgs returns the "-e" pairs describing this VM's networking,
// passed to netshim itself and (in flat mode) to the VM container too, so
// both read the same settings rather than keeping separate copies.
func netshimEnvArgs(spec staticpod.VMSpec) []string {
	if spec.NetModeOrDefault() == netshim.ModeNAT {
		return []string{
			"-e", "NETSHIM_VMS=" + fmt.Sprintf("%s:%s:%d", spec.Name, spec.IP, spec.Port),
			"-e", "NETSHIM_BRIDGE=" + spec.Bridge,
			"-e", "NETSHIM_BRIDGE_CIDR=" + spec.BridgeCIDR,
			"-e", "NETSHIM_EXTERNAL_IFACE=" + spec.ExternalIface,
			"-e", "NETSHIM_GUEST_PORT=" + strconv.Itoa(spec.GuestPort),
		}
	}
	return []string{
		"-e", "NETSHIM_MODE=" + netshim.ModeFlat,
		"-e", "NETSHIM_VM=" + spec.Name,
		"-e", "NETSHIM_BRIDGE=" + spec.Bridge,
		"-e", "NETSHIM_CONTROL_CIDR=" + spec.ControlCIDR,
		"-e", "NETSHIM_EXTERNAL_IFACE=" + spec.ExternalIface,
	}
}

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

	// A previous Create for this VM name may have died partway through
	// -- e.g. the daemon restarting between "docker run"ning these
	// containers and konturctl persisting that success to local state --
	// leaving one or both containers running under the names this
	// Create is about to reuse. Force-remove any such leftovers first so
	// Create is idempotent the same way Delete already is, rather than
	// failing outright on docker's "name already in use" error.
	if err := d.remove(ctx, vmName); err != nil {
		return fmt.Errorf("removing stale VM container %s: %w", vmName, err)
	}
	if err := d.remove(ctx, netnsName); err != nil {
		return fmt.Errorf("removing stale network namespace holder %s: %w", netnsName, err)
	}

	// Everything docker offers about a container's networking -- port
	// publishing, network membership, DNS, aliases -- is a property of
	// the network namespace, and has to be set on the container that
	// creates it. Containers that join an existing namespace with
	// "--network container:" cannot add any of it afterwards, so the
	// holder is the only place a caller's own docker options can go.
	netnsArgs := []string{
		"run", "-d",
		"--name", netnsName,
		"--label", "kontur.dev/vm=" + spec.Name,
		"--entrypoint", "/usr/local/bin/kontur",
	}
	netnsArgs = append(netnsArgs, spec.DockerRunOpts...)
	netnsArgs = append(netnsArgs, spec.KonturImage, "sleep")
	if err := d.run(ctx, io.Discard, netnsArgs...); err != nil {
		return fmt.Errorf("starting network namespace holder %s: %w", netnsName, err)
	}

	netshimArgs := append([]string{
		"run", "--rm",
		"--network", "container:" + netnsName,
	}, netshimPrivilegeArgs(spec)...)
	netshimArgs = append(netshimArgs, netshimEnvArgs(spec)...)
	netshimArgs = append(netshimArgs, spec.KonturImage, "netshim")
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
		"-e", "CHV_DISK_MODE=" + spec.DiskModeOrDerived(),
	}
	// A spec with no disk of its own boots the one baked into the kontur
	// image: nothing to mount, and CHV_DISK_IMAGE left unset so "kontur
	// run"'s own default applies. See VMSpec.Validate.
	if spec.DiskImage != "" {
		// Read-only, always: a writable disk is a qcow2 overlay the VM's
		// own container creates against this image now, so nothing here
		// ever writes to the shared mount.
		vmArgs = append(vmArgs,
			"-v", spec.ImagesHostPath+":"+staticpod.ImagesMountPath+":ro",
			"-e", "CHV_DISK_IMAGE="+spec.DiskImage)
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
	if spec.NetModeOrDefault() == netshim.ModeNAT {
		vmArgs = append(vmArgs, "-e", "CHV_NET=tap=tap-"+spec.Name)
	} else {
		// Flat mode derives its own --net values (and the guest's ip=
		// parameter) at boot from the identity on the namespace's
		// external interface, so the VM container gets the same netshim
		// settings rather than a precomputed CHV_NET. See cmd/kontur's
		// applyFlatNet.
		vmArgs = append(vmArgs, netshimEnvArgs(spec)...)
	}
	vmArgs = append(vmArgs,
		"-e", "CHV_CPUS="+strconv.Itoa(spec.CPUs),
		"-e", "CHV_MEMORY_MB="+strconv.Itoa(spec.MemoryMB),
		"-e", "CHV_SHUTDOWN_TIMEOUT="+spec.ShutdownTimeout,
	)
	// See staticpod's manifestTemplateSrc for why this is an address on
	// the guest's own NIC at the fixed guest sshd port, not the external
	// port NAT mode forwards. Flat mode with no control link has no such
	// address at all, and so no exec path in.
	if addr := spec.ExecAddr(); addr != "" {
		vmArgs = append(vmArgs, "-e", "KONTUR_EXEC_ADDR="+addr)
	}
	// Read twice inside the container, for the two halves of one fact:
	// "kontur run" puts it on the guest's kernel command line so the
	// generated key is authorized for this account, and "kontur exec"
	// (which docker exec runs with the container's own environment) logs
	// in as it. See VMSpec.GuestUser.
	if spec.GuestUser != "" {
		vmArgs = append(vmArgs, "-e", "KONTUR_EXEC_USER="+spec.GuestUser)
	}
	vmArgs = append(vmArgs, spec.KonturImage, "run")
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
