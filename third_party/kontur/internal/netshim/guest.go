package netshim

import (
	"bytes"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// routeParamKey is the kernel command-line parameter the guest's routes
// travel in. The kernel itself has nothing to do with it -- it takes no
// route parameter of any kind, which is the whole problem -- so this is
// namespaced like every other parameter a kernel passes through to
// userspace, and read there by kontur-configure-routes (see
// deploy/guest-image/).
const routeParamKey = "kontur.routes"

// Identity is the network identity the container runtime assigned to this
// network namespace: the address, MAC and MTU it put on the external
// interface. The guest takes this identity over wholesale, so that from
// outside the namespace there is still exactly one endpoint, with the
// address and MAC the runtime allocated -- see Setup.
type Identity struct {
	// Iface is the interface the identity was read from, i.e. the one
	// the runtime configured (Config.ExternalIface).
	Iface string

	MAC net.HardwareAddr
	IP  net.IP
	// Mask is IP's netmask, and Gateway the namespace's default route.
	// Gateway may be nil if the namespace has no default route, which is
	// unusual but not fatal: the guest simply gets no default route
	// either -- and so, since this is the only thing that ever gives it
	// one, no way off its own segment. A guest in that state boots and
	// answers perfectly well; see defaultGateway.
	Mask    net.IPMask
	Gateway net.IP
	MTU     int

	// Routes is the interface's whole IPv4 routing table, in the order
	// the guest has to reinstall it in (see carriedRoutes).
	//
	// Address, netmask and gateway describe a bridge CNI's world
	// exactly, and a point-to-point one's not at all: there the subnet
	// is deliberately *not* on-link, and the route saying so is the one
	// part of the identity a netmask cannot express. Carrying the table
	// itself is what makes the two cases the same case.
	Routes []Route
}

// Route is one entry of that table, in the only two shapes a container's
// own routes ever take: a destination reached directly over the
// interface, or one reached through a gateway.
type Route struct {
	// Dst is the destination prefix, masked to its network address. A
	// default route's is 0.0.0.0/0.
	Dst *net.IPNet

	// Gw is the gateway the destination is reached through, or nil for
	// a destination that is on-link.
	Gw net.IP
}

// String renders a route the way the guest is given it:
// "<destination>" for an on-link route, "<destination>@<gateway>" for a
// gatewayed one.
func (r Route) String() string {
	if r.Dst == nil {
		return ""
	}
	if r.Gw == nil {
		return r.Dst.String()
	}
	return r.Dst.String() + "@" + r.Gw.String()
}

// Netmask renders Mask in the dotted-decimal form the kernel's ip= boot
// parameter expects.
func (id Identity) Netmask() string {
	return net.IP(id.Mask).To4().String()
}

// DiscoverIdentity reads the runtime-assigned identity off iface. It only
// reads, so it needs no capabilities of its own -- which is what lets the
// VM container work out its own guest configuration from inside the
// shared namespace, rather than having it handed down as environment by
// whatever created the sandbox (see cmd/kontur's applyNetshimNet).
func DiscoverIdentity(iface string) (Identity, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return Identity{}, fmt.Errorf("looking up %s: %w", iface, err)
	}
	attrs := link.Attrs()

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return Identity{}, fmt.Errorf("reading addresses on %s: %w", iface, err)
	}
	if len(addrs) == 0 {
		return Identity{}, fmt.Errorf("no IPv4 address found on %s", iface)
	}

	// The first address is the interface's primary one, which is what a
	// container runtime assigns and therefore what the guest takes over.
	// A namespace whose interface carries several is not something any
	// of the supported runtimes produce.
	id := Identity{
		Iface: iface,
		MAC:   attrs.HardwareAddr,
		MTU:   attrs.MTU,
		IP:    addrs[0].IP.To4(),
		Mask:  addrs[0].Mask,
	}

	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return Identity{}, fmt.Errorf("reading routes on %s: %w", iface, err)
	}
	id.Gateway = defaultGateway(routes)
	id.Routes = carriedRoutes(routes)

	return id, nil
}

// carriedRoutes selects the routes worth handing the guest out of a dump
// of the external interface's table, and orders them so that they can be
// installed from first to last.
//
// On-link routes come first, because every gatewayed route depends on
// one: "10.244.0.0/24 via 10.244.0.1" can only be installed once
// 10.244.0.1 is itself reachable, and on a point-to-point CNI the only
// thing making it reachable is the "10.244.0.1/32 dev eth0" route in the
// same table. Installing them in the order the kernel happens to dump
// them would fail on that one route about half the time.
//
// Within each half the most specific prefix comes first, for the same
// reason one step further out (a gateway of one route reached through
// another) and for one more: a kernel dumps a table in an order of its
// own -- this one comes back with the default route ahead of the subnet
// route it is less specific than -- and a guest's routing table is not
// something to have depend on that.
//
// Anything that is not a plain unicast route is dropped: a blackhole,
// broadcast or local entry describes the namespace's own stack rather
// than a path off the interface, and means nothing to a guest that only
// took the interface over.
func carriedRoutes(routes []netlink.Route) []Route {
	var onLink, viaGateway []Route
	for _, r := range routes {
		// A dump fills Type in; a Route a caller built itself may not,
		// and zero is not RTN_UNICAST, so an unset Type is taken at
		// face value rather than as a reason to drop the route.
		if r.Type != 0 && r.Type != unix.RTN_UNICAST {
			continue
		}
		dst := normalizeDst(r.Dst)
		if dst == nil {
			continue
		}
		route := Route{Dst: dst, Gw: r.Gw.To4()}
		if route.Gw == nil {
			onLink = append(onLink, route)
			continue
		}
		viaGateway = append(viaGateway, route)
	}
	sortBySpecificity(onLink)
	sortBySpecificity(viaGateway)
	return append(onLink, viaGateway...)
}

// sortBySpecificity orders routes longest prefix first, and destinations
// of equal length by address, so that the same table always renders the
// same way.
func sortBySpecificity(routes []Route) {
	sort.SliceStable(routes, func(i, j int) bool {
		iOnes, _ := routes[i].Dst.Mask.Size()
		jOnes, _ := routes[j].Dst.Mask.Size()
		if iOnes != jOnes {
			return iOnes > jOnes
		}
		return bytes.Compare(routes[i].Dst.IP, routes[j].Dst.IP) < 0
	})
}

// normalizeDst turns a dumped route's destination into a masked IPv4
// prefix, or nil for a destination this cannot describe. A nil Dst is a
// default route: the kernel leaves RTA_DST off a route whose prefix
// length is zero (see defaultGateway), and while a dump has that filled
// back in, a hand-built Route does not.
func normalizeDst(dst *net.IPNet) *net.IPNet {
	if dst == nil {
		return &net.IPNet{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)}
	}
	ip := dst.IP.To4()
	ones, bits := dst.Mask.Size()
	if ip == nil || bits != 32 {
		return nil
	}
	mask := net.CIDRMask(ones, 32)
	return &net.IPNet{IP: ip.Mask(mask), Mask: mask}
}

// defaultGateway returns the gateway of the first default route in
// routes, or nil if none of them is one.
//
// It matches on the destination's prefix length rather than on a nil
// Dst, because a nil Dst is not what a route read back off the kernel
// has. The kernel leaves RTA_DST off a route message whose prefix length
// is zero, and the netlink library fills that absence back in the way
// iproute2 does -- deserializeRoute synthesizes 0.0.0.0/0 rather than
// leaving Dst nil -- so every route in a dump carries a non-nil Dst and
// a "Dst == nil" test can only ever match a Route a caller built itself.
//
// That is not a hypothetical: testing "Dst == nil" is what left every
// guest without a default route (the ip= parameter GuestConfig derives
// has an empty gateway field when Identity.Gateway is nil), and so with
// no egress off its own segment at all, while every other part of the
// identity it took over was correct.
func defaultGateway(routes []netlink.Route) net.IP {
	for _, r := range routes {
		if r.Gw == nil || !isDefaultDst(r.Dst) {
			continue
		}
		return r.Gw.To4()
	}
	return nil
}

// isDefaultDst reports whether dst is a default route's destination:
// either absent altogether, or the all-zero address with a zero-length
// prefix (0.0.0.0/0).
func isDefaultDst(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	ones, _ := dst.Mask.Size()
	return ones == 0 && dst.IP.IsUnspecified()
}

// Guest is the guest-side half of a netshim setup: what to hand
// cloud-hypervisor as --net, and the kernel ip= parameter that makes the
// guest configure the identity it has taken over. It is derived rather
// than configured, so the same values come out whether the sandbox was
// created by docker, by a kubelet, or by hand.
type Guest struct {
	// Nets holds one --net value per guest NIC: the spliced interface
	// first, then the control link if there is one.
	Nets []string

	// IPParam is the kernel ip= parameter configuring the first NIC.
	// The control NIC is not covered by it -- the kernel's own ip=
	// handling configures a single interface -- so a guest that wants
	// the control link has to bring its own second address up itself.
	IPParam string

	// RouteParam is the kernel parameter carrying the routes the
	// runtime installed on the external interface, which the guest
	// reinstalls at boot over the table ip= alone leaves it with.
	// Empty when the interface had no routes to carry.
	//
	// It is a parameter of kontur's own rather than more of ip=,
	// because ip= cannot express this: it has one netmask and no route
	// list, so a guest configured from it alone treats its whole subnet
	// as on-link -- true on a bridge CNI, and precisely what a
	// point-to-point CNI arranges for it not to be.
	RouteParam string

	// ControlIP is the address the guest is expected to configure on the
	// control NIC, and therefore where this namespace reaches it (for
	// "kontur exec" and the memory agent). Empty when there is no
	// control link.
	ControlIP string
}

// GuestConfig derives the guest configuration for a VM from the identity
// discovered on the external interface.
func GuestConfig(cfg Config, id Identity) Guest {
	// mac= is what makes the takeover invisible from outside: the guest
	// presents the very MAC the runtime put on the veth, so the segment
	// sees the same single endpoint it always did rather than a second
	// one appearing behind an authorized port.
	g := Guest{
		Nets: []string{fmt.Sprintf("tap=%s,mac=%s,mtu=%d", cfg.TapName(), id.MAC, id.MTU)},
	}

	gw := ""
	if id.Gateway != nil {
		gw = id.Gateway.String()
	}
	g.IPParam = fmt.Sprintf("ip=%s::%s:%s::eth0:off%s", id.IP, gw, id.Netmask(), DNSFields(cfg.DNS))
	g.RouteParam = routeParam(id.Routes)

	if cfg.ControlNet != nil {
		g.Nets = append(g.Nets, fmt.Sprintf("tap=%s", cfg.ControlTapName()))
		g.ControlIP = ControlGuestIP(cfg.ControlAddr).String()
	}

	return g
}

// ControlGuestIP returns the address the guest is expected to hold on the
// control link, one past the address this namespace holds on it (so the
// default 169.254.100.1/24 control bridge puts the guest at
// 169.254.100.2).
func ControlGuestIP(bridgeAddr net.IP) net.IP {
	next := make(net.IP, len(bridgeAddr.To4()))
	copy(next, bridgeAddr.To4())
	next[3]++
	return next
}

func maskBits(m net.IPMask) int {
	ones, _ := m.Size()
	return ones
}

// routeParam renders routes as the kernel parameter the guest reads
// them back out of: comma-separated entries, each "<destination>" or
// "<destination>@<gateway>", in the order they have to be installed in.
// Empty for an interface with no routes, which is a guest with nothing
// to reinstall rather than one to hand an empty list to.
//
// The preferred source address is deliberately not carried, though a
// point-to-point CNI sets one on the routes it installs: the guest's
// interface holds exactly one address -- the one it took over -- so the
// kernel picks that same address for these routes anyway.
func routeParam(routes []Route) string {
	if len(routes) == 0 {
		return ""
	}
	specs := make([]string, 0, len(routes))
	for _, r := range routes {
		if s := r.String(); s != "" {
			specs = append(specs, s)
		}
	}
	if len(specs) == 0 {
		return ""
	}
	return routeParamKey + "=" + strings.Join(specs, ",")
}

// WithIPParam appends ipParam to a kernel command line, unless it already
// configures an interface itself. An explicit ip= in CHV_CMDLINE is an
// operator overriding the derived identity on purpose, so it wins.
func WithIPParam(cmdline, ipParam string) string {
	if ipParam == "" {
		return cmdline
	}
	if hasParam(cmdline, "ip=") {
		return cmdline
	}
	return appendParam(cmdline, ipParam)
}

// WithGuestParams appends everything GuestConfig derived for the guest's
// first NIC to a kernel command line: the ip= that addresses it, and the
// kontur.routes= that gives it the table the runtime really installed.
//
// An explicit ip= in CHV_CMDLINE takes both away, not just the first.
// The two describe one interface between them, so handing a
// hand-addressed guest the namespace's routes would be applying half of
// a configuration nobody asked for -- and the half likelier to be wrong,
// since an operator overriding the identity is by definition not the
// identity these routes were read off.
func WithGuestParams(cmdline string, g Guest) string {
	withIP := WithIPParam(cmdline, g.IPParam)
	if g.IPParam != "" && withIP == cmdline {
		return cmdline
	}
	if g.RouteParam == "" || hasParam(withIP, routeParamKey+"=") {
		return withIP
	}
	return appendParam(withIP, g.RouteParam)
}

func hasParam(cmdline, prefix string) bool {
	for _, field := range strings.Fields(cmdline) {
		if strings.HasPrefix(field, prefix) {
			return true
		}
	}
	return false
}

func appendParam(cmdline, param string) string {
	if cmdline == "" {
		return param
	}
	return cmdline + " " + param
}

// DNSFields renders nameservers as the trailing dns0/dns1 fields of an
// ip= boot parameter -- ":8.8.8.8" for one, ":8.8.8.8:1.1.1.1" for two --
// or the empty string for none, which leaves the parameter exactly as
// short as it always was.
//
// The guest is what acts on them: nothing in the kernel's own ip=
// handling writes /etc/resolv.conf, so the guest's kontur-configure-dns
// reads them back off /proc/cmdline and writes the file itself (see
// deploy/guest-image/overlay-common). Passing them here rather than
// baking a resolver into the guest image is what makes the resolver a
// per-boot setting: one image, and each deployment names the nameserver
// its own network actually has.
func DNSFields(dns []net.IP) string {
	var b strings.Builder
	for _, ip := range dns {
		b.WriteString(":")
		b.WriteString(ip.String())
	}
	return b.String()
}
