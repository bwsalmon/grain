// Package config loads the settings for a single cloud-hypervisor VM from
// environment variables, so the container can be driven entirely through a
// Kubernetes pod spec (env / envFrom) without a mounted config file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/kontur/internal/qcow2"
)

const (
	envDiskImage        = "CHV_DISK_IMAGE"
	envDiskReadonly     = "CHV_DISK_READONLY"
	envDiskMode         = "CHV_DISK_MODE"
	envDiskOverlayPath  = "CHV_DISK_OVERLAY_PATH"
	envDiskSizeMB       = "CHV_DISK_SIZE_MB"
	envExtraDisks       = "CHV_EXTRA_DISKS"
	envKernel           = "CHV_KERNEL"
	envInitramfs        = "CHV_INITRAMFS"
	envFirmware         = "CHV_FIRMWARE"
	envCmdline          = "CHV_CMDLINE"
	envCPUs             = "CHV_CPUS"
	envCPUsMax          = "CHV_CPUS_MAX"
	envMemoryMB         = "CHV_MEMORY_MB"
	envMemoryMaxMB      = "CHV_MEMORY_MAX_MB"
	envMemoryHotplug    = "CHV_MEMORY_HOTPLUG"
	envMemoryShared     = "CHV_MEMORY_SHARED"
	envNet              = "CHV_NET"
	envAPISocket        = "CHV_API_SOCKET"
	envVsockSocket      = "CHV_VSOCK_SOCKET"
	envBinaryPath       = "CHV_BINARY_PATH"
	envExtraArgs        = "CHV_EXTRA_ARGS"
	envShutdownTimeout  = "CHV_SHUTDOWN_TIMEOUT"
	envSetupScript      = "CHV_SETUP_SCRIPT"
	envSnapshotPath     = "CHV_SNAPSHOT_PATH"
	envMemAgent         = "CHV_MEM_AGENT"
	envMemAgentAddr     = "CHV_MEM_AGENT_ADDR"
	envMemAgentStepMB   = "CHV_MEM_AGENT_STEP_MB"
	envMemAgentCooldown = "CHV_MEM_AGENT_COOLDOWN"

	defaultCmdline = "console=ttyS0 root=/dev/vda rw"
	defaultCPUs    = 2

	// defaultMemoryMB is deliberately small: with memory hotplug on by
	// default (defaultMemoryHotplug), the guest can grow up to
	// defaultMemoryMaxMB on demand (see BuildArgs/Resize), so there is no
	// need to pay for a large VMM memory footprint from the very first
	// boot the way a fixed CHV_MEMORY_MB always did.
	defaultMemoryMB      = 256
	defaultMemoryMaxMB   = 2048
	defaultMemoryHotplug = true
	defaultAPISocket     = "/run/cloud-hypervisor/api.sock"
	// defaultVsockSocket is the host end of the guest's virtio-vsock
	// device: a unix socket in this container that "kontur exec" dials
	// to reach kontur-agent inside the guest (see internal/guestexec and
	// cmd/kontur-agent). Under cloud-hypervisor's hybrid vsock the host
	// side is a socket file rather than anything on a network, which is
	// what lets exec work with no guest networking at all.
	defaultVsockSocket = "/run/kontur/vsock.sock"
	// defaultVsockCID is the guest's context id. Any value above the two
	// reserved ones (0 is hypervisor, 2 is host) does, and nothing on
	// either side reads it back: the guest binds VMADDR_CID_ANY and the
	// host addresses the guest by connecting to the socket above, not by
	// CID. cloud-hypervisor requires one anyway.
	defaultVsockCID        = 3
	defaultBinaryPath      = "/usr/local/bin/cloud-hypervisor"
	defaultShutdownTimeout = 20 * time.Second

	// defaultMemAgent is off by default: unlike CHV_MEMORY_HOTPLUG (which
	// merely wires up the *capability* to resize), enabling this actually
	// lets the guest drive resizes on its own, unprompted -- new enough
	// behavior, and enough of a departure from every other knob in this
	// package (all of which are either inert until an operator acts, or
	// bounded by values the operator themselves set), that it's opt-in
	// rather than on by default. See internal/memagent.
	defaultMemAgent = false

	// defaultMemAgentAddr matches netshim's own default control link
	// address (see internal/netshim's defaultControlCIDR): the
	// guest-side agent (deploy/guest-image's kontur-mem-agent) is told
	// that address by kontur-control-net at boot and signals it on this
	// fixed port. Overriding NETSHIM_CONTROL_CIDR therefore also
	// requires overriding this to match, or the guest's signals go
	// nowhere.
	defaultMemAgentAddr     = "169.254.100.1:30090"
	defaultMemAgentStepMB   = 256
	defaultMemAgentCooldown = 30 * time.Second

	// defaultDiskImage is the guest disk image baked into the kontur OCI
	// image itself (see the Dockerfile's guest-image stage): a minimal
	// Debian system carrying kontur-agent, usable out of the box without
	// a separately-managed disk image. CHV_DISK_IMAGE overrides this for
	// any other guest.
	defaultDiskImage = "/var/lib/kontur/guest/disk.img"

	// DiskModeOverlay, DiskModePersistent and DiskModeReadOnly are the
	// three things "a writable root disk" can mean, which CHV_DISK_READONLY
	// could not express as a boolean.
	//
	//   overlay    (the default) the guest writes into a thin qcow2 of its
	//              own, backed by the disk image, which is only ever read.
	//              Boot costs no copy of the image however large it is,
	//              several VMs on a host share it, and discarding the
	//              overlay resets the guest.
	//   persistent the guest writes through to the disk image itself. What
	//              "konturctl guest build" needs, since the point there is
	//              for the changes to end up in the image being committed,
	//              and what CHV_DISK_READONLY=false has always meant.
	//   readonly   the disk is attached read-only, as CHV_DISK_READONLY=true.
	DiskModeOverlay    = "overlay"
	DiskModePersistent = "persistent"
	DiskModeReadOnly   = "readonly"

	// defaultOverlayPath is where DiskModeOverlay puts the guest's qcow2.
	// Inside the container's own writable layer rather than on a tmpfs, so
	// a stopped-and-started container keeps whatever the guest had written
	// -- the same thing that happened when writes landed in the disk image
	// -- and only "docker rm" discards it. The .qcow2 suffix is load-bearing:
	// it is what hypervisor.Args uses to decide image_type and
	// backing_files. Override with CHV_DISK_OVERLAY_PATH.
	defaultOverlayPath = "/var/lib/kontur/overlay/disk.qcow2"

	// defaultKernel is the guest kernel baked into the kontur OCI image
	// itself (see the Dockerfile's fetch-kernel stage): a
	// cloud-hypervisor/linux release build with PVH entry plus
	// virtio-pci/virtio-blk/virtio-net/virtio-mem support, matching what
	// the bundled disk image (defaultDiskImage) itself needs to boot and
	// what CONFIG_VIRTIO_MEM memory hotplug (see "Memory hotplug" in the
	// README) needs to actually take effect. Used only when neither
	// CHV_KERNEL nor CHV_FIRMWARE is set, so an operator supplying either
	// (e.g. for a custom guest that needs its own kernel, or a
	// firmware/bootloader-based guest) always overrides it as before.
	defaultKernel = "/var/lib/kontur/guest/vmlinux"

	// defaultGuestKernel and defaultGuestInitramfs are the guest's *own*
	// kernel and initramfs, present only when the image was built with
	// GUEST_KERNEL_PACKAGE (see the Dockerfile's guest-customized stage).
	// A guest that brings its own kernel needs it and nothing else:
	// booting such a rootfs under defaultKernel above instead gives it a
	// kernel whose modules are not the ones in its /lib/modules, which
	// fails late and obscurely rather than at boot. So when both are
	// there they win over defaultKernel, and when they are not -- the
	// default build -- nothing changes at all.
	defaultGuestKernel    = "/var/lib/kontur/guest/vmlinuz"
	defaultGuestInitramfs = "/var/lib/kontur/guest/initrd.img"
)

// Disk describes one virtio-blk device to attach to the VM.
type Disk struct {
	Path     string
	ReadOnly bool
}

// Config holds everything needed to boot exactly one cloud-hypervisor VM.
type Config struct {
	// Disks[0] is always the primary boot disk (CHV_DISK_IMAGE). Any
	// further entries come from CHV_EXTRA_DISKS.
	Disks []Disk

	// DiskMode is how Disks[0] is attached: DiskModeOverlay,
	// DiskModePersistent or DiskModeReadOnly. In overlay mode Disks[0]
	// still names the *source* image here; the caller creates the overlay
	// and repoints it (see PrepareOverlay) before the VM boots.
	DiskMode string

	// OverlayPath is where DiskModeOverlay writes the guest's qcow2.
	OverlayPath string

	// DiskSizeMB is the size of the writable disk the guest boots from,
	// in MiB -- the qcow2 overlay DiskModeOverlay gives it (see
	// PrepareOverlay), which is created at this size and grown to it on
	// a later boot if it already exists at a smaller one. Growing only,
	// and only ever the overlay: the source image itself is never
	// touched, so it must be at least as large as that image.
	//
	// Zero (the default) means the source image's own size, which is what
	// an overlay has always been given. Only meaningful in
	// DiskModeOverlay: the other two modes write to (or refuse to write
	// to) the image itself, and there is no overlay to size.
	//
	// This sizes the block device the guest sees and nothing else --
	// growing the partition table and filesystem inside it is the guest's
	// own job. The bundled reference guest does that for itself on every
	// boot (kontur-growfs, see deploy/guest-image/README.md); a guest
	// supplied via CHV_DISK_IMAGE has to arrange its own.
	DiskSizeMB int

	// Direct kernel boot. Kernel is mutually exclusive with Firmware:
	// setting both is an error, but leaving both unset is not -- Kernel
	// then defaults to defaultKernel, the kernel baked into the image
	// (see FromEnv).
	Kernel    string
	Initramfs string
	Firmware  string
	Cmdline   string

	// CPUs is the guest's vCPU count at boot. With CPUsMax greater than
	// CPUs, it can grow later via cloud-hypervisor's ACPI CPU hotplug
	// (see hypervisor.APIClient.ResizeCPUs and "kontur resize").
	CPUs int

	// CPUsMax is the ceiling CPUs can grow to via hotplug. Left at its
	// default (CPUs itself, i.e. no headroom), no hotplug device is
	// attached at all -- same fixed-vCPU behavior as before this
	// existed. Must be at least CPUs.
	CPUsMax int

	// MemoryMB is the guest's memory size at boot. With MemoryHotplug on,
	// it can grow up to MemoryMaxMB later, either from within the guest
	// or via the cloud-hypervisor API (see hypervisor.APIClient.Resize
	// and "kontur resize").
	MemoryMB int

	// MemoryMaxMB is the ceiling MemoryMB can grow to via hotplug. Ignored
	// when MemoryHotplug is false. Must be at least MemoryMB.
	MemoryMaxMB int

	// MemoryHotplug attaches a virtio-mem hotplug device sized for growth
	// up to MemoryMaxMB, so long as MemoryMaxMB is actually greater than
	// MemoryMB (otherwise there is no room to grow into, and no device is
	// attached at all). Requires MemoryShared, since virtio-mem needs a
	// shared memory backend.
	MemoryHotplug bool

	MemoryShared bool

	// Nets holds one --net value per guest NIC, each passed through
	// verbatim, e.g. "tap=eth0,mac=02:00:00:00:00:01". CHV_NET supplies
	// a single entry; a netshim-managed sandbox (see cmd/kontur)
	// replaces the whole list with values it derives from the
	// namespace's own identity, which is how the guest gets both its
	// spliced NIC and its control NIC. Left empty, the VM boots with no
	// network device.
	Nets []string

	APISocket string

	// VsockSocket is the host end of the guest's virtio-vsock device --
	// the unix socket cloud-hypervisor creates and "kontur exec" dials.
	// Empty attaches no vsock device at all, which leaves the guest with
	// no way to be exec'd into.
	VsockSocket string

	// VsockCID is the guest's vsock context id. See defaultVsockCID for
	// why its value does not matter.
	VsockCID   int
	BinaryPath string

	// ExtraArgs is split on whitespace and appended to the
	// cloud-hypervisor invocation as-is, for flags this package does not
	// model directly (e.g. --device, --vsock).
	ExtraArgs []string

	// ShutdownTimeout bounds how long the runtime waits for the guest to
	// power off gracefully after receiving SIGTERM before forcing the
	// VMM to stop.
	ShutdownTimeout time.Duration

	// SetupScript, if set, is run once inside the guest over SSH (see
	// internal/guestexec, the same machinery "kontur exec" uses) right
	// after a fresh (i.e. not restored) boot finishes. If SnapshotPath is
	// also set, the VM is then suspended to it so a later kontur run
	// resumes that exact post-setup state instead of booting fresh and
	// re-running the script; left unset, the script just runs again on
	// every fresh boot instead.
	SetupScript string

	// SnapshotPath, if SetupScript is also set, is where SetupScript's
	// completion is suspended to (see hypervisor.Runner.Suspend). Either
	// way, if a complete snapshot is already present there at startup,
	// it's restored from instead of a fresh boot (see
	// hypervisor.BuildArgs). Must be an absolute path: it's turned
	// directly into a "file://" URL for cloud-hypervisor's own
	// snapshot/restore API.
	SnapshotPath string

	// MemAgent starts internal/memagent's listener alongside the VM: a
	// guest-side agent (deploy/guest-image's kontur-mem-agent, installed
	// in both guest image variants) watches its own memory pressure and
	// signals this listener when it's under strain, which then grows
	// the guest's memory the same way an operator's "kontur resize"
	// would, up to MemoryMaxMB. Requires MemoryHotplug.
	MemAgent bool

	// MemAgentAddr is the address the listener above binds -- reachable
	// from the guest over netshim's control link (see
	// defaultMemAgentAddr). Ignored unless MemAgent is set.
	MemAgentAddr string

	// MemAgentStepMB is how much each pressure signal grows the guest's
	// memory by, capped at MemoryMaxMB.
	MemAgentStepMB int

	// MemAgentCooldown is the minimum time between two resizes the
	// listener performs, so a guest still under pressure right after a
	// grow (which is asynchronous -- see hypervisor.APIClient.Resize)
	// doesn't trigger another one before that grow has had a chance to
	// help.
	MemAgentCooldown time.Duration
}

// FromEnv builds a Config from the process environment and validates it.
// The disk image (and kernel/firmware, if given explicitly) are expected
// to already be present on the local filesystem: this runtime never
// fetches images, so startup only pays for booting the VM itself.
// CHV_DISK_IMAGE defaults to the guest image baked into the kontur OCI
// image (defaultDiskImage), and, unless CHV_FIRMWARE is given instead,
// CHV_KERNEL defaults to the matching kernel baked in alongside it
// (defaultKernel) -- set either explicitly to boot a different guest
// instead.
func FromEnv() (Config, error) {
	cfg := Config{
		Kernel:       os.Getenv(envKernel),
		Initramfs:    os.Getenv(envInitramfs),
		Firmware:     os.Getenv(envFirmware),
		Cmdline:      getEnvDefault(envCmdline, defaultCmdline),
		Nets:         netsFromEnv(),
		APISocket:    getEnvDefault(envAPISocket, defaultAPISocket),
		VsockSocket:  getEnvDefault(envVsockSocket, defaultVsockSocket),
		VsockCID:     defaultVsockCID,
		BinaryPath:   getEnvDefault(envBinaryPath, defaultBinaryPath),
		MemoryShared: true,
	}

	diskPath := getEnvDefault(envDiskImage, defaultDiskImage)
	mode, err := diskModeFromEnv()
	if err != nil {
		return Config{}, err
	}
	cfg.DiskMode = mode
	cfg.OverlayPath = getEnvDefault(envDiskOverlayPath, defaultOverlayPath)
	cfg.Disks = append(cfg.Disks, Disk{Path: diskPath, ReadOnly: mode == DiskModeReadOnly})

	cfg.DiskSizeMB, err = getEnvInt(envDiskSizeMB, 0)
	if err != nil {
		return Config{}, err
	}
	if cfg.DiskSizeMB < 0 {
		return Config{}, fmt.Errorf("%s must not be negative, got %d", envDiskSizeMB, cfg.DiskSizeMB)
	}
	// Rejected rather than ignored in the other two modes: the only
	// writable disk kontur creates for itself is the overlay, so a
	// request to size a disk it doesn't create can't be honoured, and
	// silently booting a guest at a size the operator didn't ask for is
	// the worse of the two answers.
	if cfg.DiskSizeMB > 0 && mode != DiskModeOverlay {
		return Config{}, fmt.Errorf("%s requires %s=%s: only the guest's own overlay is resized, never the disk image itself (got %s=%s)",
			envDiskSizeMB, envDiskMode, DiskModeOverlay, envDiskMode, mode)
	}

	for _, spec := range splitNonEmpty(os.Getenv(envExtraDisks), ",") {
		path, ro, err := parseExtraDisk(spec)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", envExtraDisks, err)
		}
		cfg.Disks = append(cfg.Disks, Disk{Path: path, ReadOnly: ro})
	}

	cfg.CPUs, err = getEnvInt(envCPUs, defaultCPUs)
	if err != nil {
		return Config{}, err
	}
	if cfg.CPUs < 1 {
		return Config{}, fmt.Errorf("%s must be at least 1, got %d", envCPUs, cfg.CPUs)
	}

	// CHV_CPUS_MAX defaults to CHV_CPUS itself: unlike memory, CPU
	// hotplug headroom is opt-in rather than on by default, since
	// (unlike virtio-mem) newly added vCPUs need the guest to online
	// them itself -- see the README's "CPU hotplug" section.
	cfg.CPUsMax, err = getEnvInt(envCPUsMax, cfg.CPUs)
	if err != nil {
		return Config{}, err
	}
	if cfg.CPUsMax < cfg.CPUs {
		return Config{}, fmt.Errorf("%s (%d) must be at least %s (%d)", envCPUsMax, cfg.CPUsMax, envCPUs, cfg.CPUs)
	}

	cfg.MemoryMB, err = getEnvInt(envMemoryMB, defaultMemoryMB)
	if err != nil {
		return Config{}, err
	}
	if cfg.MemoryMB < 128 {
		return Config{}, fmt.Errorf("%s must be at least 128, got %d", envMemoryMB, cfg.MemoryMB)
	}

	cfg.MemoryShared, err = getEnvBool(envMemoryShared, true)
	if err != nil {
		return Config{}, err
	}

	// CHV_MEMORY_MAX_MB defaults to whichever is larger of
	// defaultMemoryMaxMB and the (possibly overridden) starting size, so
	// raising CHV_MEMORY_MB alone never produces a nonsensical "max below
	// start" default.
	maxDefault := defaultMemoryMaxMB
	if cfg.MemoryMB > maxDefault {
		maxDefault = cfg.MemoryMB
	}
	cfg.MemoryMaxMB, err = getEnvInt(envMemoryMaxMB, maxDefault)
	if err != nil {
		return Config{}, err
	}
	if cfg.MemoryMaxMB < cfg.MemoryMB {
		return Config{}, fmt.Errorf("%s (%d) must be at least %s (%d)", envMemoryMaxMB, cfg.MemoryMaxMB, envMemoryMB, cfg.MemoryMB)
	}

	cfg.MemoryHotplug, err = getEnvBool(envMemoryHotplug, defaultMemoryHotplug)
	if err != nil {
		return Config{}, err
	}
	if cfg.MemoryHotplug && cfg.MemoryMaxMB > cfg.MemoryMB && !cfg.MemoryShared {
		return Config{}, fmt.Errorf("%s requires %s=true when %s (%d) exceeds %s (%d): virtio-mem hotplug needs a shared memory backend", envMemoryHotplug, envMemoryShared, envMemoryMaxMB, cfg.MemoryMaxMB, envMemoryMB, cfg.MemoryMB)
	}

	cfg.ShutdownTimeout, err = getEnvDuration(envShutdownTimeout, defaultShutdownTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.MemAgent, err = getEnvBool(envMemAgent, defaultMemAgent)
	if err != nil {
		return Config{}, err
	}
	cfg.MemAgentAddr = getEnvDefault(envMemAgentAddr, defaultMemAgentAddr)
	cfg.MemAgentStepMB, err = getEnvInt(envMemAgentStepMB, defaultMemAgentStepMB)
	if err != nil {
		return Config{}, err
	}
	if cfg.MemAgentStepMB < 1 {
		return Config{}, fmt.Errorf("%s must be at least 1, got %d", envMemAgentStepMB, cfg.MemAgentStepMB)
	}
	cfg.MemAgentCooldown, err = getEnvDuration(envMemAgentCooldown, defaultMemAgentCooldown)
	if err != nil {
		return Config{}, err
	}
	if cfg.MemAgent && !cfg.MemoryHotplug {
		return Config{}, fmt.Errorf("%s requires %s=true: the guest agent has nothing to ask for without hotplug wired up", envMemAgent, envMemoryHotplug)
	}

	cfg.ExtraArgs = splitNonEmpty(os.Getenv(envExtraArgs), " ")

	cfg.SetupScript = os.Getenv(envSetupScript)
	cfg.SnapshotPath = os.Getenv(envSnapshotPath)
	if cfg.SnapshotPath != "" {
		if !filepath.IsAbs(cfg.SnapshotPath) {
			return Config{}, fmt.Errorf("%s must be an absolute path, got %q", envSnapshotPath, cfg.SnapshotPath)
		}
		parent := filepath.Dir(cfg.SnapshotPath)
		if _, err := os.Stat(parent); err != nil {
			return Config{}, fmt.Errorf("%s's parent directory (%s): %w", envSnapshotPath, parent, err)
		}
	}

	if cfg.Kernel != "" && cfg.Firmware != "" {
		return Config{}, fmt.Errorf("%s and %s are mutually exclusive", envKernel, envFirmware)
	}
	// Neither set: default to the kernel baked into the image itself,
	// matching the bundled guest disk image the same way -- rather than
	// requiring one of the two to be supplied externally. The guest's
	// own kernel, if the image has one, is the right default for the
	// disk beside it; defaultKernel is the fallback for a guest that
	// brought none. CHV_INITRAMFS set by hand is left alone either way.
	if cfg.Kernel == "" && cfg.Firmware == "" {
		kernel, initramfs := bakedInKernel(defaultGuestKernel, defaultGuestInitramfs, defaultKernel)
		cfg.Kernel = kernel
		if cfg.Initramfs == "" {
			cfg.Initramfs = initramfs
		}
	}

	if err := cfg.checkPathsExist(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// APISocket returns the cloud-hypervisor API socket path from
// CHV_API_SOCKET (or its default) alone, without loading or validating
// the rest of a VM's configuration. Used by modes like "kontur resize"
// that only need to reach the API of an already-running VM in this same
// container, not to boot one.
func APISocket() string {
	return getEnvDefault(envAPISocket, defaultAPISocket)
}

// bakedInKernel picks between the two kernels an image can carry: the
// guest's own (guestKernel plus guestInitramfs, present only when the
// image was built with GUEST_KERNEL_PACKAGE) and fallback, the
// fetch-kernel build that has always been there. Both halves of the pair
// have to exist to take it -- a vmlinuz with no matching initrd would
// boot without the modules and udev rules the initramfs carries, which
// is exactly the silent unreachable-guest failure the pair exists to
// avoid. The fallback pairs with no initramfs, as before.
func bakedInKernel(guestKernel, guestInitramfs, fallback string) (kernel, initramfs string) {
	if fileExists(guestKernel) && fileExists(guestInitramfs) {
		return guestKernel, guestInitramfs
	}
	return fallback, ""
}

// fileExists reports whether path is present and is a regular file --
// used to pick between the two baked-in kernels, where "is it in this
// image" is the whole question. Any error (including a directory in the
// way) reads as absent: the fallback it selects is the kernel that has
// always been used, so a wrong answer here degrades to today's behavior
// rather than to a boot failure.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (c Config) checkPathsExist() error {
	paths := map[string]string{}
	for i, d := range c.Disks {
		paths[fmt.Sprintf("disk[%d] %s", i, d.Path)] = d.Path
	}
	if c.Kernel != "" {
		paths[envKernel] = c.Kernel
	}
	if c.Initramfs != "" {
		paths[envInitramfs] = c.Initramfs
	}
	if c.Firmware != "" {
		paths[envFirmware] = c.Firmware
	}
	for label, path := range paths {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s (%s): %w", label, path, err)
		}
	}
	return nil
}

func parseExtraDisk(spec string) (path string, readonly bool, err error) {
	parts := strings.SplitN(spec, ":", 2)
	path = parts[0]
	if path == "" {
		return "", false, fmt.Errorf("empty disk path in %q", spec)
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "ro":
			readonly = true
		case "rw":
			readonly = false
		default:
			return "", false, fmt.Errorf("unknown disk mode %q in %q, want \"ro\" or \"rw\"", parts[1], spec)
		}
	}
	return path, readonly, nil
}

func splitNonEmpty(s, sep string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, f := range strings.Split(s, sep) {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func getEnvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func getEnvInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", key, v)
	}
	return n, nil
}

func getEnvBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q", key, v)
	}
	return b, nil
}

func getEnvDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q", key, v)
	}
	return d, nil
}

// netsFromEnv turns CHV_NET into the one-element list BuildArgs expects,
// or no list at all when it is unset (a VM with no network device).
func netsFromEnv() []string {
	if v := os.Getenv(envNet); v != "" {
		return []string{v}
	}
	return nil
}

// diskModeFromEnv resolves CHV_DISK_MODE, falling back to the boolean it
// replaced.
//
// The fallback is not a straight translation, because the two settings
// were asked in different places and "writable" meant different things in
// each. CHV_DISK_READONLY is read here, inside the VM's own container,
// where false has always meant "write through to the disk image" -- so it
// maps to DiskModePersistent, and a deployment that sets it keeps exactly
// the behaviour it has today rather than silently acquiring a
// throw-away overlay. konturctl's own -disk-readonly=false meant
// something else again ("give this VM a private writable disk", which it
// implemented as a host-side overlay), so it maps to DiskModeOverlay --
// see internal/cli's own translation.
//
// Setting both is an error rather than a precedence rule: they contradict
// each other often enough (mode=overlay with readonly=true) that guessing
// which the caller meant is worse than saying so.
func diskModeFromEnv() (string, error) {
	mode := os.Getenv(envDiskMode)
	rawReadonly, hasReadonly := os.LookupEnv(envDiskReadonly)

	if mode != "" && hasReadonly {
		return "", fmt.Errorf("%s and %s are mutually exclusive (%s=%q, %s=%q): %s replaces it",
			envDiskMode, envDiskReadonly, envDiskMode, mode, envDiskReadonly, rawReadonly, envDiskMode)
	}

	if mode == "" {
		if !hasReadonly {
			return DiskModeOverlay, nil
		}
		readonly, err := getEnvBool(envDiskReadonly, false)
		if err != nil {
			return "", err
		}
		if readonly {
			return DiskModeReadOnly, nil
		}
		return DiskModePersistent, nil
	}

	switch mode {
	case DiskModeOverlay, DiskModePersistent, DiskModeReadOnly:
		return mode, nil
	default:
		return "", fmt.Errorf("%s must be %q, %q or %q, got %q",
			envDiskMode, DiskModeOverlay, DiskModePersistent, DiskModeReadOnly, mode)
	}
}

// PrepareOverlay makes DiskModeOverlay real: it creates the guest's qcow2
// at OverlayPath, backed by the source image Disks[0] currently names, and
// repoints Disks[0] at it. A no-op in the other two modes.
//
// Idempotent, and deliberately so in a specific way: an overlay that
// already exists is reused rather than recreated, so restarting a
// container keeps whatever the guest had written -- which is what happened
// when writes landed in the disk image itself, and the behaviour a caller
// restarting a VM expects. Discarding the overlay (removing the container)
// is what resets the guest.
//
// The backing file is recorded as the path Disks[0] already holds, which
// is a path inside this container -- the same place cloud-hypervisor will
// open it from, since it runs here too.
//
// DiskSizeMB, when set, is applied here too -- to a fresh overlay by
// creating it at that size, and to one that already exists by growing it
// (see qcow2.Resize) -- so it takes effect before the VM is started
// either way, and raising it on an already-running VM's next boot enlarges
// the disk it comes back with instead of being ignored.
func (c *Config) PrepareOverlay() error {
	if c.DiskMode != DiskModeOverlay || len(c.Disks) == 0 {
		return nil
	}
	source := c.Disks[0].Path
	wantBytes := int64(c.DiskSizeMB) * 1024 * 1024

	if _, err := os.Stat(c.OverlayPath); err == nil {
		if wantBytes > 0 {
			if err := qcow2.Resize(c.OverlayPath, wantBytes); err != nil {
				return fmt.Errorf("resizing the existing overlay %s to %s=%d: %w", c.OverlayPath, envDiskSizeMB, c.DiskSizeMB, err)
			}
		}
		c.Disks[0] = Disk{Path: c.OverlayPath}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking for an existing overlay at %s: %w", c.OverlayPath, err)
	}

	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("statting the source disk %s: %w", source, err)
	}
	// The overlay reads through to the source image for everything the
	// guest hasn't written, so presenting it as smaller than that image
	// would cut the guest's own filesystem off at the new end. Refuse
	// instead of quietly booting a truncated root disk.
	size := info.Size()
	if wantBytes > 0 {
		if wantBytes < size {
			return fmt.Errorf("%s=%d is smaller than the source disk %s (%d bytes): the guest's filesystem would be truncated",
				envDiskSizeMB, c.DiskSizeMB, source, size)
		}
		size = wantBytes
	}
	if err := os.MkdirAll(filepath.Dir(c.OverlayPath), 0o755); err != nil {
		return fmt.Errorf("creating the overlay directory: %w", err)
	}
	if err := qcow2.WriteOverlay(c.OverlayPath, source, size); err != nil {
		return fmt.Errorf("creating the qcow2 overlay %s backed by %s: %w", c.OverlayPath, source, err)
	}

	c.Disks[0] = Disk{Path: c.OverlayPath}
	return nil
}
