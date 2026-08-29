package hypervisor

import (
	"fmt"
	"strings"

	"github.com/bwsalmon/kontur/internal/config"
)

// BuildArgs turns a Config into the argv for the cloud-hypervisor binary.
//
// It always boots and configures the VM in one invocation (no separate
// vm.create/vm.boot API calls) so there is nothing to do at startup beyond
// exec'ing the process: the disk image is assumed to already be present on
// the local filesystem, so there is no image fetch on the hot path either.
func BuildArgs(cfg config.Config) []string {
	var args []string

	args = append(args, "--api-socket", "path="+cfg.APISocket)

	args = append(args, "--cpus", fmt.Sprintf("boot=%d", cfg.CPUs))
	args = append(args, "--memory", fmt.Sprintf("size=%dM,shared=%s", cfg.MemoryMB, onOff(cfg.MemoryShared)))

	for _, d := range cfg.Disks {
		args = append(args, "--disk", fmt.Sprintf("path=%s,readonly=%s", d.Path, onOff(d.ReadOnly)))
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
