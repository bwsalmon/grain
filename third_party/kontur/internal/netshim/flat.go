package netshim

import (
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/vishvananda/netlink"
)

// Identity is the network identity the container runtime assigned to this
// network namespace: the address, MAC and MTU it put on the external
// interface. In flat mode the guest takes this identity over wholesale,
// so that from outside the namespace there is still exactly one endpoint,
// with the address and MAC the runtime allocated -- see SetupFlat.
type Identity struct {
	// Iface is the interface the identity was read from, i.e. the one
	// the runtime configured (Config.ExternalIface).
	Iface string

	MAC net.HardwareAddr
	IP  net.IP
	// Mask is IP's netmask, and Gateway the namespace's default route.
	// Gateway may be nil if the namespace has no default route, which is
	// unusual but not fatal: the guest simply gets no default route
	// either.
	Mask    net.IPMask
	Gateway net.IP
	MTU     int
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
// whatever created the sandbox (see cmd/kontur's flat-mode handling).
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
	for _, r := range routes {
		if r.Dst == nil && r.Gw != nil {
			id.Gateway = r.Gw.To4()
			break
		}
	}

	return id, nil
}

// SetupFlat wires up flat mode: the guest is spliced directly onto the
// container's own network segment and inherits its identity, instead of
// living behind the private subnet and NAT rules Setup builds.
//
// Nothing here rewrites, routes or filters a packet. Once the splice is
// programmed the kernel moves every frame between the external interface
// and the tap on its own, so this runs once to completion (as an init
// container, or as one "docker run" before the VM container's) and exits,
// leaving state that lives as long as the namespace does.
//
// Two things it deliberately does not do, both of which the NAT path
// needs and this one does not: it never writes net.ipv4.ip_forward (there
// is no routing), and it installs no nftables rules (there is no NAT). A
// flat-mode netshim therefore needs only CAP_NET_ADMIN and access to
// /dev/net/tun, not the full privileged container the NAT path requires
// for its masked-off /proc/sys/net write.
//
// The external interface keeps its address: the splice steals the
// interface's ingress, so the namespace's own stack can never receive a
// reply and cannot hold a connection over it, and leaving the address in
// place keeps the namespace looking normal to anything that inspects it
// -- including the VM container, which reads its own guest configuration
// back off that interface (DiscoverIdentity).
//
// It is idempotent for the same reason Setup is, so a retried init
// container converges on the same end state.
func SetupFlat(cfg Config) error {
	if len(cfg.VMs) != 1 {
		return fmt.Errorf("%s mode needs exactly one VM, got %d", ModeFlat, len(cfg.VMs))
	}
	vm := cfg.VMs[0]

	id, err := DiscoverIdentity(cfg.ExternalIface)
	if err != nil {
		return err
	}
	log.Printf("%s identity: %s/%d mac %s mtu %d", id.Iface, id.IP,
		maskBits(id.Mask), id.MAC, id.MTU)

	// The tap has to match the external interface's MTU exactly. A
	// splice is a wire: there is no bridge or router in between to
	// fragment an oversized frame or to answer with an ICMP
	// "fragmentation needed", so a mismatch here silently blackholes
	// large packets on any segment that isn't 1500 (a VXLAN overlay, or
	// most CNIs).
	tap, err := ensureTapMTU(vm.TapName(), id.MTU)
	if err != nil {
		return err
	}

	ext, err := netlink.LinkByName(cfg.ExternalIface)
	if err != nil {
		return fmt.Errorf("looking up %s: %w", cfg.ExternalIface, err)
	}

	if err := splice(ext, tap); err != nil {
		return err
	}
	log.Printf("spliced %s <-> %s", cfg.ExternalIface, vm.TapName())

	if cfg.ControlNet == nil {
		log.Printf("no control link configured; %q and the memory agent will not reach this guest", "kontur exec")
		return nil
	}

	// The control link is a second, private segment purely for traffic
	// between this namespace and the guest. It cannot share the spliced
	// interface: that interface's *egress* still goes to the veth peer,
	// not to the tap, so anything the namespace sends over it reaches
	// the container network rather than the guest. And the guest now
	// holds the namespace's own address, so dialing that address from
	// in here reaches the local stack instead. A separate NIC is the
	// only path back in.
	if err := ensureBridge(cfg.Bridge, cfg.ControlAddr, cfg.ControlNet); err != nil {
		return err
	}
	if err := ensureTap(vm.ControlTapName(), cfg.Bridge); err != nil {
		return err
	}
	log.Printf("control link %s on %s via %s", cfg.ControlAddr, cfg.Bridge, vm.ControlTapName())

	return nil
}

// ensureTapMTU creates the tap if it does not exist, sets its MTU and
// brings it up, returning the link. Unlike ensureTap it attaches the tap
// to no bridge: in flat mode the tap's only peer is the splice.
func ensureTapMTU(name string, mtu int) (netlink.Link, error) {
	if !linkExists(name) {
		tap := &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: name},
			Mode:      netlink.TUNTAP_MODE_TAP,
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return nil, fmt.Errorf("creating tap %s: %w", name, err)
		}
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("looking up tap %s: %w", name, err)
	}
	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		return nil, fmt.Errorf("setting tap %s MTU to %d: %w", name, mtu, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("bringing up tap %s: %w", name, err)
	}
	return link, nil
}

// FlatGuest is the guest-side half of a flat-mode setup: what to hand
// cloud-hypervisor as --net, and the kernel ip= parameter that makes the
// guest configure the identity it has taken over. It is derived rather
// than configured, so the same values come out whether the sandbox was
// created by docker, by a kubelet, or by hand.
type FlatGuest struct {
	// Nets holds one --net value per guest NIC: the spliced interface
	// first, then the control link if there is one.
	Nets []string

	// IPParam is the kernel ip= parameter configuring the first NIC.
	// The control NIC is not covered by it -- the kernel's own ip=
	// handling configures a single interface -- so a guest that wants
	// the control link has to bring its own second address up itself.
	IPParam string

	// ControlIP is the address the guest is expected to configure on the
	// control NIC, and therefore where this namespace reaches it (for
	// "kontur exec" and the memory agent). Empty when there is no
	// control link.
	ControlIP string
}

// FlatGuestConfig derives the guest configuration for a flat-mode VM from
// the identity discovered on the external interface. It expects the
// single-VM Config flat mode always produces, and returns nothing useful
// for any other.
func FlatGuestConfig(cfg Config, id Identity) FlatGuest {
	if len(cfg.VMs) != 1 {
		return FlatGuest{}
	}
	vm := cfg.VMs[0]

	// mac= is what makes the takeover invisible from outside: the guest
	// presents the very MAC the runtime put on the veth, so the segment
	// sees the same single endpoint it always did rather than a second
	// one appearing behind an authorized port.
	g := FlatGuest{
		Nets: []string{fmt.Sprintf("tap=%s,mac=%s,mtu=%d", vm.TapName(), id.MAC, id.MTU)},
	}

	gw := ""
	if id.Gateway != nil {
		gw = id.Gateway.String()
	}
	g.IPParam = fmt.Sprintf("ip=%s::%s:%s::eth0:off", id.IP, gw, id.Netmask())

	if cfg.ControlNet != nil {
		g.Nets = append(g.Nets, fmt.Sprintf("tap=%s", vm.ControlTapName()))
		g.ControlIP = ControlGuestIP(cfg.ControlAddr).String()
	}

	return g
}

// ControlGuestIP returns the address the guest is expected to hold on the
// control link, one past the address this namespace holds on it (so the
// default 169.254.100.1/24 control bridge puts the guest at
// 169.254.100.2, matching what the NAT path has always used).
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

// FlatEnabled reports whether the environment selects flat mode. The VM
// container uses it to decide whether to derive its own guest
// configuration from the namespace (see cmd/kontur's runVM) rather than
// taking CHV_NET and CHV_CMDLINE at face value.
func FlatEnabled() bool {
	return getEnvDefault(envMode, defaultMode) == ModeFlat
}

// WithIPParam appends ipParam to a kernel command line, unless it already
// configures an interface itself. An explicit ip= in CHV_CMDLINE is an
// operator overriding the derived identity on purpose, so it wins.
func WithIPParam(cmdline, ipParam string) string {
	if ipParam == "" {
		return cmdline
	}
	for _, field := range strings.Fields(cmdline) {
		if strings.HasPrefix(field, "ip=") {
			return cmdline
		}
	}
	if cmdline == "" {
		return ipParam
	}
	return cmdline + " " + ipParam
}
