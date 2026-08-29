// Package config loads the settings for a single cloud-hypervisor VM from
// environment variables, so the container can be driven entirely through a
// Kubernetes pod spec (env / envFrom) without a mounted config file.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	envDiskImage       = "CHV_DISK_IMAGE"
	envDiskReadonly    = "CHV_DISK_READONLY"
	envExtraDisks      = "CHV_EXTRA_DISKS"
	envKernel          = "CHV_KERNEL"
	envInitramfs       = "CHV_INITRAMFS"
	envFirmware        = "CHV_FIRMWARE"
	envCmdline         = "CHV_CMDLINE"
	envCPUs            = "CHV_CPUS"
	envMemoryMB        = "CHV_MEMORY_MB"
	envMemoryShared    = "CHV_MEMORY_SHARED"
	envNet             = "CHV_NET"
	envAPISocket       = "CHV_API_SOCKET"
	envBinaryPath      = "CHV_BINARY_PATH"
	envExtraArgs       = "CHV_EXTRA_ARGS"
	envShutdownTimeout = "CHV_SHUTDOWN_TIMEOUT"

	defaultCmdline         = "console=ttyS0 root=/dev/vda rw"
	defaultCPUs            = 2
	defaultMemoryMB        = 2048
	defaultAPISocket       = "/run/cloud-hypervisor/api.sock"
	defaultBinaryPath      = "/usr/local/bin/cloud-hypervisor"
	defaultShutdownTimeout = 20 * time.Second

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

	CPUs         int
	MemoryMB     int
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

	cfg.ShutdownTimeout, err = getEnvDuration(envShutdownTimeout, defaultShutdownTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.ExtraArgs = splitNonEmpty(os.Getenv(envExtraArgs), " ")

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
