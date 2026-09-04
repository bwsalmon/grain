package netshim

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"

	"github.com/vishvananda/netlink"
)

// Setup wires up the guest's networking: it is spliced directly onto the
// container's own network segment and inherits its identity, so from
// outside the namespace there is still exactly one endpoint, with the
// address and MAC the runtime allocated.
//
// Nothing here rewrites, routes or filters a packet. Once the splice is
// programmed the kernel moves every frame between the external interface
// and the tap on its own, so this runs once to completion (as an init
// container, or as one "docker run" before the VM container's) and exits,
// leaving state that lives as long as the namespace does.
//
// Two things it deliberately does not do: it never writes
// net.ipv4.ip_forward (there is no routing), and it installs no nftables
// rules (there is no NAT). netshim therefore needs only CAP_NET_ADMIN and
// access to /dev/net/tun, not a fully privileged container.
//
// The external interface keeps its address: the splice steals the
// interface's ingress, so the namespace's own stack can never receive a
// reply and cannot hold a connection over it, and leaving the address in
// place keeps the namespace looking normal to anything that inspects it
// -- including the VM container, which reads its own guest configuration
// back off that interface (DiscoverIdentity).
//
// It is idempotent, so a retried init container converges on the same end
// state rather than erroring on things that already exist.
func Setup(cfg Config) error {
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
	tap, err := ensureTapMTU(cfg.TapName(), id.MTU)
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
	log.Printf("spliced %s <-> %s", cfg.ExternalIface, cfg.TapName())

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
	if err := ensureTap(cfg.ControlTapName(), cfg.Bridge); err != nil {
		return err
	}
	log.Printf("control link %s on %s via %s", cfg.ControlAddr, cfg.Bridge, cfg.ControlTapName())

	return nil
}

// ensureTapMTU creates the tap if it does not exist, sets its MTU and
// brings it up, returning the link. Unlike ensureTap it attaches the tap
// to no bridge: the spliced tap's only peer is the external interface.
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

func ensureBridge(name string, addr net.IP, subnet *net.IPNet) error {
	if !linkExists(name) {
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if err := netlink.LinkAdd(br); err != nil {
			return fmt.Errorf("creating bridge %s: %w", name, err)
		}
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("looking up bridge %s: %w", name, err)
	}

	ones, _ := subnet.Mask.Size()
	nlAddr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", addr, ones))
	if err != nil {
		return fmt.Errorf("parsing bridge address %s/%d: %w", addr, ones, err)
	}
	if err := netlink.AddrAdd(link, nlAddr); err != nil && !isEExist(err) {
		return fmt.Errorf("assigning %s to bridge %s: %w", nlAddr, name, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bringing up bridge %s: %w", name, err)
	}
	return nil
}

func ensureTap(name, bridge string) error {
	if !linkExists(name) {
		tap := &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: name},
			Mode:      netlink.TUNTAP_MODE_TAP,
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return fmt.Errorf("creating tap %s: %w", name, err)
		}
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("looking up tap %s: %w", name, err)
	}
	bridgeLink, err := netlink.LinkByName(bridge)
	if err != nil {
		return fmt.Errorf("looking up bridge %s: %w", bridge, err)
	}
	if err := netlink.LinkSetMaster(link, bridgeLink); err != nil {
		return fmt.Errorf("attaching tap %s to bridge %s: %w", name, bridge, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bringing up tap %s: %w", name, err)
	}
	return nil
}

func linkExists(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

func isEExist(err error) bool {
	return errors.Is(err, syscall.EEXIST) || errors.Is(err, os.ErrExist)
}
