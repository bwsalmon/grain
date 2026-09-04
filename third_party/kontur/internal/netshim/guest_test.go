package netshim

import (
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
)

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
// guest booted with an empty gateway field and no route off its own
// segment.
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
	guest := GuestConfig(Config{VM: vmName, ExternalIface: extName}, id)
	if want := "ip=172.31.252.2::172.31.252.1:255.255.255.0::eth0:off"; guest.IPParam != want {
		t.Errorf("Guest.IPParam = %q, want %q", guest.IPParam, want)
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
