package hypervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/kontur/internal/config"
)

func baseConfig() config.Config {
	return config.Config{
		Disks:           []config.Disk{{Path: "/data/disk.img"}},
		Firmware:        "/opt/firmware/CLOUDHV.fd",
		CPUs:            2,
		MemoryMB:        2048,
		MemoryShared:    true,
		APISocket:       "/run/cloud-hypervisor/api.sock",
		BinaryPath:      "/usr/local/bin/cloud-hypervisor",
		ShutdownTimeout: 20 * time.Second,
	}
}

// argValue returns the value that follows a --flag in args, failing the
// test if the flag is absent.
func argValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) {
				t.Fatalf("flag %s has no value in %v", flag, args)
			}
			return args[i+1]
		}
	}
	t.Fatalf("flag %s not found in %v", flag, args)
	return ""
}

func TestBuildArgs_FirmwareBoot(t *testing.T) {
	cfg := baseConfig()
	args := BuildArgs(cfg)

	if got := argValue(t, args, "--firmware"); got != cfg.Firmware {
		t.Errorf("--firmware = %q, want %q", got, cfg.Firmware)
	}
	for _, flag := range []string{"--kernel", "--initramfs", "--cmdline"} {
		for _, a := range args {
			if a == flag {
				t.Errorf("did not expect %s when Firmware is set", flag)
			}
		}
	}
	if got := argValue(t, args, "--disk"); got != "path=/data/disk.img,readonly=off,image_type=raw" {
		t.Errorf("--disk = %q, want path=/data/disk.img,readonly=off,image_type=raw", got)
	}
	if got := argValue(t, args, "--cpus"); got != "boot=2" {
		t.Errorf("--cpus = %q, want boot=2", got)
	}
	if got := argValue(t, args, "--memory"); got != "size=2048M,shared=on" {
		t.Errorf("--memory = %q, want size=2048M,shared=on", got)
	}
	if got := argValue(t, args, "--api-socket"); got != "path=/run/cloud-hypervisor/api.sock" {
		t.Errorf("--api-socket = %q, want path=/run/cloud-hypervisor/api.sock", got)
	}
	if got := argValue(t, args, "--serial"); got != "tty" {
		t.Errorf("--serial = %q, want tty", got)
	}
	if got := argValue(t, args, "--console"); got != "off" {
		t.Errorf("--console = %q, want off", got)
	}
}

func TestBuildArgs_CPUHotplugAddsMaxVcpus(t *testing.T) {
	cfg := baseConfig()
	cfg.CPUs = 2
	cfg.CPUsMax = 8

	args := BuildArgs(cfg)

	if got := argValue(t, args, "--cpus"); got != "boot=2,max=8" {
		t.Errorf("--cpus = %q, want boot=2,max=8", got)
	}
}

func TestBuildArgs_CPUHotplugWithNoGrowthRoomOmitsMax(t *testing.T) {
	cfg := baseConfig()
	cfg.CPUs = 2
	cfg.CPUsMax = 2

	args := BuildArgs(cfg)

	if got := argValue(t, args, "--cpus"); got != "boot=2" {
		t.Errorf("--cpus = %q, want boot=2 (no max= when there's no room to grow into)", got)
	}
}

func TestBuildArgs_MemoryHotplugAddsVirtioMemDevice(t *testing.T) {
	cfg := baseConfig()
	cfg.MemoryMB = 256
	cfg.MemoryMaxMB = 2048
	cfg.MemoryHotplug = true

	args := BuildArgs(cfg)

	if got := argValue(t, args, "--memory"); got != "size=256M,shared=on,hotplug_method=virtio-mem,hotplug_size=1792M" {
		t.Errorf("--memory = %q, want size=256M,shared=on,hotplug_method=virtio-mem,hotplug_size=1792M", got)
	}
}

func TestBuildArgs_MemoryHotplugDisabledOmitsDevice(t *testing.T) {
	cfg := baseConfig()
	cfg.MemoryMB = 256
	cfg.MemoryMaxMB = 2048
	cfg.MemoryHotplug = false

	args := BuildArgs(cfg)

	if got := argValue(t, args, "--memory"); got != "size=256M,shared=on" {
		t.Errorf("--memory = %q, want size=256M,shared=on", got)
	}
}

func TestBuildArgs_MemoryHotplugWithNoGrowthRoomOmitsDevice(t *testing.T) {
	cfg := baseConfig()
	cfg.MemoryMB = 2048
	cfg.MemoryMaxMB = 2048
	cfg.MemoryHotplug = true

	args := BuildArgs(cfg)

	if got := argValue(t, args, "--memory"); got != "size=2048M,shared=on" {
		t.Errorf("--memory = %q, want size=2048M,shared=on (no hotplug device when there's no room to grow into)", got)
	}
}

func TestBuildArgs_KernelBoot(t *testing.T) {
	cfg := baseConfig()
	cfg.Firmware = ""
	cfg.Kernel = "/boot/vmlinux"
	cfg.Initramfs = "/boot/initrd"
	cfg.Cmdline = "console=ttyS0 root=/dev/vda rw"

	args := BuildArgs(cfg)

	if got := argValue(t, args, "--kernel"); got != cfg.Kernel {
		t.Errorf("--kernel = %q, want %q", got, cfg.Kernel)
	}
	if got := argValue(t, args, "--initramfs"); got != cfg.Initramfs {
		t.Errorf("--initramfs = %q, want %q", got, cfg.Initramfs)
	}
	if got := argValue(t, args, "--cmdline"); got != cfg.Cmdline {
		t.Errorf("--cmdline = %q, want %q", got, cfg.Cmdline)
	}
	for _, a := range args {
		if a == "--firmware" {
			t.Errorf("did not expect --firmware when Kernel is set")
		}
	}
}

func TestBuildArgs_MultipleDisksAndNet(t *testing.T) {
	cfg := baseConfig()
	cfg.Disks = append(cfg.Disks, config.Disk{Path: "/data/extra.img", ReadOnly: true})
	cfg.Net = "tap=eth0,mac=02:00:00:00:00:01"

	args := BuildArgs(cfg)

	var disks []string
	for i, a := range args {
		if a == "--disk" {
			disks = append(disks, args[i+1])
		}
	}
	want := []string{"path=/data/disk.img,readonly=off,image_type=raw", "path=/data/extra.img,readonly=on,image_type=raw"}
	if len(disks) != len(want) {
		t.Fatalf("disks = %v, want %v", disks, want)
	}
	for i := range want {
		if disks[i] != want[i] {
			t.Errorf("disk[%d] = %q, want %q", i, disks[i], want[i])
		}
	}
	if got := argValue(t, args, "--net"); got != cfg.Net {
		t.Errorf("--net = %q, want %q", got, cfg.Net)
	}
}

// A writable disk overlay (staticpod.PrepareWritableDisk) is qcow2, not
// raw, and must not get the raw image_type every other disk here gets --
// cloud-hypervisor refuses to open it at all if it does ("Maximum disk
// nesting depth exceeded", confirmed by hand against a real
// cloud-hypervisor v53.0 binary), and needs backing_files=on before it
// will follow the overlay's backing-file chain.
func TestBuildArgs_Qcow2DiskGetsQcow2ImageTypeAndBackingFiles(t *testing.T) {
	cfg := baseConfig()
	cfg.Disks = []config.Disk{{Path: "/disk/disk.qcow2"}}

	args := BuildArgs(cfg)

	if got := argValue(t, args, "--disk"); got != "path=/disk/disk.qcow2,readonly=off,image_type=qcow2,backing_files=on" {
		t.Errorf("--disk = %q, want path=/disk/disk.qcow2,readonly=off,image_type=qcow2,backing_files=on", got)
	}
}

func TestBuildArgs_NoNetFlagWhenUnset(t *testing.T) {
	args := BuildArgs(baseConfig())
	for _, a := range args {
		if a == "--net" {
			t.Errorf("did not expect --net flag when Net is unset")
		}
	}
}

func TestBuildArgs_ExtraArgsAppended(t *testing.T) {
	cfg := baseConfig()
	cfg.ExtraArgs = []string{"--watchdog", "--rng", "src=/dev/urandom"}

	args := BuildArgs(cfg)
	tail := args[len(args)-len(cfg.ExtraArgs):]
	if strings.Join(tail, " ") != strings.Join(cfg.ExtraArgs, " ") {
		t.Errorf("extra args not appended verbatim at the end: got %v", args)
	}
}

func TestBuildArgs_RestoresFromExistingSnapshot(t *testing.T) {
	cfg := baseConfig()
	cfg.SnapshotPath = filepath.Join(t.TempDir(), "snapshot")
	if err := os.Mkdir(cfg.SnapshotPath, 0o755); err != nil {
		t.Fatal(err)
	}

	args := BuildArgs(cfg)

	if got := argValue(t, args, "--api-socket"); got != "path="+cfg.APISocket {
		t.Errorf("--api-socket = %q, want path=%s", got, cfg.APISocket)
	}
	want := "source_url=file://" + cfg.SnapshotPath + ",resume=true"
	if got := argValue(t, args, "--restore"); got != want {
		t.Errorf("--restore = %q, want %q", got, want)
	}
	for _, flag := range []string{"--cpus", "--memory", "--disk", "--firmware", "--kernel", "--serial", "--console"} {
		for _, a := range args {
			if a == flag {
				t.Errorf("did not expect %s when restoring from a snapshot", flag)
			}
		}
	}
}

func TestBuildArgs_BootsFreshWhenSnapshotPathUnsetOrMissing(t *testing.T) {
	cfg := baseConfig()
	args := BuildArgs(cfg)
	for _, a := range args {
		if a == "--restore" {
			t.Fatalf("did not expect --restore with no SnapshotPath: %v", args)
		}
	}

	cfg.SnapshotPath = filepath.Join(t.TempDir(), "does-not-exist")
	args = BuildArgs(cfg)
	for _, a := range args {
		if a == "--restore" {
			t.Fatalf("did not expect --restore when SnapshotPath does not exist yet: %v", args)
		}
	}
}

func TestSnapshotExists(t *testing.T) {
	if SnapshotExists("") {
		t.Error("SnapshotExists(\"\") = true, want false")
	}

	missing := filepath.Join(t.TempDir(), "missing")
	if SnapshotExists(missing) {
		t.Error("SnapshotExists(missing) = true, want false")
	}

	present := filepath.Join(t.TempDir(), "present")
	if err := os.Mkdir(present, 0o755); err != nil {
		t.Fatal(err)
	}
	if !SnapshotExists(present) {
		t.Error("SnapshotExists(present) = false, want true")
	}
}

func TestString_QuotesArgsWithSpaces(t *testing.T) {
	got := String("cloud-hypervisor", []string{"--cmdline", "console=ttyS0 root=/dev/vda"})
	want := `cloud-hypervisor --cmdline "console=ttyS0 root=/dev/vda"`
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
