package netshim

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
)

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

	if err := ensureMasquerade(cfg.BridgeNet, cfg.ExternalIface); err != nil {
		return err
	}
	if err := ensureForwarding(cfg.Bridge); err != nil {
		return err
	}

	for _, vm := range cfg.VMs {
		if err := ensurePortForward(cfg.ExternalIface, podIP, vm, cfg.GuestPort); err != nil {
			return err
		}
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
		if err := run("ip", "link", "add", name, "type", "bridge"); err != nil {
			return fmt.Errorf("creating bridge %s: %w", name, err)
		}
	}

	ones, _ := subnet.Mask.Size()
	cidr := fmt.Sprintf("%s/%d", addr, ones)
	if err := run("ip", "addr", "add", cidr, "dev", name); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("assigning %s to bridge %s: %w", cidr, name, err)
	}

	if err := run("ip", "link", "set", name, "up"); err != nil {
		return fmt.Errorf("bringing up bridge %s: %w", name, err)
	}
	return nil
}

func ensureTap(name, bridge string) error {
	if !linkExists(name) {
		if err := run("ip", "tuntap", "add", "dev", name, "mode", "tap"); err != nil {
			return fmt.Errorf("creating tap %s: %w", name, err)
		}
	}
	if err := run("ip", "link", "set", name, "master", bridge); err != nil {
		return fmt.Errorf("attaching tap %s to bridge %s: %w", name, bridge, err)
	}
	if err := run("ip", "link", "set", name, "up"); err != nil {
		return fmt.Errorf("bringing up tap %s: %w", name, err)
	}
	return nil
}

// ensureMasquerade lets VM-initiated outbound traffic leave via
// extIface looking like it came from the pod IP, so VMs don't need a
// routable address of their own.
func ensureMasquerade(bridgeNet *net.IPNet, extIface string) error {
	return ensureIPTablesRule("nat", "POSTROUTING",
		"-s", bridgeNet.String(), "-o", extIface, "-j", "MASQUERADE")
}

// ensureForwarding allows packets to flow between the bridge and the pod's
// external interface. Some CNIs default the FORWARD chain to DROP, so this
// can't be assumed already permitted.
func ensureForwarding(bridge string) error {
	if err := ensureIPTablesRule("filter", "FORWARD", "-i", bridge, "-j", "ACCEPT"); err != nil {
		return err
	}
	return ensureIPTablesRule("filter", "FORWARD", "-o", bridge, "-j", "ACCEPT")
}

// ensurePortForward makes vm.Port on the pod IP reach guestPort inside the
// VM. PREROUTING covers traffic arriving from outside the pod; OUTPUT
// covers traffic originated locally within the pod's own network
// namespace (which PREROUTING never sees).
func ensurePortForward(extIface string, podIP net.IP, vm VM, guestPort int) error {
	dest := fmt.Sprintf("%s:%d", vm.IP, guestPort)
	for _, proto := range []string{"tcp", "udp"} {
		if err := ensureIPTablesRule("nat", "PREROUTING",
			"-i", extIface, "-d", podIP.String(), "-p", proto, "--dport", fmt.Sprint(vm.Port),
			"-j", "DNAT", "--to-destination", dest); err != nil {
			return err
		}
		if err := ensureIPTablesRule("nat", "OUTPUT",
			"-d", podIP.String(), "-p", proto, "--dport", fmt.Sprint(vm.Port),
			"-j", "DNAT", "--to-destination", dest); err != nil {
			return err
		}
	}
	return nil
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

// ensureIPTablesRule appends rule to table/chain unless an identical rule
// is already present, so Setup can be safely re-run.
func ensureIPTablesRule(table, chain string, rule ...string) error {
	checkArgs := append([]string{"-t", table, "-C", chain}, rule...)
	if err := run("iptables", checkArgs...); err == nil {
		return nil // already present
	}
	appendArgs := append([]string{"-t", table, "-A", chain}, rule...)
	if err := run("iptables", appendArgs...); err != nil {
		return fmt.Errorf("adding iptables rule (-t %s -A %s %s): %w", table, chain, strings.Join(rule, " "), err)
	}
	return nil
}

func isAlreadyExists(err error) bool {
	return strings.Contains(err.Error(), "File exists")
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
