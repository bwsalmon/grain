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

// TestDiscoverIdentity_Gateway is the half of the identity every other
// test here left uncovered, and the one a guest's whole egress path
// hangs off: the namespace's default route, which the guest can only
// learn through the ip= parameter derived from it.
//
// It reads the route back off the kernel rather than constructing a
// netlink.Route, because the bug it guards against is precisely the
// difference between the two: a dump fills in a destination of 0.0.0.0/0
// for a route the kernel sent no RTA_DST for, so the "Dst == nil" test
// this used to make matched nothing a container ever has, and every
// flat-mode guest booted with an empty gateway field and no route off
// its own segment.
func TestDiscoverIdentity_Gateway(t *testing.T) {
	requireRoot(t)

	suffix := os.Getpid() % 10000
	extName := fmt.Sprintf("ge-%d", suffix)
	peerName := fmt.Sprintf("gp-%d", suffix)
	vmName := fmt.Sprintf("g%d", suffix)

	t.Cleanup(func() {
		if link, err := netlink.LinkByName(extName); err == nil {
			netlink.LinkDel(link)
		}
	})

	// Both ends go up: a link with no carrier takes no route, and the
	// default route below is the entire point of this test.
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: extName},
		PeerName:  peerName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("creating veth pair: %v", err)
	}
	for _, n := range []string{extName, peerName} {
		link, err := netlink.LinkByName(n)
		if err != nil {
			t.Fatalf("looking up %s: %v", n, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatalf("bringing up %s: %v", n, err)
		}
	}
	ext, err := netlink.LinkByName(extName)
	if err != nil {
		t.Fatalf("looking up %s: %v", extName, err)
	}
	addr, err := netlink.ParseAddr("172.31.252.2/24")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	if err := netlink.AddrAdd(ext, addr); err != nil {
		t.Fatalf("addressing %s: %v", extName, err)
	}

	// The same shape a container runtime leaves behind: an on-link
	// gateway at the segment's first address, reached over this
	// interface.
	//
	// The metric is only there so this can run in a namespace that
	// already has a default route of its own -- which is every namespace
	// this test is ever run in outside a container, and where a second
	// one at the same metric is rejected outright. It does not weaken
	// what is being tested: DiscoverIdentity dumps the routes on this
	// interface alone, so the namespace's own default (on some other
	// link) is not in the list either way.
	gw := net.IPv4(172, 31, 252, 1).To4()
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: ext.Attrs().Index,
		Gw:        gw,
		Priority:  4242,
	}); err != nil {
		t.Fatalf("adding the default route: %v", err)
	}

	id, err := DiscoverIdentity(extName)
	if err != nil {
		t.Fatalf("DiscoverIdentity: %v", err)
	}
	if id.Gateway == nil {
		t.Fatalf("Identity.Gateway = nil, want %s -- the guest gets no default route without it", gw)
	}
	if got := id.Gateway.String(); got != gw.String() {
		t.Errorf("Identity.Gateway = %q, want %q", got, gw)
	}

	// And the whole reason the gateway is discovered at all: it has to
	// reach the guest, in the one place the guest can act on it.
	guest := FlatGuestConfig(Config{
		Mode:          ModeFlat,
		ExternalIface: extName,
		VMs:           []VM{{Name: vmName}},
	}, id)
	if want := "ip=172.31.252.2::172.31.252.1:255.255.255.0::eth0:off"; guest.IPParam != want {
		t.Errorf("FlatGuest.IPParam = %q, want %q", guest.IPParam, want)
	}
}

// TestDefaultGateway covers the same decision without a kernel, over
// both shapes a default route can arrive in -- the synthesized
// 0.0.0.0/0 a dump produces, and the nil destination a hand-built Route
// carries -- plus the routes that must not be mistaken for one.
func TestDefaultGateway(t *testing.T) {
	_, zeroNet, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	_, onLink, err := net.ParseCIDR("172.17.0.0/16")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}

	gw := net.IPv4(172, 17, 0, 1).To4()

	for _, tc := range []struct {
		name   string
		routes []netlink.Route
		want   string
	}{
		{
			name: "as the kernel reports it",
			routes: []netlink.Route{
				{Dst: onLink},
				{Dst: zeroNet, Gw: gw},
			},
			want: "172.17.0.1",
		},
		{
			name:   "with no destination at all",
			routes: []netlink.Route{{Gw: gw}},
			want:   "172.17.0.1",
		},
		{
			name:   "on-link routes only",
			routes: []netlink.Route{{Dst: onLink}},
			want:   "",
		},
		{
			name:   "a default route with no gateway",
			routes: []netlink.Route{{Dst: zeroNet}},
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ""
			if ip := defaultGateway(tc.routes); ip != nil {
				got = ip.String()
			}
			if got != tc.want {
				t.Errorf("defaultGateway() = %q, want %q", got, tc.want)
			}
		})
	}
}
