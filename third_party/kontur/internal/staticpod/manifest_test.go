package staticpod

import (
	"strings"
	"testing"

	"github.com/bwsalmon/kontur/internal/config"

	"github.com/bwsalmon/kontur/internal/netshim"
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
	if got, want := envValue(t, out, "CHV_NET"), "tap=tap-web"; got != want {
		t.Errorf("CHV_NET = %q, want %q", got, want)
	}
	if got, want := envValue(t, out, "NETSHIM_VMS"), "web:169.254.100.2:30080"; got != want {
		t.Errorf("NETSHIM_VMS = %q, want %q", got, want)
	}
	if got, want := envValue(t, out, "CHV_CMDLINE"), s.Cmdline; got != want {
		t.Errorf("CHV_CMDLINE = %q, want %q", got, want)
	}
	if got, want := envValue(t, out, "KONTUR_EXEC_ADDR"), "169.254.100.2:22"; got != want {
		t.Errorf("KONTUR_EXEC_ADDR = %q, want %q", got, want)
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
	// netshim-matching "ip=" boot parameter.
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

func TestRender_FlatMode(t *testing.T) {
	s := Defaults()
	s.Name = "web"
	s.DiskImage = "/images/disk.img"
	s.NetMode = netshim.ModeFlat
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if got, want := envValue(t, out, "NETSHIM_MODE"), netshim.ModeFlat; got != want {
		t.Errorf("NETSHIM_MODE = %q, want %q", got, want)
	}
	if got, want := envValue(t, out, "NETSHIM_VM"), "web"; got != want {
		t.Errorf("NETSHIM_VM = %q, want %q", got, want)
	}
	// NAT-only settings must not leak into a flat-mode manifest, where
	// they describe a subnet and forwarding rules that do not exist.
	for _, unwanted := range []string{"NETSHIM_VMS", "NETSHIM_GUEST_PORT", "NETSHIM_BRIDGE_CIDR", "CHV_NET"} {
		if strings.Contains(out, "- name: "+unwanted) {
			t.Errorf("manifest sets %s in flat mode:\n%s", unwanted, out)
		}
	}
	// exec goes via the control link, since the guest now answers to the
	// namespace's own address.
	if got, want := envValue(t, out, "KONTUR_EXEC_ADDR"), "169.254.100.2:22"; got != want {
		t.Errorf("KONTUR_EXEC_ADDR = %q, want %q", got, want)
	}
}

func TestRender_FlatModeWithoutControlLinkOmitsExecAddr(t *testing.T) {
	s := Defaults()
	s.Name = "web"
	s.DiskImage = "/images/disk.img"
	s.NetMode = netshim.ModeFlat
	s.ControlCIDR = ""
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if strings.Contains(out, "KONTUR_EXEC_ADDR") {
		t.Errorf("manifest sets KONTUR_EXEC_ADDR with no control link, which has no address to dial:\n%s", out)
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
