package netshim

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requireRoot skips tests that need to create real network interfaces and
// iptables rules, which requires CAP_NET_ADMIN (in practice, root).
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_NET_ADMIN to manipulate network interfaces")
	}
	for _, bin := range []string{"ip", "iptables"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found on PATH", bin)
		}
	}
}

// TestSetup_Idempotent exercises Setup against the real network stack,
// using a throwaway bridge/subnet name derived from the test's own PID so
// concurrent runs (e.g. on a shared CI host) don't collide. It runs Setup
// twice to confirm the second run is a safe no-op, then tears everything
// down.
func TestSetup_Idempotent(t *testing.T) {
	requireRoot(t)

	bridge := fmt.Sprintf("nst-%d", os.Getpid()%10000)
	t.Cleanup(func() {
		// Delete every rule Setup could have added; -D only removes one
		// match at a time, so loop until none are left.
		for _, args := range [][]string{
			{"-t", "nat", "-D", "PREROUTING", "-i", "lo", "-d", "127.0.0.1", "-p", "tcp", "--dport", "30080", "-j", "DNAT", "--to-destination", "169.254.100.2:8080"},
			{"-t", "nat", "-D", "PREROUTING", "-i", "lo", "-d", "127.0.0.1", "-p", "udp", "--dport", "30080", "-j", "DNAT", "--to-destination", "169.254.100.2:8080"},
			{"-t", "nat", "-D", "OUTPUT", "-d", "127.0.0.1", "-p", "tcp", "--dport", "30080", "-j", "DNAT", "--to-destination", "169.254.100.2:8080"},
			{"-t", "nat", "-D", "OUTPUT", "-d", "127.0.0.1", "-p", "udp", "--dport", "30080", "-j", "DNAT", "--to-destination", "169.254.100.2:8080"},
			{"-t", "nat", "-D", "POSTROUTING", "-s", "169.254.100.0/24", "-o", "lo", "-j", "MASQUERADE"},
			{"-D", "FORWARD", "-i", bridge, "-j", "ACCEPT"},
			{"-D", "FORWARD", "-o", bridge, "-j", "ACCEPT"},
		} {
			for exec.Command("iptables", args...).Run() == nil {
			}
		}
		exec.Command("ip", "link", "del", bridge).Run()
		exec.Command("ip", "link", "del", tapPrefix+"vm1").Run()
	})

	cfg := Config{
		Bridge:        bridge,
		ExternalIface: "lo",
		GuestPort:     8080,
		VMs: []VM{
			{Name: "vm1", IP: net.ParseIP("169.254.100.2"), Port: 30080},
		},
	}
	_, subnet, err := net.ParseCIDR("169.254.100.1/24")
	if err != nil {
		t.Fatal(err)
	}
	cfg.BridgeAddr = net.ParseIP("169.254.100.1")
	cfg.BridgeNet = subnet

	if err := Setup(cfg); err != nil {
		t.Fatalf("Setup() first run error = %v", err)
	}
	if !linkExists(bridge) {
		t.Errorf("bridge %s was not created", bridge)
	}
	if !linkExists(tapPrefix + "vm1") {
		t.Errorf("tap %s was not created", tapPrefix+"vm1")
	}

	// Re-running must not error on "already exists" conditions.
	if err := Setup(cfg); err != nil {
		t.Fatalf("Setup() second run error = %v", err)
	}

	out, err := exec.Command("iptables", "-t", "nat", "-S", "PREROUTING").CombinedOutput()
	if err != nil {
		t.Fatalf("iptables -S PREROUTING: %v: %s", err, out)
	}
	rules := string(out)
	// One DNAT rule per protocol (tcp, udp) for port 30080, and no more:
	// each must appear exactly once even though Setup ran twice.
	for _, proto := range []string{"tcp", "udp"} {
		want := fmt.Sprintf("-p %s -m %s --dport 30080", proto, proto)
		if n := strings.Count(rules, want); n != 1 {
			t.Errorf("PREROUTING rules contain %d occurrences of %q, want exactly 1 (Setup should be idempotent):\n%s", n, want, rules)
		}
	}
}
