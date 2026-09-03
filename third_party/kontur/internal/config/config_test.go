package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/kontur/internal/qcow2"
)

// clearEnv resets every env var this package reads, so tests don't leak
// into each other or pick up the outer environment.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		envDiskImage, envDiskReadonly, envDiskMode, envDiskOverlayPath, envDiskSizeMB, envExtraDisks, envKernel, envInitramfs,
		envFirmware, envCmdline, envCPUs, envCPUsMax, envMemoryMB, envMemoryMaxMB,
		envMemoryHotplug, envMemoryShared, envNet,
		envAPISocket, envBinaryPath, envExtraArgs, envShutdownTimeout,
		envSetupScript, envSnapshotPath,
		envMemAgent, envMemAgentAddr, envMemAgentStepMB, envMemAgentCooldown,
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
	if cfg.CPUsMax != defaultCPUs {
		t.Errorf("CPUsMax = %d, want default %d (no hotplug headroom unless CHV_CPUS_MAX is set)", cfg.CPUsMax, defaultCPUs)
	}
	if cfg.MemoryMB != defaultMemoryMB {
		t.Errorf("MemoryMB = %d, want default %d", cfg.MemoryMB, defaultMemoryMB)
	}
	if cfg.MemoryMaxMB != defaultMemoryMaxMB {
		t.Errorf("MemoryMaxMB = %d, want default %d", cfg.MemoryMaxMB, defaultMemoryMaxMB)
	}
	if !cfg.MemoryHotplug {
		t.Errorf("MemoryHotplug = false, want default true")
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

func TestFromEnv_KernelDefaultsToBundledKernel(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))

	// Neither CHV_KERNEL nor CHV_FIRMWARE is set: FromEnv should fall
	// back to defaultKernel (which won't exist in a test environment)
	// rather than reporting one of the two as required.
	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for nonexistent default kernel, got nil")
	}
	if !strings.Contains(err.Error(), defaultKernel) {
		t.Errorf("error = %v, want it to mention the default kernel path %q", err, defaultKernel)
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
	if len(cfg.Nets) != 1 || cfg.Nets[0] != "tap=eth0" {
		t.Errorf("Nets = %q, want [tap=eth0]", cfg.Nets)
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

func TestFromEnv_SetupScriptWithoutSnapshotPath(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envSetupScript, "echo hi")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v, want CHV_SETUP_SCRIPT to be usable without CHV_SNAPSHOT_PATH", err)
	}
	if cfg.SetupScript != "echo hi" {
		t.Errorf("SetupScript = %q, want %q", cfg.SetupScript, "echo hi")
	}
	if cfg.SnapshotPath != "" {
		t.Errorf("SnapshotPath = %q, want empty", cfg.SnapshotPath)
	}
}

func TestFromEnv_SnapshotPathMustBeAbsolute(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envSnapshotPath, "relative/path")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for a relative CHV_SNAPSHOT_PATH, got nil")
	}
}

func TestFromEnv_SnapshotPathParentMustExist(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envSnapshotPath, filepath.Join(t.TempDir(), "does-not-exist", "snapshot"))

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when CHV_SNAPSHOT_PATH's parent directory doesn't exist, got nil")
	}
}

func TestFromEnv_SetupScriptAndSnapshotPath(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envSetupScript, "apt-get install -y foo")
	t.Setenv(envSnapshotPath, filepath.Join(t.TempDir(), "snapshot"))

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.SetupScript != "apt-get install -y foo" {
		t.Errorf("SetupScript = %q, want %q", cfg.SetupScript, "apt-get install -y foo")
	}
	if cfg.SnapshotPath == "" {
		t.Error("SnapshotPath is empty, want it set")
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

func TestFromEnv_CPUsMaxDefaultsToCPUsWhenOverridden(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envCPUs, "4")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.CPUsMax != 4 {
		t.Errorf("CPUsMax = %d, want 4 (tracking the overridden boot count, since CHV_CPUS_MAX was left unset)", cfg.CPUsMax)
	}
}

func TestFromEnv_CPUsMaxExplicit(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envCPUs, "2")
	t.Setenv(envCPUsMax, "8")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.CPUs != 2 {
		t.Errorf("CPUs = %d, want 2", cfg.CPUs)
	}
	if cfg.CPUsMax != 8 {
		t.Errorf("CPUsMax = %d, want 8", cfg.CPUsMax)
	}
}

func TestFromEnv_RejectsCPUsMaxBelowCPUs(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envCPUs, "4")
	t.Setenv(envCPUsMax, "2")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when CHV_CPUS_MAX is below CHV_CPUS, got nil")
	}
}

func TestFromEnv_MemoryMaxDefaultTracksOverriddenStartingSize(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envMemoryMB, "4096")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.MemoryMaxMB != 4096 {
		t.Errorf("MemoryMaxMB = %d, want 4096 (tracking the overridden starting size, since it exceeds the default max)", cfg.MemoryMaxMB)
	}
}

func TestFromEnv_MemoryHotplugExplicit(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envMemoryMB, "512")
	t.Setenv(envMemoryMaxMB, "1024")
	t.Setenv(envMemoryHotplug, "false")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.MemoryMB != 512 {
		t.Errorf("MemoryMB = %d, want 512", cfg.MemoryMB)
	}
	if cfg.MemoryMaxMB != 1024 {
		t.Errorf("MemoryMaxMB = %d, want 1024", cfg.MemoryMaxMB)
	}
	if cfg.MemoryHotplug {
		t.Errorf("MemoryHotplug = true, want false")
	}
}

func TestFromEnv_RejectsMemoryMaxBelowStart(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envMemoryMB, "1024")
	t.Setenv(envMemoryMaxMB, "512")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when CHV_MEMORY_MAX_MB is below CHV_MEMORY_MB, got nil")
	}
}

func TestFromEnv_HotplugWithGrowthRoomRequiresSharedMemory(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envMemoryMB, "512")
	t.Setenv(envMemoryMaxMB, "1024")
	t.Setenv(envMemoryShared, "false")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for hotplug enabled with growth room but shared memory disabled, got nil")
	}

	// Disabling hotplug altogether lifts the shared-memory requirement,
	// since no virtio-mem device gets attached at all.
	t.Setenv(envMemoryHotplug, "false")
	if _, err := FromEnv(); err != nil {
		t.Errorf("FromEnv() error = %v, want nil once hotplug is disabled", err)
	}
}

func TestFromEnv_MemAgentDefaultsOffAndDoesNotRequireHotplugUnlessEnabled(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envMemoryHotplug, "false")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.MemAgent {
		t.Errorf("MemAgent = true, want false by default")
	}
	if cfg.MemAgentAddr != defaultMemAgentAddr {
		t.Errorf("MemAgentAddr = %q, want %q", cfg.MemAgentAddr, defaultMemAgentAddr)
	}
	if cfg.MemAgentStepMB != defaultMemAgentStepMB {
		t.Errorf("MemAgentStepMB = %d, want %d", cfg.MemAgentStepMB, defaultMemAgentStepMB)
	}
	if cfg.MemAgentCooldown != defaultMemAgentCooldown {
		t.Errorf("MemAgentCooldown = %s, want %s", cfg.MemAgentCooldown, defaultMemAgentCooldown)
	}
}

func TestFromEnv_MemAgentRequiresMemoryHotplug(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envMemAgent, "true")
	t.Setenv(envMemoryHotplug, "false")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for CHV_MEM_AGENT=true with memory hotplug disabled, got nil")
	}
}

func TestFromEnv_MemAgentExplicitValues(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envMemAgent, "true")
	t.Setenv(envMemAgentAddr, "169.254.100.1:12345")
	t.Setenv(envMemAgentStepMB, "128")
	t.Setenv(envMemAgentCooldown, "1m")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if !cfg.MemAgent {
		t.Errorf("MemAgent = false, want true")
	}
	if cfg.MemAgentAddr != "169.254.100.1:12345" {
		t.Errorf("MemAgentAddr = %q, want 169.254.100.1:12345", cfg.MemAgentAddr)
	}
	if cfg.MemAgentStepMB != 128 {
		t.Errorf("MemAgentStepMB = %d, want 128", cfg.MemAgentStepMB)
	}
	if cfg.MemAgentCooldown != time.Minute {
		t.Errorf("MemAgentCooldown = %s, want 1m", cfg.MemAgentCooldown)
	}
}

func TestFromEnv_RejectsNonPositiveMemAgentStepMB(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envMemAgentStepMB, "0")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for CHV_MEM_AGENT_STEP_MB=0, got nil")
	}
}

func TestBakedInKernelPrefersTheGuestsOwn(t *testing.T) {
	// A guest built with GUEST_KERNEL_PACKAGE carries its own kernel and
	// initramfs beside its disk; booting that rootfs under the
	// fetch-kernel build instead would give it a kernel whose modules
	// are not the ones in its /lib/modules.
	dir := t.TempDir()
	guestKernel := filepath.Join(dir, "vmlinuz")
	guestInitramfs := filepath.Join(dir, "initrd.img")
	fallback := filepath.Join(dir, "vmlinux")
	for _, p := range []string{guestKernel, guestInitramfs, fallback} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	kernel, initramfs := bakedInKernel(guestKernel, guestInitramfs, fallback)
	if kernel != guestKernel {
		t.Errorf("kernel = %q, want the guest's own %q", kernel, guestKernel)
	}
	if initramfs != guestInitramfs {
		t.Errorf("initramfs = %q, want %q", initramfs, guestInitramfs)
	}
}

func TestBakedInKernelFallsBackWithoutBothHalves(t *testing.T) {
	// Half a pair is not a guest kernel: a vmlinuz with no initrd boots
	// without the modules and udev rules the initramfs carries, which
	// fails as an unreachable guest rather than as a boot error. The
	// default build has neither half, which is the same case.
	dir := t.TempDir()
	fallback := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(fallback, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	guestKernel := filepath.Join(dir, "vmlinuz")

	for _, tc := range []struct {
		name    string
		present []string
	}{
		{"neither", nil},
		{"kernel only", []string{guestKernel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, p := range tc.present {
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove(p) })
			}
			kernel, initramfs := bakedInKernel(guestKernel, filepath.Join(dir, "initrd.img"), fallback)
			if kernel != fallback {
				t.Errorf("kernel = %q, want the fallback %q", kernel, fallback)
			}
			if initramfs != "" {
				t.Errorf("initramfs = %q, want none", initramfs)
			}
		})
	}
}

func TestDiskModeDefaultsToOverlay(t *testing.T) {
	// The default matters more than most: it is what every VM that says
	// nothing gets, and the alternative -- writing through to the disk
	// image -- costs a copy of that image on the guest's first write and
	// makes two VMs on one host contend for the same file.
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.DiskMode != DiskModeOverlay {
		t.Errorf("DiskMode = %q, want %q", cfg.DiskMode, DiskModeOverlay)
	}
	if cfg.Disks[0].ReadOnly {
		t.Error("Disks[0].ReadOnly = true, want false: an overlay-backed disk is attached writable")
	}
}

func TestDiskModeFallsBackToTheBoolean(t *testing.T) {
	// CHV_DISK_READONLY=false has always meant "write through to the disk
	// image" in this container, so a deployment still setting it must keep
	// getting exactly that rather than quietly acquiring a throw-away
	// overlay.
	for _, tc := range []struct {
		readonly string
		want     string
	}{
		{"false", DiskModePersistent},
		{"true", DiskModeReadOnly},
	} {
		t.Run(tc.readonly, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
			t.Setenv(envFirmware, writeTempFile(t, "fw"))
			t.Setenv(envDiskReadonly, tc.readonly)

			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv() error = %v", err)
			}
			if cfg.DiskMode != tc.want {
				t.Errorf("DiskMode = %q, want %q", cfg.DiskMode, tc.want)
			}
		})
	}
}

func TestDiskModeRejectsContradictions(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envDiskMode, DiskModeOverlay)
	t.Setenv(envDiskReadonly, "true")

	if _, err := FromEnv(); err == nil {
		t.Error("FromEnv() = nil, want an error: the two settings contradict each other and guessing is worse than saying so")
	}
}

func TestDiskModeRejectsUnknownModes(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))
	t.Setenv(envDiskMode, "writable")

	if _, err := FromEnv(); err == nil {
		t.Error("FromEnv() = nil, want an error naming the three valid modes")
	}
}

func TestPrepareOverlayCreatesAndThenReusesTheOverlay(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(source, []byte("source disk contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "overlay", "disk.qcow2")

	cfg := Config{
		DiskMode:    DiskModeOverlay,
		OverlayPath: overlay,
		Disks:       []Disk{{Path: source}},
	}
	if err := cfg.PrepareOverlay(); err != nil {
		t.Fatalf("PrepareOverlay() error = %v", err)
	}
	if cfg.Disks[0].Path != overlay {
		t.Errorf("Disks[0].Path = %q, want the overlay %q", cfg.Disks[0].Path, overlay)
	}
	// The suffix is not cosmetic: hypervisor.Args keys image_type and
	// backing_files off it, and a qcow2 passed as raw does not boot.
	if filepath.Ext(cfg.Disks[0].Path) != ".qcow2" {
		t.Errorf("overlay path %q must end in .qcow2", cfg.Disks[0].Path)
	}

	// A restarted container must find the guest's writes still there, so
	// a second call reuses the overlay rather than starting it over.
	if err := os.WriteFile(overlay, []byte("written by the guest"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2 := Config{DiskMode: DiskModeOverlay, OverlayPath: overlay, Disks: []Disk{{Path: source}}}
	if err := cfg2.PrepareOverlay(); err != nil {
		t.Fatalf("PrepareOverlay() second call error = %v", err)
	}
	data, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "written by the guest" {
		t.Error("PrepareOverlay() recreated an existing overlay, discarding whatever the guest had written")
	}
}

func TestPrepareOverlayIsANoOpInTheOtherModes(t *testing.T) {
	for _, mode := range []string{DiskModePersistent, DiskModeReadOnly} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			overlay := filepath.Join(dir, "overlay", "disk.qcow2")
			cfg := Config{DiskMode: mode, OverlayPath: overlay, Disks: []Disk{{Path: "/images/disk.img"}}}
			if err := cfg.PrepareOverlay(); err != nil {
				t.Fatalf("PrepareOverlay() error = %v", err)
			}
			if cfg.Disks[0].Path != "/images/disk.img" {
				t.Errorf("Disks[0].Path = %q, want the source untouched", cfg.Disks[0].Path)
			}
			if _, err := os.Stat(overlay); !os.IsNotExist(err) {
				t.Error("an overlay was created in a mode that has none")
			}
		})
	}
}

func TestFromEnv_DiskSizeMB(t *testing.T) {
	clearEnv(t)
	t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
	t.Setenv(envFirmware, writeTempFile(t, "fw"))

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.DiskSizeMB != 0 {
		t.Errorf("DiskSizeMB = %d, want 0 (the disk image's own size) when %s is unset", cfg.DiskSizeMB, envDiskSizeMB)
	}

	t.Setenv(envDiskSizeMB, "8192")
	cfg, err = FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.DiskSizeMB != 8192 {
		t.Errorf("DiskSizeMB = %d, want 8192", cfg.DiskSizeMB)
	}
}

func TestFromEnv_DiskSizeMBRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		size  string
		mode  string
		wants string
	}{
		{name: "not a number", size: "8G", mode: DiskModeOverlay, wants: envDiskSizeMB},
		{name: "negative", size: "-1", mode: DiskModeOverlay, wants: "negative"},
		// The overlay is the only disk kontur creates; in the other two
		// modes the guest is on the image itself, which is shared and
		// never resized -- so asking for a size there can't be honoured.
		{name: "persistent mode", size: "8192", mode: DiskModePersistent, wants: DiskModeOverlay},
		{name: "readonly mode", size: "8192", mode: DiskModeReadOnly, wants: DiskModeOverlay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(envDiskImage, writeTempFile(t, "disk.img"))
			t.Setenv(envFirmware, writeTempFile(t, "fw"))
			t.Setenv(envDiskMode, tc.mode)
			t.Setenv(envDiskSizeMB, tc.size)

			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv() = nil, want an error for %s=%q with %s=%s", envDiskSizeMB, tc.size, envDiskMode, tc.mode)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("FromEnv() error = %v, want it to mention %q", err, tc.wants)
			}
		})
	}
}

// TestPrepareOverlaySizesANewOverlay: a VM told how big a disk it wants
// gets an overlay presenting exactly that, rather than the source image's
// size, from its very first boot.
func TestPrepareOverlaySizesANewOverlay(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(source, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "overlay", "disk.qcow2")

	cfg := Config{
		DiskMode:    DiskModeOverlay,
		OverlayPath: overlay,
		DiskSizeMB:  4096,
		Disks:       []Disk{{Path: source}},
	}
	if err := cfg.PrepareOverlay(); err != nil {
		t.Fatalf("PrepareOverlay() error = %v", err)
	}
	size, err := qcow2.VirtualSize(overlay)
	if err != nil {
		t.Fatalf("VirtualSize() error = %v", err)
	}
	if want := int64(4096) * 1024 * 1024; size != want {
		t.Errorf("overlay virtual size = %d, want %d", size, want)
	}
}

// TestPrepareOverlayGrowsAnExistingOverlay is the restart case: the
// overlay from an earlier boot is kept (whatever the guest wrote is in
// it), and raising the configured size grows it in place before the VM
// starts rather than being ignored until someone deletes the container.
func TestPrepareOverlayGrowsAnExistingOverlay(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(source, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "overlay", "disk.qcow2")

	first := Config{DiskMode: DiskModeOverlay, OverlayPath: overlay, DiskSizeMB: 1024, Disks: []Disk{{Path: source}}}
	if err := first.PrepareOverlay(); err != nil {
		t.Fatalf("PrepareOverlay() error = %v", err)
	}

	second := Config{DiskMode: DiskModeOverlay, OverlayPath: overlay, DiskSizeMB: 4096, Disks: []Disk{{Path: source}}}
	if err := second.PrepareOverlay(); err != nil {
		t.Fatalf("PrepareOverlay() on an existing overlay error = %v", err)
	}
	size, err := qcow2.VirtualSize(overlay)
	if err != nil {
		t.Fatalf("VirtualSize() error = %v", err)
	}
	if want := int64(4096) * 1024 * 1024; size != want {
		t.Errorf("overlay virtual size = %d, want it grown to %d", size, want)
	}
	if second.Disks[0].Path != overlay {
		t.Errorf("Disks[0].Path = %q, want the overlay %q", second.Disks[0].Path, overlay)
	}

	// Shrinking would leave the guest's filesystem spanning clusters
	// past the new end, so a lowered size fails rather than boots.
	third := Config{DiskMode: DiskModeOverlay, OverlayPath: overlay, DiskSizeMB: 2048, Disks: []Disk{{Path: source}}}
	if err := third.PrepareOverlay(); err == nil {
		t.Error("PrepareOverlay() = nil for a shrink, want an error")
	}
}

// TestPrepareOverlayRejectsASizeSmallerThanTheImage: an overlay reads
// through to the image for everything the guest hasn't written, so one
// smaller than the image would present a truncated root filesystem.
func TestPrepareOverlayRejectsASizeSmallerThanTheImage(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(source, make([]byte, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "overlay", "disk.qcow2")

	cfg := Config{DiskMode: DiskModeOverlay, OverlayPath: overlay, DiskSizeMB: 1, Disks: []Disk{{Path: source}}}
	err := cfg.PrepareOverlay()
	if err == nil {
		t.Fatal("PrepareOverlay() = nil, want an error for a size below the source image's")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("PrepareOverlay() error = %v, want it to explain the guest's filesystem would be truncated", err)
	}
	if _, statErr := os.Stat(overlay); !os.IsNotExist(statErr) {
		t.Error("an overlay was created despite the rejected size")
	}
}
