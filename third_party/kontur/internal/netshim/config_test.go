package netshim

import (
	"net"
	"os"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envBridge, envBridgeCIDR, envExternalIface, envGuestPort, envVMs} {
		os.Unsetenv(k)
	}
}

func TestFromEnv_Defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv(envVMs, "web:169.254.100.2:30080")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}

	if cfg.Bridge != defaultBridge {
		t.Errorf("Bridge = %q, want default %q", cfg.Bridge, defaultBridge)
	}
	if cfg.ExternalIface != defaultExternalIface {
		t.Errorf("ExternalIface = %q, want default %q", cfg.ExternalIface, defaultExternalIface)
	}
	if cfg.GuestPort != defaultGuestPort {
		t.Errorf("GuestPort = %d, want default %d", cfg.GuestPort, defaultGuestPort)
	}
	if !cfg.BridgeAddr.Equal(net.ParseIP("169.254.100.1")) {
		t.Errorf("BridgeAddr = %s, want 169.254.100.1", cfg.BridgeAddr)
	}
	if len(cfg.VMs) != 1 {
		t.Fatalf("VMs = %+v, want exactly one", cfg.VMs)
	}
	vm := cfg.VMs[0]
	if vm.Name != "web" || !vm.IP.Equal(net.ParseIP("169.254.100.2")) || vm.Port != 30080 {
		t.Errorf("VMs[0] = %+v, want {web 169.254.100.2 30080}", vm)
	}
	if vm.TapName() != "tap-web" {
		t.Errorf("TapName() = %q, want tap-web", vm.TapName())
	}
}

func TestFromEnv_MultipleVMs(t *testing.T) {
	clearEnv(t)
	t.Setenv(envVMs, "a:169.254.100.2:8080, b:169.254.100.3:8081")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if len(cfg.VMs) != 2 {
		t.Fatalf("VMs = %+v, want 2 entries", cfg.VMs)
	}
	if cfg.VMs[0].Name != "a" || cfg.VMs[1].Name != "b" {
		t.Errorf("VMs = %+v, want order [a b]", cfg.VMs)
	}
}

func TestFromEnv_MissingVMs(t *testing.T) {
	clearEnv(t)
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv() error = nil, want error for missing NETSHIM_VMS")
	}
}

func TestFromEnv_DuplicateName(t *testing.T) {
	clearEnv(t)
	t.Setenv(envVMs, "a:169.254.100.2:8080,a:169.254.100.3:8081")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv() error = nil, want error for duplicate VM name")
	}
}

func TestFromEnv_AddressOutsideSubnet(t *testing.T) {
	clearEnv(t)
	t.Setenv(envVMs, "a:10.0.0.2:8080")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv() error = nil, want error for address outside bridge subnet")
	}
}

func TestFromEnv_NameTooLongForTap(t *testing.T) {
	clearEnv(t)
	// tapPrefix ("tap-") + 12 chars = 16, one over the 15-byte interface
	// name limit.
	t.Setenv(envVMs, "abcdefghijkl:169.254.100.2:8080")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv() error = nil, want error for VM name producing an overlong tap name")
	}
}

func TestFromEnv_InvalidVMSpec(t *testing.T) {
	clearEnv(t)
	for _, spec := range []string{
		"missing-parts",
		"a:not-an-ip:8080",
		"a:169.254.100.2:not-a-port",
		"a:169.254.100.2:70000",
		":169.254.100.2:8080",
	} {
		t.Run(spec, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(envVMs, spec)
			if _, err := FromEnv(); err == nil {
				t.Fatalf("FromEnv() with %s: error = nil, want error", spec)
			}
		})
	}
}

func TestFromEnv_InvalidGuestPort(t *testing.T) {
	clearEnv(t)
	t.Setenv(envVMs, "a:169.254.100.2:8080")
	t.Setenv(envGuestPort, "0")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv() error = nil, want error for out-of-range guest port")
	}
}

func TestFromEnv_CustomBridgeCIDR(t *testing.T) {
	clearEnv(t)
	t.Setenv(envBridgeCIDR, "10.200.0.1/24")
	t.Setenv(envVMs, "a:10.200.0.5:8080")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if !cfg.BridgeAddr.Equal(net.ParseIP("10.200.0.1")) {
		t.Errorf("BridgeAddr = %s, want 10.200.0.1", cfg.BridgeAddr)
	}
}
