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

func TestFromEnv_Flat(t *testing.T) {
	t.Setenv(envMode, ModeFlat)
	t.Setenv(envVM, "web")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.Mode != ModeFlat {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeFlat)
	}
	if len(cfg.VMs) != 1 || cfg.VMs[0].Name != "web" {
		t.Fatalf("VMs = %+v, want a single VM named web", cfg.VMs)
	}
	if got, want := cfg.VMs[0].TapName(), "tap-web"; got != want {
		t.Errorf("TapName() = %q, want %q", got, want)
	}
	if got, want := cfg.VMs[0].ControlTapName(), "ctl-web"; got != want {
		t.Errorf("ControlTapName() = %q, want %q", got, want)
	}
	if got, want := cfg.ExternalIface, defaultExternalIface; got != want {
		t.Errorf("ExternalIface = %q, want %q", got, want)
	}
	if got, want := cfg.ControlAddr.String(), "169.254.100.1"; got != want {
		t.Errorf("ControlAddr = %q, want %q", got, want)
	}
	if cfg.ControlNet == nil {
		t.Errorf("ControlNet = nil, want the default control subnet")
	}
}

// TestFromEnv_FlatControlDisabled covers the explicit opt-out: an empty
// NETSHIM_CONTROL_CIDR means no control link at all, as opposed to an
// unset one meaning "use the default".
func TestFromEnv_FlatControlDisabled(t *testing.T) {
	t.Setenv(envMode, ModeFlat)
	t.Setenv(envVM, "web")
	t.Setenv(envControlCIDR, "")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.ControlNet != nil || cfg.ControlAddr != nil {
		t.Errorf("control link = %v/%v, want none", cfg.ControlAddr, cfg.ControlNet)
	}
}

func TestFromEnv_FlatErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"no VM name", map[string]string{envMode: ModeFlat}},
		{"name too long", map[string]string{envMode: ModeFlat, envVM: "averylongvmname"}},
		{"bad control CIDR", map[string]string{envMode: ModeFlat, envVM: "web", envControlCIDR: "nonsense"}},
		{"unknown mode", map[string]string{envMode: "bridged", envVM: "web"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := FromEnv(); err == nil {
				t.Errorf("FromEnv() error = nil, want an error")
			}
		})
	}
}

func TestFlatGuestConfig(t *testing.T) {
	t.Setenv(envMode, ModeFlat)
	t.Setenv(envVM, "web")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}

	mac, err := net.ParseMAC("02:42:ac:11:00:02")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	id := Identity{
		Iface:   "eth0",
		MAC:     mac,
		IP:      net.IPv4(172, 17, 0, 2).To4(),
		Mask:    net.CIDRMask(16, 32),
		Gateway: net.IPv4(172, 17, 0, 1).To4(),
		MTU:     1450,
	}

	g := FlatGuestConfig(cfg, id)

	// The guest must present the MAC the runtime assigned, or the
	// segment sees a second endpoint appear behind an authorized port.
	want := "tap=tap-web,mac=02:42:ac:11:00:02,mtu=1450"
	if len(g.Nets) != 2 {
		t.Fatalf("Nets = %q, want the spliced NIC plus the control NIC", g.Nets)
	}
	if g.Nets[0] != want {
		t.Errorf("Nets[0] = %q, want %q", g.Nets[0], want)
	}
	if g.Nets[1] != "tap=ctl-web" {
		t.Errorf("Nets[1] = %q, want tap=ctl-web", g.Nets[1])
	}
	if wantIP := "ip=172.17.0.2::172.17.0.1:255.255.0.0::eth0:off"; g.IPParam != wantIP {
		t.Errorf("IPParam = %q, want %q", g.IPParam, wantIP)
	}
	if g.ControlIP != "169.254.100.2" {
		t.Errorf("ControlIP = %q, want 169.254.100.2", g.ControlIP)
	}
}

func TestWithIPParam(t *testing.T) {
	const derived = "ip=172.17.0.2::172.17.0.1:255.255.0.0::eth0:off"
	tests := []struct {
		name    string
		cmdline string
		want    string
	}{
		{"appended", "console=ttyS0 root=/dev/vda ro", "console=ttyS0 root=/dev/vda ro " + derived},
		{"empty cmdline", "", derived},
		{
			// An explicit ip= is an operator overriding the derived
			// identity on purpose, so it must survive untouched.
			"operator override wins",
			"console=ttyS0 ip=10.0.0.5::10.0.0.1:255.255.255.0::eth0:off",
			"console=ttyS0 ip=10.0.0.5::10.0.0.1:255.255.255.0::eth0:off",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithIPParam(tc.cmdline, derived); got != tc.want {
				t.Errorf("WithIPParam() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestControlGuestIP(t *testing.T) {
	if got := ControlGuestIP(net.IPv4(169, 254, 100, 1)).String(); got != "169.254.100.2" {
		t.Errorf("ControlGuestIP() = %q, want 169.254.100.2", got)
	}
}
