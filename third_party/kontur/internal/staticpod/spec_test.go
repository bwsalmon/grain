package staticpod

import (
	"strings"
	"testing"
)

func baseSpec() VMSpec {
	s := Defaults()
	s.Name = "web"
	s.DiskImage = "/images/disk.img"
	s.IP = "169.254.100.2"
	s.Port = 30080
	return s
}

func TestValidate_RequiresCore(t *testing.T) {
	cases := []struct {
		name string
		mut  func(s *VMSpec)
	}{
		{"name", func(s *VMSpec) { s.Name = "" }},
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

func TestValidate_Minimal(t *testing.T) {
	s := baseSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if s.Cmdline != "" {
		t.Errorf("Cmdline = %q, want empty (no kernel set)", s.Cmdline)
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
	if got != s {
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
