package staticpod

import (
	"strings"
	"testing"
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
	for _, key := range []string{"CHV_KERNEL", "CHV_INITRAMFS", "CHV_FIRMWARE", "CHV_CMDLINE"} {
		if strings.Contains(out, key) {
			t.Errorf("manifest unexpectedly contains %s when unset:\n%s", key, out)
		}
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

func TestRender_WritableDiskAddsWritableMount(t *testing.T) {
	s := baseSpec()
	s.DiskReadOnly = false
	s.DiskHostPath = "/var/lib/kontur/vm-disks"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	out, err := Render(s)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if got, want := envValue(t, out, "CHV_DISK_IMAGE"), "/disk/disk.qcow2"; got != want {
		t.Errorf("CHV_DISK_IMAGE = %q, want %q", got, want)
	}
	if !strings.Contains(out, "mountPath: /disk") {
		t.Errorf("manifest missing /disk volumeMount:\n%s", out)
	}
	if !strings.Contains(out, `path: "/var/lib/kontur/vm-disks/web"`) {
		t.Errorf("manifest missing writable disk hostPath:\n%s", out)
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
