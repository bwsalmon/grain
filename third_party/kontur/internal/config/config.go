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
)

const (
	envDiskImage        = "CHV_DISK_IMAGE"
	envDiskReadonly     = "CHV_DISK_READONLY"
	envExtraDisks       = "CHV_EXTRA_DISKS"
	envKernel           = "CHV_KERNEL"
	envInitramfs        = "CHV_INITRAMFS"
	envFirmware         = "CHV_FIRMWARE"
	envCmdline          = "CHV_CMDLINE"
	envCPUs             = "CHV_CPUS"
	envMemoryMB         = "CHV_MEMORY_MB"
	envMemoryMaxMB      = "CHV_MEMORY_MAX_MB"
	envMemoryHotplug    = "CHV_MEMORY_HOTPLUG"
	envMemoryShared     = "CHV_MEMORY_SHARED"
	envNet              = "CHV_NET"
	envAPISocket        = "CHV_API_SOCKET"
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
	defaultMemoryMB        = 256
	defaultMemoryMaxMB     = 2048
	defaultMemoryHotplug   = true
	defaultAPISocket       = "/run/cloud-hypervisor/api.sock"
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

	// defaultMemAgentAddr matches netshim's own default bridge gateway
	// (see internal/netshim's defaultBridgeCIDR): the guest-side agent
	// (deploy/guest-image's kontur-mem-agent) has no way to learn a
	// nonstandard listen address at boot, so it always signals its
	// default route's gateway on this fixed port. Overriding
	// NETSHIM_BRIDGE_CIDR's gateway therefore also requires overriding
	// this to match, or the guest's signals go nowhere.
	defaultMemAgentAddr     = "169.254.100.1:30090"
	defaultMemAgentStepMB   = 256
	defaultMemAgentCooldown = 30 * time.Second

	// defaultDiskImage is the guest disk image baked into the kontur OCI
	// image itself (see the Dockerfile's guest-image stage): a minimal
	// Debian system with sshd, usable out of the box without a
	// separately-managed disk image. CHV_DISK_IMAGE overrides this for
	// any other guest.
	defaultDiskImage = "/var/lib/kontur/guest/disk.img"
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

	// Direct kernel boot. Kernel is mutually exclusive with Firmware:
	// exactly one of the two must be set.
	Kernel    string
	Initramfs string
	Firmware  string
	Cmdline   string

	CPUs int

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

	// Net is passed through verbatim as the value of --net, e.g.
	// "tap=eth0,mac=02:00:00:00:00:01". Left empty, the VM boots with no
	// network device.
	Net string

	APISocket  string
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
	// from the guest since it shares netshim's bridge network (see
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
// The disk image (and kernel/firmware, if set) are expected to already be
// present on the local filesystem: this runtime never fetches images, so
// startup only pays for booting the VM itself. CHV_DISK_IMAGE defaults to
// the guest image baked into the kontur OCI image (defaultDiskImage); set
// it explicitly to boot a different disk instead.
func FromEnv() (Config, error) {
	cfg := Config{
		Kernel:       os.Getenv(envKernel),
		Initramfs:    os.Getenv(envInitramfs),
		Firmware:     os.Getenv(envFirmware),
		Cmdline:      getEnvDefault(envCmdline, defaultCmdline),
		Net:          os.Getenv(envNet),
		APISocket:    getEnvDefault(envAPISocket, defaultAPISocket),
		BinaryPath:   getEnvDefault(envBinaryPath, defaultBinaryPath),
		MemoryShared: true,
	}

	diskPath := getEnvDefault(envDiskImage, defaultDiskImage)
	readonly, err := getEnvBool(envDiskReadonly, false)
	if err != nil {
		return Config{}, err
	}
	cfg.Disks = append(cfg.Disks, Disk{Path: diskPath, ReadOnly: readonly})

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

	if cfg.Kernel == "" && cfg.Firmware == "" {
		return Config{}, fmt.Errorf("one of %s or %s is required", envKernel, envFirmware)
	}
	if cfg.Kernel != "" && cfg.Firmware != "" {
		return Config{}, fmt.Errorf("%s and %s are mutually exclusive", envKernel, envFirmware)
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
