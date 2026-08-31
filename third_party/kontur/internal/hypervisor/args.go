package hypervisor

import (
	"fmt"
	"os"
	"strings"

	"github.com/bwsalmon/kontur/internal/config"
)

// SnapshotExists reports whether path already holds a complete VM
// snapshot for BuildArgs to restore from (see its "--restore" branch
// below) rather than booting fresh. Runner.Suspend only ever makes a
// snapshot visible at its final path by renaming a finished staging
// directory into place, so simple existence is enough to know it's
// complete.
func SnapshotExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// BuildArgs turns a Config into the argv for the cloud-hypervisor binary.
//
// It always boots and configures the VM in one invocation (no separate
// vm.create/vm.boot API calls) so there is nothing to do at startup beyond
// exec'ing the process: the disk image is assumed to already be present on
// the local filesystem, so there is no image fetch on the hot path either.
func BuildArgs(cfg config.Config) []string {
	var args []string

	args = append(args, "--api-socket", "path="+cfg.APISocket)

	if SnapshotExists(cfg.SnapshotPath) {
		// A restored VM's CPUs, memory, disks, kernel/firmware, net and
		// console are all replayed from the snapshot's own config.json
		// rather than given again here -- see Runner.Suspend and
		// cloud-hypervisor's snapshot/restore docs. "resume=true" starts
		// it running immediately, matching the running state it was in
		// right before Suspend paused it to take the snapshot.
		args = append(args, "--restore", "source_url=file://"+cfg.SnapshotPath+",resume=true")
		return args
	}

	args = append(args, "--cpus", cpusArg(cfg))
	args = append(args, "--memory", memoryArg(cfg))

	for _, d := range cfg.Disks {
		// image_type=raw is a local addition (bwsalmon/agents#478, see
		// this repo's own VENDORED.md) -- every disk this runtime was
		// originally ever pointed at was a plain raw image (config.go's
		// own defaultDiskImage, and every guest packer/kontur/build.sh
		// produces), and without this hint cloud-hypervisor's own format
		// auto-detection refuses writes to sector 0 ("Attempting to
		// write to sector 0 on a disk without specifying image_type"),
		// which fails the guest's root-filesystem mount outright.
		// Confirmed by hand against a real cloud-hypervisor v53.0
		// binary: the exact same disk boots cleanly once this is added.
		//
		// bwsalmon/agents#510's own vendor bump introduced a second kind
		// of disk this same runtime is pointed at: the qcow2 writable
		// overlay staticpod.PrepareWritableDisk creates for
		// -disk-readonly=false (internal/staticpod/qcow2.go upstream,
		// always named disk.qcow2 -- staticpod.writableDiskFileName),
		// backed by the shared read-only source image. Forcing
		// image_type=raw on that disk made cloud-hypervisor refuse to
		// open it at all ("Maximum disk nesting depth exceeded"), and
		// switching to the correct image_type=qcow2 alone did not fix
		// that -- cloud-hypervisor v53.0 also needs backing_files=on to
		// actually follow a qcow2 file's backing-file chain at all
		// (undocumented beyond the --disk flag's own --help listing it
		// as an option); without it, a backing file counts as "nesting"
		// no depth budget allows, regardless of image_type. Confirmed by
		// hand end to end: with both set, a real writable-disk VM boots,
		// SSHes in, and a write made through the guest lands in the
		// overlay (confirmed by its file size growing) while the shared
		// source image file underneath (mounted read-only regardless --
		// see ImagesMountPath's own doc comment) is untouched. Every raw
		// disk in this tree keeps the extension config.go's
		// defaultDiskImage and packer/kontur/build.sh's own output
		// already use (".img"), so this distinction never mistakes one
		// kind of disk for the other.
		extra := ",image_type=raw"
		if strings.HasSuffix(d.Path, ".qcow2") {
			extra = ",image_type=qcow2,backing_files=on"
		}
		args = append(args, "--disk", fmt.Sprintf("path=%s,readonly=%s%s", d.Path, onOff(d.ReadOnly), extra))
	}

	if cfg.Kernel != "" {
		args = append(args, "--kernel", cfg.Kernel)
		if cfg.Initramfs != "" {
			args = append(args, "--initramfs", cfg.Initramfs)
		}
		if cfg.Cmdline != "" {
			args = append(args, "--cmdline", cfg.Cmdline)
		}
	} else {
		args = append(args, "--firmware", cfg.Firmware)
	}

	if cfg.Net != "" {
		args = append(args, "--net", cfg.Net)
	}

	// Route the guest's serial console to our own stdio (so it shows up
	// under `kubectl logs`) and disable the redundant virtio-console.
	args = append(args, "--serial", "tty")
	args = append(args, "--console", "off")

	args = append(args, cfg.ExtraArgs...)

	return args
}

// memoryArg builds --memory's value: a fixed starting size, plus (when
// MemoryHotplug is set and there's actually room to grow into) a
// virtio-mem hotplug device sized for growth up to MemoryMaxMB. virtio-mem
// is used over cloud-hypervisor's other hotplug mechanism (ACPI-based DIMM
// hotplug) because it needs no guest-side udev rule to online newly added
// memory and supports shrinking back down again, not just growing -- at
// the cost of requiring a guest kernel built with CONFIG_VIRTIO_MEM
// (Linux 5.8+) to actually make use of it; a guest without that support
// still boots fine at MemoryMB, it just can't grow beyond it. Live
// resizing (up to MemoryMaxMB, down to MemoryMB) works via the
// cloud-hypervisor API's vm.resize (see APIClient.Resize / "kontur
// resize").
func memoryArg(cfg config.Config) string {
	arg := fmt.Sprintf("size=%dM,shared=%s", cfg.MemoryMB, onOff(cfg.MemoryShared))
	if cfg.MemoryHotplug && cfg.MemoryMaxMB > cfg.MemoryMB {
		arg += fmt.Sprintf(",hotplug_method=virtio-mem,hotplug_size=%dM", cfg.MemoryMaxMB-cfg.MemoryMB)
	}
	return arg
}

// cpusArg builds --cpus's value: a fixed boot count, plus (when CPUsMax
// is greater than CPUs) a "max=" ceiling that turns on cloud-
// hypervisor's ACPI-based CPU hotplug for growth up to CPUsMax. Unlike
// memory hotplug, there is no separate hotplug_method or enable flag to
// set -- cloud-hypervisor treats any max greater than boot as hotplug-
// capable on its own. Live resizing (up to CPUsMax, down to 1) works via
// the cloud-hypervisor API's vm.resize (see APIClient.ResizeCPUs /
// "kontur resize"). See the README's "CPU hotplug" section for how this
// interacts with Snapshot/suspend.
func cpusArg(cfg config.Config) string {
	arg := fmt.Sprintf("boot=%d", cfg.CPUs)
	if cfg.CPUsMax > cfg.CPUs {
		arg += fmt.Sprintf(",max=%d", cfg.CPUsMax)
	}
	return arg
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// String renders argv the way it would be typed on a shell command line,
// for logging.
func String(binary string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, binary)
	for _, a := range args {
		if strings.ContainsAny(a, " \t\"'") {
			parts = append(parts, fmt.Sprintf("%q", a))
		} else {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}
