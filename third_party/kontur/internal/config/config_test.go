package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearEnv resets every env var this package reads, so tests don't leak
// into each other or pick up the outer environment.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		envDiskImage, envDiskReadonly, envExtraDisks, envKernel, envInitramfs,
		envFirmware, envCmdline, envCPUs, envMemoryMB, envMemoryShared, envNet,
		envAPISocket, envBinaryPath, envExtraArgs, envShutdownTimeout,
	} {
		os.Unsetenv(k)
	}
}

func writeTempFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFromEnv_MinimalKernelBoot(t *testing.T) {
	clearEnv(t)
	disk := writeTempFile(t, "disk.img")
	kernel := writeTempFile(t, "vmlinux")
	t.Setenv(envDiskImage, disk)
	t.Setenv(envKernel, kernel)

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}

	if len(cfg.Disks) != 1 || cfg.Disks[0].Path != disk || cfg.Disks[0].ReadOnly {
		t.Errorf("Disks = %+v, want single rw disk at %s", cfg.Disks, disk)
	}
	if cfg.Kernel != kernel {
		t.Errorf("Kernel = %q, want %q", cfg.Kernel, kernel)
	}
	if cfg.CPUs != defaultCPUs {
		t.Errorf("CPUs = %d, want default %d", cfg.CPUs, defaultCPUs)
	}
	if cfg.MemoryMB != defaultMemoryMB {
		t.Errorf("MemoryMB = %d, want default %d", cfg.MemoryMB, defaultMemoryMB)
	}
	if cfg.ShutdownTimeout != defaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %s, want default %s", cfg.ShutdownTimeout, defaultShutdownTimeout)
	}
}

func TestFromEnv_DiskImageDefaultsToBundledGuestImage(t *testing.T) {
	clearEnv(t)
	t.Setenv(envFirmware, writeTempFile(t, "fw"))

	// CHV_DISK_IMAGE is left unset: FromEnv should fall back to
	// defaultDiskImage (which won't exist in a test environment) rather
	// than reporting it as missing outright.
	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for nonexistent default disk image, got nil")
	}
	if !strings.Contains(err.Error(), defaultDiskImage) {
		t.Errorf("error = %v, want it to mention the default disk path %q", err, defaultDiskImage)
	}
}

func TestFromEnv_RequiresKernelOrFirmware(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when neither kernel nor firmware is set, got nil")
	}
}

func TestFromEnv_KernelAndFirmwareMutuallyExclusive(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envKernel, writeTempFile(t, "vmlinux"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when both kernel and firmware are set, got nil")
	}
}

func TestFromEnv_MissingPathsRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, filepath.Join(t.TempDir(), "does-not-exist.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for nonexistent disk image, got nil")
	}
}

func TestFromEnv_ExtraDisksAndOverrides(t *testing.T) {
	clearEnv(t)
	disk := writeTempFile(t, "disk.img")
	extra1 := writeTempFile(t, "extra1.img")
	extra2 := writeTempFile(t, "extra2.img")
	t.Setenv(envDiskImage, disk)
	t.Setenv(envDiskReadonly, "true")
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envExtraDisks, extra1+":ro,"+extra2+":rw")
	t.Setenv(envCPUs, "4")
	t.Setenv(envMemoryMB, "8192")
	t.Setenv(envMemoryShared, "false")
	t.Setenv(envNet, "tap=eth0")
	t.Setenv(envAPISocket, "/tmp/custom.sock")
	t.Setenv(envBinaryPath, "/opt/cloud-hypervisor")
	t.Setenv(envExtraArgs, "--rng src=/dev/urandom --watchdog")
	t.Setenv(envShutdownTimeout, "5s")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}

	wantDisks := []Disk{
		{Path: disk, ReadOnly: true},
		{Path: extra1, ReadOnly: true},
		{Path: extra2, ReadOnly: false},
	}
	if len(cfg.Disks) != len(wantDisks) {
		t.Fatalf("Disks = %+v, want %+v", cfg.Disks, wantDisks)
	}
	for i, d := range wantDisks {
		if cfg.Disks[i] != d {
			t.Errorf("Disks[%d] = %+v, want %+v", i, cfg.Disks[i], d)
		}
	}
	if cfg.CPUs != 4 {
		t.Errorf("CPUs = %d, want 4", cfg.CPUs)
	}
	if cfg.MemoryMB != 8192 {
		t.Errorf("MemoryMB = %d, want 8192", cfg.MemoryMB)
	}
	if cfg.MemoryShared {
		t.Errorf("MemoryShared = true, want false")
	}
	if cfg.Net != "tap=eth0" {
		t.Errorf("Net = %q, want tap=eth0", cfg.Net)
	}
	if cfg.APISocket != "/tmp/custom.sock" {
		t.Errorf("APISocket = %q, want /tmp/custom.sock", cfg.APISocket)
	}
	if cfg.BinaryPath != "/opt/cloud-hypervisor" {
		t.Errorf("BinaryPath = %q, want /opt/cloud-hypervisor", cfg.BinaryPath)
	}
	wantArgs := []string{"--rng", "src=/dev/urandom", "--watchdog"}
	if len(cfg.ExtraArgs) != len(wantArgs) {
		t.Fatalf("ExtraArgs = %v, want %v", cfg.ExtraArgs, wantArgs)
	}
	for i, a := range wantArgs {
		if cfg.ExtraArgs[i] != a {
			t.Errorf("ExtraArgs[%d] = %q, want %q", i, cfg.ExtraArgs[i], a)
		}
	}
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 5s", cfg.ShutdownTimeout)
	}
}

func TestFromEnv_InvalidExtraDiskMode(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envExtraDisks, writeTempFile(t, "extra.img")+":bogus")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for invalid extra disk mode, got nil")
	}
}

func TestFromEnv_RejectsTooFewCPUs(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envCPUs, "0")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for 0 cpus, got nil")
	}
}
