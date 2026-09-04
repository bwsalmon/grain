package netshim

import (
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
)

// requireNetnsTestsEnv names the environment variable that turns
// requireRoot's skips into failures. Set it to "required" in any
// automated run that is *supposed* to exercise the kernel, so a
// misconfigured environment reports itself instead of quietly passing.
const requireNetnsTestsEnv = "KONTUR_NETNS_TESTS"

// requireRoot skips tests that need to create real network interfaces and
// tc filters, which requires CAP_NET_ADMIN (in practice, root) and access
// to /dev/net/tun -- the netlink library creates a tap by opening that
// device rather than over rtnetlink, so a root run without it fails deep
// inside link creation rather than here.
//
// It also requires the private network namespace TestMain re-execs into.
// That is what lets every test below name its interfaces plainly instead
// of deriving them from a pid, so running one in a shared namespace is
// not a degraded mode to fall back on: it would create "splice-tap" on
// whatever machine happens to be running the tests.
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
	case !inOwnNetns():
		reason = fmt.Sprintf("requires a network namespace of its own: %v", netnsSetupErr)
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

// TestSetup exercises the whole setup against the real network stack: a
// veth pair stands in for the interface a container runtime would have
// created and addressed, and Setup has to discover that identity, build a
// tap matching its MTU, splice the two, and stand up the control link
// beside them. Run twice, to confirm a retried init container converges
// rather than erroring or stacking duplicate filters.
func TestSetup(t *testing.T) {
	requireRoot(t)

	const (
		extName    = "setup-ext"
		peerName   = "setup-net"
		vmName     = "setup"
		bridgeName = "setup-br"

		mtu = 1450
	)

	t.Cleanup(func() {
		names := []string{extName, bridgeName, "tap-" + vmName, "ctl-" + vmName}
		for _, n := range names {
			if link, err := netlink.LinkByName(n); err == nil {
				netlink.LinkDel(link)
			}
		}
	})

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: extName, MTU: mtu},
		PeerName:  peerName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("creating veth pair: %v", err)
	}
	ext, err := netlink.LinkByName(extName)
	if err != nil {
		t.Fatalf("looking up %s: %v", extName, err)
	}
	addr, err := netlink.ParseAddr("172.31.253.2/24")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	if err := netlink.AddrAdd(ext, addr); err != nil {
		t.Fatalf("addressing %s: %v", extName, err)
	}
	if err := netlink.LinkSetUp(ext); err != nil {
		t.Fatalf("bringing up %s: %v", extName, err)
	}

	_, ctlNet, err := net.ParseCIDR("169.254.111.1/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	cfg := Config{
		VM:            vmName,
		Bridge:        bridgeName,
		ExternalIface: extName,
		ControlAddr:   net.IPv4(169, 254, 111, 1).To4(),
		ControlNet:    ctlNet,
	}

	for i := 0; i < 2; i++ {
		if err := Setup(cfg); err != nil {
			t.Fatalf("Setup (run %d) error = %v", i+1, err)
		}
	}

	// The identity the guest will take over has to be what the runtime
	// actually put on the interface.
	id, err := DiscoverIdentity(extName)
	if err != nil {
		t.Fatalf("DiscoverIdentity: %v", err)
	}
	if got := id.IP.String(); got != "172.31.253.2" {
		t.Errorf("Identity.IP = %q, want 172.31.253.2", got)
	}
	if id.MTU != mtu {
		t.Errorf("Identity.MTU = %d, want %d", id.MTU, mtu)
	}
	if got := id.Netmask(); got != "255.255.255.0" {
		t.Errorf("Identity.Netmask() = %q, want 255.255.255.0", got)
	}

	// The tap's MTU must match the segment's exactly: a splice is a
	// wire, with nothing in between to fragment an oversized frame.
	tap, err := netlink.LinkByName("tap-" + vmName)
	if err != nil {
		t.Fatalf("looking up tap: %v", err)
	}
	if tap.Attrs().MTU != mtu {
		t.Errorf("tap MTU = %d, want %d (the external interface's)", tap.Attrs().MTU, mtu)
	}

	for _, name := range []string{extName, "tap-" + vmName} {
		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("looking up %s: %v", name, err)
		}
		filters, err := netlink.FilterList(link, netlink.MakeHandle(0xffff, 0))
		if err != nil {
			t.Fatalf("listing filters on %s: %v", name, err)
		}
		if len(filters) != 1 {
			t.Errorf("%s has %d ingress filters, want exactly 1", name, len(filters))
		}
	}

	// The control link is a separate segment reachable from this
	// namespace, since the guest now answers to the namespace's own
	// address.
	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		t.Fatalf("looking up control bridge: %v", err)
	}
	addrs, err := netlink.AddrList(bridge, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("listing control bridge addresses: %v", err)
	}
	if len(addrs) != 1 || addrs[0].IP.String() != "169.254.111.1" {
		t.Errorf("control bridge addresses = %v, want [169.254.111.1]", addrs)
	}
	ctlTap, err := netlink.LinkByName("ctl-" + vmName)
	if err != nil {
		t.Fatalf("looking up control tap: %v", err)
	}
	if ctlTap.Attrs().MasterIndex != bridge.Attrs().Index {
		t.Errorf("control tap master = %d, want the control bridge (%d)",
			ctlTap.Attrs().MasterIndex, bridge.Attrs().Index)
	}
}

// TestSetup_NoControlLink covers the opt-out path: everything on the
// data path still gets built, and nothing of the control link does.
func TestSetup_NoControlLink(t *testing.T) {
	requireRoot(t)

	const (
		extName    = "noctl-ext"
		peerName   = "noctl-net"
		vmName     = "noctl"
		bridgeName = "noctl-br"
	)

	t.Cleanup(func() {
		for _, n := range []string{extName, "tap-" + vmName, "ctl-" + vmName} {
			if link, err := netlink.LinkByName(n); err == nil {
				netlink.LinkDel(link)
			}
		}
	})

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: extName},
		PeerName:  peerName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("creating veth pair: %v", err)
	}
	ext, err := netlink.LinkByName(extName)
	if err != nil {
		t.Fatalf("looking up %s: %v", extName, err)
	}
	addr, err := netlink.ParseAddr("172.31.254.2/24")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	if err := netlink.AddrAdd(ext, addr); err != nil {
		t.Fatalf("addressing %s: %v", extName, err)
	}
	if err := netlink.LinkSetUp(ext); err != nil {
		t.Fatalf("bringing up %s: %v", extName, err)
	}

	cfg := Config{
		VM:            vmName,
		Bridge:        bridgeName,
		ExternalIface: extName,
	}
	if err := Setup(cfg); err != nil {
		t.Fatalf("Setup() error = %v", err)
	}

	if _, err := netlink.LinkByName("tap-" + vmName); err != nil {
		t.Errorf("spliced tap missing: %v", err)
	}
	if _, err := netlink.LinkByName("ctl-" + vmName); err == nil {
		t.Errorf("control tap exists, want none when the control link is disabled")
	}
}
