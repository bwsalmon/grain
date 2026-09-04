package staticpod

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bwsalmon/kontur/internal/config"
)

func baseSpec() VMSpec {
	s := Defaults()
	s.Name = "web"
	s.DiskImage = "/images/disk.img"
	return s
}

func TestValidate_WritableDiskNeedsNoHostPathOrImagesPrefix(t *testing.T) {
	// Both of these used to be errors, and stopped being ones when the
	// overlay moved inside the VM's container: there is no host-side
	// qcow2 to place, so no -disk-hostpath to require, and no backing
	// file to resolve out here, so no reason the source has to sit under
	// the shared /images mount.
	s := baseSpec()
	s.DiskReadOnly = false
	s.DiskHostPath = ""
	s.DiskImage = "/var/lib/kontur/guest/disk.img"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
	if s.DiskImage != "/var/lib/kontur/guest/disk.img" {
		t.Errorf("DiskImage = %q, want it passed through unchanged", s.DiskImage)
	}
}

func TestDiskModeOrDerived(t *testing.T) {
	// -disk-readonly=false has always meant "a private writable disk for
	// this VM", which the overlay mode is; mapping it to persistent
	// instead would silently point every existing caller's guest at the
	// shared image and let two VMs write to one file.
	cases := []struct {
		name string
		mut  func(s *VMSpec)
		want string
	}{
		{"explicit mode wins", func(s *VMSpec) { s.DiskMode = config.DiskModePersistent; s.DiskReadOnly = true }, config.DiskModePersistent},
		{"readonly true", func(s *VMSpec) { s.DiskReadOnly = true }, config.DiskModeReadOnly},
		{"readonly false", func(s *VMSpec) { s.DiskReadOnly = false }, config.DiskModeOverlay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSpec()
			s.DiskMode = ""
			tc.mut(&s)
			if got := s.DiskModeOrDerived(); got != tc.want {
				t.Errorf("DiskModeOrDerived() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidate_RejectsAnUnknownDiskMode(t *testing.T) {
	s := baseSpec()
	s.DiskMode = "writable"
	if err := s.Validate(); err == nil {
		t.Error("Validate() = nil, want an error naming the three valid modes")
	}
}

func TestValidate_RequiresCore(t *testing.T) {
	cases := []struct {
		name string
		mut  func(s *VMSpec)
	}{
		{"name", func(s *VMSpec) { s.Name = "" }},
		{"name too long for tap device", func(s *VMSpec) { s.Name = "way-too-long-a-name" }},
		{"cpus", func(s *VMSpec) { s.CPUs = 0 }},
		{"memory", func(s *VMSpec) { s.MemoryMB = 1 }},
		{"kernel+firmware", func(s *VMSpec) { s.Kernel = "/images/vmlinux"; s.Firmware = "/images/CLOUDHV.fd" }},
		{"bad shutdown timeout", func(s *VMSpec) { s.ShutdownTimeout = "banana" }},
		{"bad control cidr", func(s *VMSpec) { s.ControlCIDR = "not-a-cidr" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSpec()
			tc.mut(&s)
			if err := s.Validate(); err == nil {
				t.Errorf("Validate() = nil, want an error")
			}
		})
	}
}

func TestValidate_NoDiskMeansTheImagesOwnGuest(t *testing.T) {
	// An empty DiskImage is not a missing field: it means the guest
	// baked into the kontur image, which is what a VM booted from a
	// customized kontur image runs. There is no host path to name and no
	// shared backing file to overlay, so the writable-disk rules below
	// must not apply to it either.
	s := baseSpec()
	s.Backend = BackendDocker
	s.DiskImage = ""
	s.DiskReadOnly = false
	s.DiskHostPath = ""
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a spec with no disk of its own", err)
	}
	if s.DiskImage != "" {
		t.Errorf("DiskImage = %q, want it left empty so kontur run's own default applies", s.DiskImage)
	}
}

func TestValidate_NoDiskIsRejectedOnTheStaticPodBackend(t *testing.T) {
	s := baseSpec()
	s.Backend = BackendStaticPod
	s.DiskImage = ""
	if err := s.Validate(); err == nil {
		t.Error("Validate() = nil, want an error: the pod manifest always emits CHV_DISK_IMAGE, and an empty value is not an unset one")
	}
}

func TestValidate_TapNameLengthBoundary(t *testing.T) {
	// "tap-" (4 chars) + name must fit in 15 chars (IFNAMSIZ-1), so an
	// 11-character name is the longest that still fits.
	s := baseSpec()
	s.Name = "twelvecharzz"[:11]
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil for an 11-character name", err)
	}
	s.Name = "twelvecharzz"[:12]
	if err := s.Validate(); err == nil {
		t.Errorf("Validate() = nil, want an error for a 12-character name")
	}
}

func TestValidate_Minimal(t *testing.T) {
	s := baseSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	// Kernel is left unset, but so is Firmware: this still means direct
	// kernel boot, via kontur run's own baked-in CHV_KERNEL default (see
	// internal/config's defaultKernel) -- so Cmdline is still
	// auto-derived here, the same as if Kernel had been given explicitly.
	//
	// "rw", because the default disk mode is the overlay: the root a
	// stock guest boots is its own writable copy.
	want := "console=ttyS0 root=/dev/vda rw"
	if s.Cmdline != want {
		t.Errorf("Cmdline = %q, want %q", s.Cmdline, want)
	}
	if !s.CmdlineAuto {
		t.Errorf("CmdlineAuto = false, want true")
	}
}

func TestValidate_FirmwareSkipsAutoCmdline(t *testing.T) {
	s := baseSpec()
	s.Firmware = "/images/firmware.fd"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if s.Cmdline != "" {
		t.Errorf("Cmdline = %q, want empty (firmware boot, not direct kernel boot)", s.Cmdline)
	}
	if s.CmdlineAuto {
		t.Errorf("CmdlineAuto = true, want false for firmware boot")
	}
}

func TestValidate_AutoCmdline(t *testing.T) {
	s := baseSpec()
	s.Kernel = "/images/vmlinux"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	want := "console=ttyS0 root=/dev/vda rw"
	if s.Cmdline != want {
		t.Errorf("Cmdline = %q, want %q", s.Cmdline, want)
	}
	if !s.CmdlineAuto {
		t.Errorf("CmdlineAuto = false, want true")
	}
}

func TestValidate_ExplicitCmdlineNotOverridden(t *testing.T) {
	s := baseSpec()
	s.Kernel = "/images/vmlinux"
	s.Cmdline = "console=ttyS0 root=/dev/vda ro custom"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if s.Cmdline != "console=ttyS0 root=/dev/vda ro custom" {
		t.Errorf("Cmdline was overwritten: %q", s.Cmdline)
	}
	if s.CmdlineAuto {
		t.Errorf("CmdlineAuto = true, want false for an explicit cmdline")
	}
}

func TestValidate_BackendDefaultsToStaticPod(t *testing.T) {
	s := baseSpec()
	s.Backend = ""
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if s.Backend != BackendStaticPod {
		t.Errorf("Backend = %q, want %q for an unset backend", s.Backend, BackendStaticPod)
	}
}

func TestValidate_RejectsUnknownBackend(t *testing.T) {
	s := baseSpec()
	s.Backend = "qemu"
	if err := s.Validate(); err == nil {
		t.Fatalf("Validate() = nil, want an error for an unknown backend")
	}
}

func TestValidate_AutoCmdlineUsesROInReadOnlyMode(t *testing.T) {
	// The mirror of TestValidate_AutoCmdline, which gets "rw" from the
	// default overlay mode: read-only is now the mode that has to be
	// asked for, and asking for it is what still mounts the root "ro".
	s := baseSpec()
	s.Kernel = "/images/vmlinux"
	s.DiskMode = config.DiskModeReadOnly
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := s.Cmdline; !strings.Contains(got, "root=/dev/vda ro") {
		t.Errorf("Cmdline = %q, want it to contain %q", got, "root=/dev/vda ro")
	}
}

// TestDefaults_BootableDiskMode pins the default a VM gets when nothing
// is passed: the overlay, the only mode a stock guest finishes booting
// from. It defaulted to read-only until this was fixed, so every docker
// caller had to pass -disk-mode overlay to get a VM that came up at all.
func TestDefaults_BootableDiskMode(t *testing.T) {
	if got, want := Defaults().DiskModeOrDerived(), config.DiskModeOverlay; got != want {
		t.Errorf("Defaults() disk mode = %q, want %q", got, want)
	}
}

func TestSaveLoadDelete(t *testing.T) {
	dir := t.TempDir()
	s := baseSpec()

	if _, err := Load(dir, s.Name); err == nil {
		t.Fatalf("Load() before Save() = nil error, want not-found")
	}

	if err := Save(dir, s); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(dir, s.Name)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Errorf("Load() = %+v, want %+v", got, s)
	}

	if err := Delete(dir, s.Name); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := Load(dir, s.Name); err == nil {
		t.Fatalf("Load() after Delete() = nil error, want not-found")
	}
	// Deleting something already gone is not an error.
	if err := Delete(dir, s.Name); err != nil {
		t.Errorf("Delete() of already-deleted VM error = %v, want nil", err)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()

	specs, err := List(dir)
	if err != nil {
		t.Fatalf("List() on missing dir error = %v", err)
	}
	if len(specs) != 0 {
		t.Errorf("List() on missing dir = %v, want empty", specs)
	}

	web := baseSpec()
	worker := baseSpec()
	worker.Name = "worker"
	if err := Save(dir, web); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, worker); err != nil {
		t.Fatal(err)
	}

	specs, err = List(dir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(specs) != 2 || specs[0].Name != "web" || specs[1].Name != "worker" {
		t.Errorf("List() = %+v, want [web, worker] sorted by name", specs)
	}
}

// TestValidate_NeedsNoAddressOrPort covers what the spec deliberately no
// longer carries: the container runtime assigns the address the guest
// takes over, and ports are published on the sandbox itself.
func TestValidate_NeedsNoAddressOrPort(t *testing.T) {
	s := baseSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	// The address is only knowable once the sandbox exists, so the
	// derived cmdline must leave ip= out for the VM container to append.
	if strings.Contains(s.Cmdline, "ip=") {
		t.Errorf("Cmdline = %q, want no ip= parameter", s.Cmdline)
	}
	if !s.CmdlineAuto {
		t.Errorf("CmdlineAuto = false, want the derived cmdline to be marked automatic")
	}
}

func TestValidate_RejectsNameTooLongForControlTap(t *testing.T) {
	s := baseSpec()
	s.Name = "twelvechars1"
	if err := s.Validate(); err == nil {
		t.Errorf("Validate() = nil, want an error: %q would exceed the 15-character interface name limit", "ctl-"+s.Name)
	}
}

func TestValidate_DiskSizeNeedsOverlayMode(t *testing.T) {
	// The overlay is the only disk kontur creates for a VM. In the other
	// two modes the guest boots the image itself, which is shared with
	// every other VM using it and never resized -- so a disk size asked
	// for there is a mistake worth reporting at "vm create" time rather
	// than a container crash loop later.
	for _, mode := range []string{config.DiskModePersistent, config.DiskModeReadOnly} {
		t.Run(mode, func(t *testing.T) {
			s := baseSpec()
			s.DiskReadOnly = false
			s.DiskMode = mode
			s.DiskSizeMB = 8192
			err := s.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error for a disk size in %q mode", mode)
			}
			if !strings.Contains(err.Error(), config.DiskModeOverlay) {
				t.Errorf("Validate() error = %v, want it to name %q as the mode that can be sized", err, config.DiskModeOverlay)
			}
		})
	}

	s := baseSpec()
	s.DiskMode = config.DiskModeOverlay
	s.DiskSizeMB = 8192
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() error = %v, want a disk size accepted in %q mode", err, config.DiskModeOverlay)
	}

	s = baseSpec()
	s.DiskMode = config.DiskModeOverlay
	s.DiskSizeMB = -1
	if err := s.Validate(); err == nil {
		t.Error("Validate() = nil for a negative disk size, want an error")
	}
}

// The nameservers are validated on this side even though the ip=
// parameter carrying them is assembled inside the VM container. An
// address rejected there would fail a VM that "vm create" had already
// reported as started, which is a much worse place to learn about a typo.
func TestValidate_RejectsNameserversTheGuestCannotBeGiven(t *testing.T) {
	for _, dns := range []string{"nameserver", "8.8.8.8,1.1.1.1,9.9.9.9", "2001:4860:4860::8888"} {
		s := baseSpec()
		s.DNS = dns
		if err := s.Validate(); err == nil {
			t.Errorf("Validate() with dns %q = nil, want an error", dns)
		}
	}

	// Empty is not a mistake: it is how a caller keeps whatever resolver
	// the guest image ships with.
	s := baseSpec()
	s.DNS = ""
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() with no dns = %v, want it accepted", err)
	}
}
