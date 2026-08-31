package staticpod

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bwsalmon/kontur/internal/netshim"
)

func baseSpec() VMSpec {
	s := Defaults()
	s.Name = "web"
	s.DiskImage = "/images/disk.img"
	s.IP = "169.254.100.2"
	s.Port = 30080
	return s
}

func TestValidate_WritableDiskMustBeUnderImagesMountPath(t *testing.T) {
	s := baseSpec()
	s.DiskReadOnly = false
	s.DiskImage = "/var/lib/kontur/guest/disk.img"
	if err := s.Validate(); err == nil {
		t.Fatalf("Validate() = nil, want an error for a writable disk outside %s", ImagesMountPath)
	}
}

func TestValidate_WritableDiskRequiresDiskHostPath(t *testing.T) {
	s := baseSpec()
	s.DiskReadOnly = false
	s.DiskHostPath = ""
	if err := s.Validate(); err == nil {
		t.Fatalf("Validate() = nil, want an error for an empty disk-hostpath with disk-readonly=false")
	}
}

func TestValidate_WritableDiskUnderImagesMountPathOK(t *testing.T) {
	s := baseSpec()
	s.DiskReadOnly = false
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for a writable disk under %s with a disk-hostpath set", err, ImagesMountPath)
	}
}

func TestResolvedDiskImage(t *testing.T) {
	s := baseSpec()
	if got, want := s.ResolvedDiskImage(), s.DiskImage; got != want {
		t.Errorf("ResolvedDiskImage() = %q, want %q (unchanged) when DiskReadOnly", got, want)
	}

	s.DiskReadOnly = false
	if got, want := s.ResolvedDiskImage(), DiskMountPath+"/disk.qcow2"; got != want {
		t.Errorf("ResolvedDiskImage() = %q, want %q when writable", got, want)
	}
}

func TestWritableDiskDir(t *testing.T) {
	s := baseSpec()
	s.DiskHostPath = "/var/lib/kontur/vm-disks"
	if got, want := s.WritableDiskDir(), "/var/lib/kontur/vm-disks/web"; got != want {
		t.Errorf("WritableDiskDir() = %q, want %q", got, want)
	}
}

func TestValidate_RequiresCore(t *testing.T) {
	cases := []struct {
		name string
		mut  func(s *VMSpec)
	}{
		{"name", func(s *VMSpec) { s.Name = "" }},
		{"name too long for tap device", func(s *VMSpec) { s.Name = "way-too-long-a-name" }},
		{"disk", func(s *VMSpec) { s.DiskImage = "" }},
		{"ip", func(s *VMSpec) { s.IP = "" }},
		{"bad ip", func(s *VMSpec) { s.IP = "not-an-ip" }},
		{"ipv6", func(s *VMSpec) { s.IP = "::1" }},
		{"port low", func(s *VMSpec) { s.Port = 0 }},
		{"port high", func(s *VMSpec) { s.Port = 70000 }},
		{"guest port", func(s *VMSpec) { s.GuestPort = 0 }},
		{"cpus", func(s *VMSpec) { s.CPUs = 0 }},
		{"memory", func(s *VMSpec) { s.MemoryMB = 1 }},
		{"kernel+firmware", func(s *VMSpec) { s.Kernel = "/images/vmlinux"; s.Firmware = "/images/CLOUDHV.fd" }},
		{"bad shutdown timeout", func(s *VMSpec) { s.ShutdownTimeout = "banana" }},
		{"bad bridge cidr", func(s *VMSpec) { s.BridgeCIDR = "not-a-cidr" }},
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
	want := "console=ttyS0 root=/dev/vda ro ip=169.254.100.2::169.254.100.1:255.255.255.0::eth0:off"
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
	s.BridgeCIDR = "169.254.100.1/24"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	want := "console=ttyS0 root=/dev/vda ro ip=169.254.100.2::169.254.100.1:255.255.255.0::eth0:off"
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

func TestValidate_AutoCmdlineUsesRWWhenNotReadOnly(t *testing.T) {
	s := baseSpec()
	s.Kernel = "/images/vmlinux"
	s.DiskReadOnly = false
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := s.Cmdline; !strings.Contains(got, "root=/dev/vda rw") {
		t.Errorf("Cmdline = %q, want it to contain %q", got, "root=/dev/vda rw")
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
	worker.IP = "169.254.100.3"
	worker.Port = 30081
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

// flatBaseSpec is baseSpec in flat mode: the container runtime assigns
// the address the guest takes over, so there is no -ip to give.
func flatBaseSpec() VMSpec {
	s := baseSpec()
	s.NetMode = netshim.ModeFlat
	s.IP = ""
	s.Port = 0
	return s
}

func TestValidate_FlatRejectsIP(t *testing.T) {
	s := flatBaseSpec()
	s.IP = "169.254.100.2"
	if err := s.Validate(); err == nil {
		t.Errorf("Validate() = nil, want an error: flat mode takes its address from the runtime, so -ip would be silently ignored")
	}
}

func TestValidate_FlatNeedsNoAddressOrPort(t *testing.T) {
	s := flatBaseSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	// The address is only knowable once the sandbox exists, so the
	// derived cmdline must leave ip= out for the VM container to append.
	if strings.Contains(s.Cmdline, "ip=") {
		t.Errorf("Cmdline = %q, want no ip= parameter in flat mode", s.Cmdline)
	}
	if !s.CmdlineAuto {
		t.Errorf("CmdlineAuto = false, want the derived cmdline to be marked automatic")
	}
}

func TestValidate_RejectsUnknownNetMode(t *testing.T) {
	s := baseSpec()
	s.NetMode = "bridged"
	if err := s.Validate(); err == nil {
		t.Errorf("Validate() = nil, want an error for an unknown net mode")
	}
}

func TestValidate_FlatRejectsBadControlCIDR(t *testing.T) {
	s := flatBaseSpec()
	s.ControlCIDR = "nonsense"
	if err := s.Validate(); err == nil {
		t.Errorf("Validate() = nil, want an error for an unparseable control CIDR")
	}
}

func TestValidate_FlatRejectsNameTooLongForControlTap(t *testing.T) {
	s := flatBaseSpec()
	s.Name = "twelvechars1"
	if err := s.Validate(); err == nil {
		t.Errorf("Validate() = nil, want an error: %q would exceed the 15-character interface name limit", "ctl-"+s.Name)
	}
}

func TestExecAddr(t *testing.T) {
	nat := baseSpec()
	if got, want := nat.ExecAddr(), "169.254.100.2:22"; got != want {
		t.Errorf("NAT ExecAddr() = %q, want %q", got, want)
	}

	// In flat mode the guest holds the namespace's own address, so
	// dialing it from inside the namespace reaches the local stack; the
	// control link is the only way back in.
	flat := flatBaseSpec()
	if got, want := flat.ExecAddr(), "169.254.100.2:22"; got != want {
		t.Errorf("flat ExecAddr() = %q, want the control link address %q", got, want)
	}

	none := flatBaseSpec()
	none.ControlCIDR = ""
	if got := none.ExecAddr(); got != "" {
		t.Errorf("ExecAddr() = %q, want empty with no control link: there is no path to the guest", got)
	}
}
