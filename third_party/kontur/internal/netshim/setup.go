package netshim

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// natTable is the name of the dedicated nftables table Setup creates for
// all the NAT/forwarding rules it needs. Setup owns this table outright:
// every run deletes and recreates it from scratch (see ensureNftRules),
// rather than trying to diff against whatever rules happen to already be
// there, so a stale rule from a previous run with a different set of VMs
// can never linger.
const natTable = "kontur"

// Setup brings up the bridge, one tap per VM, and the NAT rules that let
// every VM share the pod's IP. It is idempotent: run twice (e.g. because
// Kubernetes retried a failed init container), it leaves the same end
// state rather than erroring on things that already exist.
func Setup(cfg Config) error {
	if err := enableIPForward(); err != nil {
		return err
	}

	if err := ensureBridge(cfg.Bridge, cfg.BridgeAddr, cfg.BridgeNet); err != nil {
		return err
	}

	for _, vm := range cfg.VMs {
		if err := ensureTap(vm.TapName(), cfg.Bridge); err != nil {
			return err
		}
	}

	podIP, err := primaryAddr(cfg.ExternalIface)
	if err != nil {
		return err
	}
	log.Printf("pod IP on %s is %s", cfg.ExternalIface, podIP)

	if err := ensureNftRules(cfg, podIP); err != nil {
		return err
	}

	return nil
}

func enableIPForward() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enabling IPv4 forwarding: %w", err)
	}
	return nil
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

// ensureNftRules (re)installs the NAT/forwarding rules that let every VM
// share the pod's IP, in a dedicated "kontur" nftables table:
//
//   - postrouting: MASQUERADEs VM-initiated outbound traffic (source in
//     bridgeNet, leaving via extIface) so it looks like it came from the
//     pod IP, since VMs don't have a routable address of their own.
//   - forward: some CNIs default the FORWARD hook to drop, so traffic
//     to/from the bridge has to be explicitly accepted.
//   - prerouting/output: DNAT vm.Port on the pod IP to guestPort inside
//     the VM. prerouting covers traffic arriving from outside the pod;
//     output covers traffic originated locally within the pod's own
//     network namespace (which prerouting never sees).
//
// The table is deleted and rebuilt from scratch on every call rather than
// diffed against its previous contents, so Setup stays idempotent (no
// duplicate rules on a second run) without having to compare individual
// rules, and a VM removed from cfg since the last run never leaves a
// stale rule behind.
func ensureNftRules(cfg Config, podIP net.IP) error {
	conn := &nftables.Conn{}

	if old, err := conn.ListTables(); err == nil {
		for _, t := range old {
			if t.Name == natTable && t.Family == nftables.TableFamilyIPv4 {
				conn.DelTable(t)
			}
		}
		if err := conn.Flush(); err != nil {
			return fmt.Errorf("removing existing %s nftables table: %w", natTable, err)
		}
	}

	table := conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   natTable,
	})

	postrouting := conn.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	})
	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: postrouting,
		Exprs: masqueradeExprs(cfg.BridgeNet, cfg.ExternalIface),
	})

	forward := conn.AddChain(&nftables.Chain{
		Name:     "forward",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
	})
	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: forward,
		Exprs: acceptIifExprs(expr.MetaKeyIIFNAME, cfg.Bridge),
	})
	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: forward,
		Exprs: acceptIifExprs(expr.MetaKeyOIFNAME, cfg.Bridge),
	})

	prerouting := conn.AddChain(&nftables.Chain{
		Name:     "prerouting",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityNATDest,
	})
	output := conn.AddChain(&nftables.Chain{
		Name:     "output",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityNATDest,
	})

	for _, vm := range cfg.VMs {
		for _, proto := range []uint8{unix.IPPROTO_TCP, unix.IPPROTO_UDP} {
			conn.AddRule(&nftables.Rule{
				Table: table,
				Chain: prerouting,
				Exprs: dnatExprs(cfg.ExternalIface, podIP, vm, cfg.GuestPort, proto, true),
			})
			conn.AddRule(&nftables.Rule{
				Table: table,
				Chain: output,
				Exprs: dnatExprs(cfg.ExternalIface, podIP, vm, cfg.GuestPort, proto, false),
			})
		}
	}

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("installing %s nftables rules: %w", natTable, err)
	}
	return nil
}

// masqueradeExprs matches packets whose source address is in bridgeNet and
// which are leaving via extIface.
func masqueradeExprs(bridgeNet *net.IPNet, extIface string) []expr.Any {
	return []expr.Any{
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       12, // IPv4 source address
			Len:          4,
		},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           []byte(bridgeNet.Mask),
			Xor:            []byte{0, 0, 0, 0},
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     bridgeNet.IP.To4(),
		},
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(extIface),
		},
		&expr.Masq{},
	}
}

// acceptIifExprs matches packets whose interface (as selected by key,
// MetaKeyIIFNAME or MetaKeyOIFNAME) is iface, and accepts them.
func acceptIifExprs(key expr.MetaKey, iface string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifname(iface),
		},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// dnatExprs matches proto traffic addressed to podIP:vm.Port and DNATs it
// to vm.IP:guestPort. If matchIface, the match additionally requires the
// packet to have arrived on extIface (for the prerouting chain); the
// output chain, which only ever sees locally-originated traffic, omits
// that check since it has no meaningful incoming interface.
func dnatExprs(extIface string, podIP net.IP, vm VM, guestPort int, proto uint8, matchIface bool) []expr.Any {
	exprs := []expr.Any{}
	if matchIface {
		exprs = append(exprs,
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(extIface)},
		)
	}
	exprs = append(exprs,
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       9, // IPv4 protocol
			Len:          1,
		},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       16, // IPv4 destination address
			Len:          4,
		},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: podIP.To4()},
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2, // destination port (tcp and udp agree on this offset)
			Len:          2,
		},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(uint16(vm.Port))},
		&expr.Immediate{Register: 2, Data: vm.IP.To4()},
		&expr.Immediate{Register: 3, Data: binaryutil.BigEndian.PutUint16(uint16(guestPort))},
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      unix.NFPROTO_IPV4,
			RegAddrMin:  2,
			RegProtoMin: 3,
		},
	)
	return exprs
}

// ifname encodes iface the way nftables interface-name matches expect: a
// fixed 16-byte (IFNAMSIZ), NUL-padded buffer.
func ifname(iface string) []byte {
	b := make([]byte, 16)
	copy(b, iface)
	return b
}

// primaryAddr returns the first IPv4 address configured on iface, i.e. the
// pod's IP as assigned by the cluster's CNI.
func primaryAddr(iface string) (net.IP, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("looking up interface %s: %w", iface, err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, fmt.Errorf("reading addresses on %s: %w", iface, err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4, nil
		}
	}
	return nil, fmt.Errorf("no IPv4 address found on %s", iface)
}

func linkExists(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

func isEExist(err error) bool {
	return errors.Is(err, syscall.EEXIST) || errors.Is(err, os.ErrExist)
}
