package netshim

import (
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
)

// requireNetnsTestsEnv names the environment variable that turns
// requireRoot's skips into failures. Set it to "required" in any
// automated run that is *supposed* to exercise the kernel, so a
// misconfigured environment reports itself instead of quietly passing.
const requireNetnsTestsEnv = "KONTUR_NETNS_TESTS"

// requireRoot skips tests that need to create real network interfaces,
// nftables rules and tc filters, which requires CAP_NET_ADMIN (in
// practice, root) and access to /dev/net/tun -- the netlink library
// creates a tap by opening that device rather than over rtnetlink, so a
// root run without it fails deep inside link creation rather than here.
//
// Skipping is the right default: these tests cannot run on a developer's
// machine without sudo. It is also a trap, because `go test ./...`
// reports a package whose every kernel-touching test skipped as "ok",
// and skips are invisible without -v -- so a green run says nothing
// about whether the splice actually carries a packet. Setting
// KONTUR_NETNS_TESTS=required turns that silence into a failure.
func requireRoot(t *testing.T) {
	t.Helper()

	var reason string
	switch {
	case os.Geteuid() != 0:
		reason = fmt.Sprintf("requires root/CAP_NET_ADMIN to manipulate network interfaces (euid %d)", os.Geteuid())
	case !tunDeviceAvailable():
		reason = "requires /dev/net/tun, which the netlink library opens to create a tap"
	default:
		return
	}

	if os.Getenv(requireNetnsTestsEnv) == "required" {
		t.Fatalf("%s=required, but this test cannot run here: %s", requireNetnsTestsEnv, reason)
	}
	t.Skip(reason)
}

func tunDeviceAvailable() bool {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
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
		conn := &nftables.Conn{}
		if tables, err := conn.ListTables(); err == nil {
			for _, tbl := range tables {
				if tbl.Name == natTable {
					conn.DelTable(tbl)
				}
			}
			conn.Flush()
		}
		netlink.LinkDel(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: bridge}})
		netlink.LinkDel(&netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{Name: tapPrefix + "vm1"}})
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

	conn := &nftables.Conn{}
	tables, err := conn.ListTables()
	if err != nil {
		t.Fatalf("ListTables(): %v", err)
	}
	var found int
	for _, tbl := range tables {
		if tbl.Name == natTable && tbl.Family == nftables.TableFamilyIPv4 {
			found++
		}
	}
	// Setup rebuilds the table from scratch each run, so re-running must
	// not leave a duplicate table (or duplicate rules within it) behind.
	if found != 1 {
		t.Errorf("found %d %q nftables tables, want exactly 1", found, natTable)
	}

	chains, err := conn.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		t.Fatalf("ListChainsOfTableFamily(): %v", err)
	}
	var preroutingChain *nftables.Chain
	for _, c := range chains {
		if c.Table.Name == natTable && c.Name == "prerouting" {
			preroutingChain = c
		}
	}
	if preroutingChain == nil {
		t.Fatal("prerouting chain not found in kontur table")
	}
	rules, err := conn.GetRules(preroutingChain.Table, preroutingChain)
	if err != nil {
		t.Fatalf("GetRules(prerouting): %v", err)
	}
	// One DNAT rule per protocol (tcp, udp) for vm1, and no more, even
	// though Setup ran twice.
	if len(rules) != 2 {
		t.Errorf("prerouting chain has %d rules, want exactly 2 (Setup should be idempotent)", len(rules))
	}
}
