package staticpod

import (
	"strings"
	"testing"

	"github.com/bwsalmon/kontur/internal/config"
)

// envValue returns the "value:" line following a "- name: <key>" entry in
// a rendered manifest, failing the test if key isn't present.
func envValue(t *testing.T, manifest, key string) string {
	t.Helper()
	marker := "- name: " + key
	idx := strings.Index(manifest, marker)
	if idx < 0 {
		t.Fatalf("env var %s not found in manifest:\n%s", key, manifest)
	}
	rest := manifest[idx:]
	lines := strings.SplitN(rest, "\n", 3)
	if len(lines) < 2 {
		t.Fatalf("env var %s has no value line", key)
	}
	valueLine := strings.TrimSpace(lines[1])
	valueLine = strings.TrimPrefix(valueLine, "value: ")
	return strings.Trim(valueLine, `"`)
}

func TestRender_Basic(t *testing.T) {
	s := baseSpec()
	s.Kernel = "/images/vmlinux"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if !strings.Contains(out, `name: "kontur-vm-web"`) {
		t.Errorf("manifest missing pod name, got:\n%s", out)
	}
	if got, want := envValue(t, out, "CHV_DISK_IMAGE"), s.DiskImage; got != want {
		t.Errorf("CHV_DISK_IMAGE = %q, want %q", got, want)
	}
	// No CHV_NET: the VM container derives its own --net from the
	// identity on the namespace's interface, from the same NETSHIM_*
	// settings the init container reads.
	if strings.Contains(out, "- name: CHV_NET") {
		t.Errorf("manifest sets CHV_NET, which the VM container derives for itself:\n%s", out)
	}
	if got, want := envValue(t, out, "NETSHIM_VM"), "web"; got != want {
		t.Errorf("NETSHIM_VM = %q, want %q", got, want)
	}
	if got, want := envValue(t, out, "CHV_CMDLINE"), s.Cmdline; got != want {
		t.Errorf("CHV_CMDLINE = %q, want %q", got, want)
	}
	// The manifest no longer carries an address for exec to dial: it
	// reaches the guest over the VM's vsock device instead, at a path
	// both halves of the kontur binary default to.
	if strings.Contains(out, "KONTUR_EXEC_ADDR") {
		t.Errorf("manifest still sets KONTUR_EXEC_ADDR:\n%s", out)
	}
}

func TestRender_OmitsUnsetOptionalFields(t *testing.T) {
	s := baseSpec() // no Kernel, Initramfs, Firmware
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	// CHV_KERNEL/CHV_INITRAMFS/CHV_FIRMWARE stay omitted: kontur run's own
	// baked-in CHV_KERNEL default takes over. CHV_CMDLINE is still
	// rendered, though, since Validate auto-derives it whenever Firmware
	// is unset (see TestValidate_Minimal) -- direct kernel boot still
	// applies even with no explicit Kernel, so the guest still needs its
	// console and root device named.
	for _, key := range []string{"CHV_KERNEL", "CHV_INITRAMFS", "CHV_FIRMWARE"} {
		if strings.Contains(out, key) {
			t.Errorf("manifest unexpectedly contains %s when unset:\n%s", key, out)
		}
	}
	if !strings.Contains(out, "CHV_CMDLINE") {
		t.Errorf("manifest missing auto-derived CHV_CMDLINE:\n%s", out)
	}
}

func TestRender_QuotesSpecialCharacters(t *testing.T) {
	s := baseSpec()
	s.KonturImage = `weird"image\ref:latest`
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if got, want := envValue(t, out, "CHV_DISK_IMAGE"), s.DiskImage; got != want {
		t.Errorf("CHV_DISK_IMAGE = %q, want %q", got, want)
	}
	if !strings.Contains(out, `image: "weird\"image\\ref:latest"`) {
		t.Errorf("image field not safely quoted, got:\n%s", out)
	}
}

func TestRender_ReadOnlyDiskHasNoWritableMount(t *testing.T) {
	s := baseSpec()
	s.DiskMode = config.DiskModeReadOnly
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if strings.Contains(out, "mountPath: /disk") || strings.Contains(out, "name: disk") {
		t.Errorf("manifest for a read-only disk unexpectedly has a /disk mount:\n%s", out)
	}
	if got, want := envValue(t, out, "CHV_DISK_IMAGE"), "/images/disk.img"; got != want {
		t.Errorf("CHV_DISK_IMAGE = %q, want %q", got, want)
	}
}

func TestRender_WritableDiskNeedsNoHostMount(t *testing.T) {
	// A writable disk used to mean a second hostPath: konturctl created a
	// qcow2 out here and mounted the directory holding it read-write.
	// The overlay is created inside the VM's own container now, so the
	// manifest names the source image directly and mounts only the shared,
	// read-only images directory.
	s := baseSpec()
	s.DiskReadOnly = false
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if got, want := envValue(t, out, "CHV_DISK_IMAGE"), s.DiskImage; got != want {
		t.Errorf("CHV_DISK_IMAGE = %q, want the source image %q", got, want)
	}
	if got, want := envValue(t, out, "CHV_DISK_MODE"), config.DiskModeOverlay; got != want {
		t.Errorf("CHV_DISK_MODE = %q, want %q", got, want)
	}
	if strings.Contains(out, "mountPath: /disk") {
		t.Errorf("manifest still has a /disk volumeMount:\n%s", out)
	}
	if strings.Contains(out, "vm-disks") {
		t.Errorf("manifest still has a host writable-disk path:\n%s", out)
	}
}

func TestManifestFileNameAndPodName(t *testing.T) {
	if got, want := ManifestFileName("web"), "kontur-vm-web.yaml"; got != want {
		t.Errorf("ManifestFileName() = %q, want %q", got, want)
	}
	if got, want := PodName("web"), "kontur-vm-web"; got != want {
		t.Errorf("PodName() = %q, want %q", got, want)
	}
}

// TestRender_ReadinessProbeReportsOnTheGuest is what makes "kubectl wait
// --for=condition=Ready" on a kontur pod mean anything: without the
// probe the condition reports on a container that is up as soon as
// cloud-hypervisor is exec'd, minutes before the guest inside it can be
// reached, and every caller writes its own poll loop instead.
func TestRender_ReadinessProbeReportsOnTheGuest(t *testing.T) {
	s := baseSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if !strings.Contains(out, `command: ["/usr/local/bin/kontur", "ready", "-timeout", "0"]`) {
		t.Errorf("manifest has no \"kontur ready\" readiness probe:\n%s", out)
	}
	// The VM container only: netshim is an init container that has
	// already exited by the time anything could be probed.
	if got := strings.Count(out, "readinessProbe:"); got != 1 {
		t.Errorf("readinessProbe appears %d times, want once (on the VM container):\n%s", got, out)
	}
}

// TestRender_NetshimEnvOnBothContainers covers the settings that have to
// reach the VM container as well as the init container: it derives its
// own --net from them, so a manifest that gave them to only one would
// have the two disagree about the tap.
func TestRender_NetshimEnvOnBothContainers(t *testing.T) {
	s := baseSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	for _, key := range []string{"NETSHIM_VM", "NETSHIM_BRIDGE", "NETSHIM_CONTROL_CIDR", "NETSHIM_EXTERNAL_IFACE"} {
		if got := strings.Count(out, "- name: "+key); got != 2 {
			t.Errorf("%s appears %d times, want once per container:\n%s", key, got, out)
		}
	}
	// exec goes via the control link, since the guest answers to the
	// namespace's own address.
	// The manifest no longer carries an address for exec to dial: it
	// reaches the guest over the VM's vsock device instead, at a path
	// both halves of the kontur binary default to.
	if strings.Contains(out, "KONTUR_EXEC_ADDR") {
		t.Errorf("manifest still sets KONTUR_EXEC_ADDR:\n%s", out)
	}
}

// GuestUser has to reach the VM container, because both halves of it are
// read there: "kontur run" authorizes the guest account, "kontur exec"
// logs in as it. See VMSpec.GuestUser.
func TestRender_GuestUser(t *testing.T) {
	s := baseSpec()
	s.GuestUser = "debian"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if got, want := envValue(t, out, "KONTUR_EXEC_USER"), "debian"; got != want {
		t.Errorf("KONTUR_EXEC_USER = %q, want %q", got, want)
	}
}

// Unset means root, which the guest authorizes unconditionally -- so the
// variable is left out rather than sent as an empty string, which
// guestexec would read as an account named "".
func TestRender_NoGuestUser(t *testing.T) {
	s := baseSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if strings.Contains(out, "KONTUR_EXEC_USER") {
		t.Errorf("manifest sets KONTUR_EXEC_USER with no guest user configured:\n%s", out)
	}
}

func TestRender_DiskSizeMB(t *testing.T) {
	s := baseSpec()
	s.DiskReadOnly = false
	s.DiskSizeMB = 8192
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if got, want := envValue(t, out, "CHV_DISK_SIZE_MB"), "8192"; got != want {
		t.Errorf("CHV_DISK_SIZE_MB = %q, want %q", got, want)
	}
}

func TestRender_OmitsDiskSizeWhenUnset(t *testing.T) {
	// Unset means "whatever the disk image itself is", which the VM
	// container already does when it sees no CHV_DISK_SIZE_MB -- rendering
	// a "0" instead would have to be special-cased in there.
	s := baseSpec()
	s.DiskReadOnly = false
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if strings.Contains(out, "CHV_DISK_SIZE_MB") {
		t.Errorf("manifest unexpectedly contains CHV_DISK_SIZE_MB when unset:\n%s", out)
	}
}

// The two hotplug ceilings a VM can only be given at boot: without them
// "kontur resize" has nothing to grow into, which is the whole reason
// they are settable from konturctl at all.
func TestRender_HotplugCeilings(t *testing.T) {
	s := baseSpec()
	s.CPUsMax = 8
	s.MemoryMaxMB = 4096
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if got, want := envValue(t, out, "CHV_CPUS_MAX"), "8"; got != want {
		t.Errorf("CHV_CPUS_MAX = %q, want %q", got, want)
	}
	if got, want := envValue(t, out, "CHV_MEMORY_MAX_MB"), "4096"; got != want {
		t.Errorf("CHV_MEMORY_MAX_MB = %q, want %q", got, want)
	}
}

func TestRender_OmitsHotplugCeilingsWhenUnset(t *testing.T) {
	// Rendering the boot values as their own ceilings would be the same
	// as leaving them out -- and would make a spec saved before these
	// existed look like one that had asked for no headroom on purpose.
	s := baseSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	for _, name := range []string{"CHV_CPUS_MAX", "CHV_MEMORY_MAX_MB"} {
		if strings.Contains(out, name) {
			t.Errorf("manifest unexpectedly contains %s when unset:\n%s", name, out)
		}
	}
}

func TestValidate_RejectsCeilingBelowBootSize(t *testing.T) {
	s := baseSpec()
	s.CPUsMax = s.CPUs - 1
	if err := s.Validate(); err == nil {
		t.Errorf("Validate() with cpus-max below cpus = nil, want error")
	}
	s = baseSpec()
	s.MemoryMaxMB = s.MemoryMB - 1
	if err := s.Validate(); err == nil {
		t.Errorf("Validate() with memory-max-mb below memory-mb = nil, want error")
	}
}
