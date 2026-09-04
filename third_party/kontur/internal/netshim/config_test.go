package netshim

import (
	"net"
	"os"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envVM, envBridge, envControlCIDR, envExternalIface} {
		os.Unsetenv(k)
	}
}

func TestFromEnv_Defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv(envVM, "web")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}

	if cfg.VM != "web" {
		t.Errorf("VM = %q, want web", cfg.VM)
	}
	if got, want := cfg.TapName(), "tap-web"; got != want {
		t.Errorf("TapName() = %q, want %q", got, want)
	}
	if got, want := cfg.ControlTapName(), "ctl-web"; got != want {
		t.Errorf("ControlTapName() = %q, want %q", got, want)
	}
	if cfg.Bridge != defaultBridge {
		t.Errorf("Bridge = %q, want default %q", cfg.Bridge, defaultBridge)
	}
	if cfg.ExternalIface != defaultExternalIface {
		t.Errorf("ExternalIface = %q, want default %q", cfg.ExternalIface, defaultExternalIface)
	}
	if got, want := cfg.ControlAddr.String(), "169.254.100.1"; got != want {
		t.Errorf("ControlAddr = %q, want %q", got, want)
	}
	if cfg.ControlNet == nil {
		t.Errorf("ControlNet = nil, want the default control subnet")
	}
}

// TestFromEnv_ControlDisabled covers the explicit opt-out: an empty
// NETSHIM_CONTROL_CIDR means no control link at all, as opposed to an
// unset one meaning "use the default".
func TestFromEnv_ControlDisabled(t *testing.T) {
	clearEnv(t)
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

func TestFromEnv_Errors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"no VM name", map[string]string{}},
		// tapPrefix ("tap-") + 12 chars = 16, one over the 15-byte
		// interface name limit.
		{"name too long", map[string]string{envVM: "abcdefghijkl"}},
		{"bad control CIDR", map[string]string{envVM: "web", envControlCIDR: "nonsense"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := FromEnv(); err == nil {
				t.Errorf("FromEnv() error = nil, want an error")
			}
		})
	}
}

// TestEnabled covers the gate the VM container uses to tell a
// netshim-managed sandbox from a bare "kontur run", which has no tap to
// attach to and no identity to take over.
func TestEnabled(t *testing.T) {
	clearEnv(t)
	if Enabled() {
		t.Errorf("Enabled() = true with %s unset, want false", envVM)
	}
	t.Setenv(envVM, "web")
	if !Enabled() {
		t.Errorf("Enabled() = false with %s set, want true", envVM)
	}
}

func TestGuestConfig(t *testing.T) {
	clearEnv(t)
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

	g := GuestConfig(cfg, id)

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
	// The trailing field is dns0: FromEnv filled DNS in from DefaultDNS,
	// since NETSHIM_DNS is unset here. See TestFromEnv_DNS for the rest
	// of that.
	if wantIP := "ip=172.17.0.2::172.17.0.1:255.255.0.0::eth0:off:8.8.8.8"; g.IPParam != wantIP {
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

// The nameservers reach the guest on its own ip= parameter, which is what
// makes a deployment able to name the resolver its network actually has
// -- rather than every guest booting with whatever /etc/resolv.conf the
// machine that built the image happened to have, on an address nothing in
// the guest can route to.
func TestFromEnv_DNS(t *testing.T) {
	tests := []struct {
		name string
		// set is whether NETSHIM_DNS is in the environment at all, which
		// is a different thing from it being empty.
		set   bool
		value string
		want  string
	}{
		{name: "unset means the default", want: ":" + DefaultDNS},
		{name: "one", set: true, value: "10.0.0.53", want: ":10.0.0.53"},
		{name: "two", set: true, value: "10.0.0.53,10.0.1.53", want: ":10.0.0.53:10.0.1.53"},
		// Explicitly empty is the opt-out, the same shape
		// NETSHIM_CONTROL_CIDR has: the guest keeps whatever resolver its
		// image ships with, and the parameter stays the length it has
		// always been.
		{name: "explicitly empty", set: true, value: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envVM, "web")
			if tc.set {
				t.Setenv(envDNS, tc.value)
			}
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv() error = %v", err)
			}
			if got := DNSFields(cfg.DNS); got != tc.want {
				t.Errorf("DNSFields(cfg.DNS) = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseDNS_Rejects(t *testing.T) {
	// A third nameserver has nowhere to go (the ip= parameter has two
	// fields), and neither a name nor an IPv6 address is something the
	// parameter can carry -- all refused rather than quietly dropped,
	// since the guest is where the mistake would otherwise surface, as
	// lookups that hang.
	for _, spec := range []string{"nameserver", "8.8.8.8,1.1.1.1,9.9.9.9", "2001:4860:4860::8888"} {
		if _, err := ParseDNS(spec); err == nil {
			t.Errorf("ParseDNS(%q) = nil error, want one", spec)
		}
	}
}
