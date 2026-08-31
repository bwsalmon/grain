package netshim

import (
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestSetupFlat exercises the whole flat-mode setup against the real
// network stack: a veth pair stands in for the interface a container
// runtime would have created and addressed, and SetupFlat has to
// discover that identity, build a tap matching its MTU, splice the two,
// and stand up the control link beside them. Run twice, to confirm a
// retried init container converges rather than erroring or stacking
// duplicate filters.
func TestSetupFlat(t *testing.T) {
	requireRoot(t)

	suffix := os.Getpid() % 10000
	extName := fmt.Sprintf("fe-%d", suffix)
	peerName := fmt.Sprintf("fp-%d", suffix)
	vmName := fmt.Sprintf("f%d", suffix)
	bridgeName := fmt.Sprintf("fb-%d", suffix)

	const mtu = 1450

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
		Mode:          ModeFlat,
		Bridge:        bridgeName,
		ExternalIface: extName,
		ControlAddr:   net.IPv4(169, 254, 111, 1).To4(),
		ControlNet:    ctlNet,
		VMs:           []VM{{Name: vmName}},
	}

	for i := 0; i < 2; i++ {
		if err := SetupFlat(cfg); err != nil {
			t.Fatalf("SetupFlat (run %d) error = %v", i+1, err)
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

// TestSetupFlat_NoControlLink covers the opt-out path: everything on the
// data path still gets built, and nothing of the control link does.
func TestSetupFlat_NoControlLink(t *testing.T) {
	requireRoot(t)

	suffix := os.Getpid() % 10000
	extName := fmt.Sprintf("ne-%d", suffix)
	peerName := fmt.Sprintf("np-%d", suffix)
	vmName := fmt.Sprintf("n%d", suffix)

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
		Mode:          ModeFlat,
		Bridge:        fmt.Sprintf("nb-%d", suffix),
		ExternalIface: extName,
		VMs:           []VM{{Name: vmName}},
	}
	if err := SetupFlat(cfg); err != nil {
		t.Fatalf("SetupFlat() error = %v", err)
	}

	if _, err := netlink.LinkByName("tap-" + vmName); err != nil {
		t.Errorf("spliced tap missing: %v", err)
	}
	if _, err := netlink.LinkByName("ctl-" + vmName); err == nil {
		t.Errorf("control tap exists, want none when the control link is disabled")
	}
}

// TestDiscoverIdentity_NoAddress confirms a namespace whose external
// interface has not been addressed yet fails loudly, rather than handing
// the guest an empty identity to take over.
func TestDiscoverIdentity_NoAddress(t *testing.T) {
	requireRoot(t)

	name := fmt.Sprintf("na-%d", os.Getpid()%10000)
	t.Cleanup(func() {
		if link, err := netlink.LinkByName(name); err == nil {
			netlink.LinkDel(link)
		}
	})
	if _, err := ensureTapMTU(name, 1500); err != nil {
		t.Fatalf("ensureTapMTU: %v", err)
	}
	if _, err := DiscoverIdentity(name); err == nil {
		t.Errorf("DiscoverIdentity() error = nil, want an error for an unaddressed interface")
	}
}
