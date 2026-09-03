// Package netshim sets up the pod-local networking that lets several
// cloud-hypervisor VMs, each running as its own container in the same pod,
// share the pod's single IP. It is meant to run once, to completion, as a
// Kubernetes init container: by the time the VM containers start, the
// bridge, taps and NAT rules it creates are already in place and persist
// for the lifetime of the pod's network namespace.
package netshim

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

const (
	envMode          = "NETSHIM_MODE"
	envVM            = "NETSHIM_VM"
	envControlCIDR   = "NETSHIM_CONTROL_CIDR"
	envBridge        = "NETSHIM_BRIDGE"
	envBridgeCIDR    = "NETSHIM_BRIDGE_CIDR"
	envExternalIface = "NETSHIM_EXTERNAL_IFACE"
	envGuestPort     = "NETSHIM_GUEST_PORT"
	envVMs           = "NETSHIM_VMS"
	envDNS           = "NETSHIM_DNS"

	// ModeNAT is the original mode: a private subnet inside the
	// namespace, with DNAT/masquerade rules sharing the namespace's
	// single IP between one or more VMs. ModeFlat instead splices one
	// guest directly onto the namespace's own segment, where it takes
	// over the address and MAC the container runtime assigned -- see
	// SetupFlat. NAT remains the default: flat mode is one VM per
	// namespace by construction, and needs the segment to tolerate the
	// guest speaking for the endpoint.
	ModeNAT  = "nat"
	ModeFlat = "flat"

	defaultMode          = ModeNAT
	defaultControlCIDR   = "169.254.100.1/24"
	defaultBridge        = "kontur0"
	defaultBridgeCIDR    = "169.254.100.1/24"
	defaultExternalIface = "eth0"
	defaultGuestPort     = 80

	// DefaultDNS is the nameserver a guest is pointed at when nothing
	// names one -- a public resolver, because it is the only answer that
	// is right on an arbitrary host: the namespace's own resolver is
	// docker's embedded one on 127.0.0.11, which is the *namespace's*
	// loopback and not on the wire, and the host's resolver is routinely
	// an address (a cloud metadata service, a link-local stub) that
	// exists only in the host's own network namespace and is unroutable
	// from a guest on a tap. Neither is reachable from inside the guest,
	// and a guest pointed at one has open IP egress and hangs on every
	// name it looks up.
	//
	// It is a default rather than a fact: a deployment on a restricted
	// or air-gapped network names its own resolver instead
	// (NETSHIM_DNS, konturctl's -dns), and one that wants the guest
	// image's own /etc/resolv.conf left alone passes the empty string.
	DefaultDNS = "8.8.8.8"

	// maxDNS is how many nameservers the guest can be told about: the
	// ip= boot parameter has exactly two fields for them (dns0 and
	// dns1), so a third has nowhere to go.
	maxDNS = 2

	// tapPrefix is prepended to each VM's name to derive its tap device
	// name. Linux interface names are capped at 15 bytes, which bounds
	// how long a VM name may be.
	tapPrefix = "tap-"

	// controlTapPrefix names the flat-mode control link's tap. It is the
	// same length as tapPrefix so both derived names fit the same VM
	// name budget.
	controlTapPrefix = "ctl-"

	maxIfaceName = 15
)

// VM describes one guest that will attach to the shared bridge.
type VM struct {
	// Name identifies the VM within the pod. It must match the name a
	// sibling VM container uses when it references this VM's tap device
	// (TapName) as its own CHV_NET, e.g. "tap=tap-web,...".
	Name string

	// IP is the address this VM is assigned on the bridge subnet. The
	// guest must configure this same address on its own NIC (e.g. via a
	// kernel `ip=` boot parameter), since netshim only sets up the host
	// side.
	IP net.IP

	// Port is the port on the pod's external IP that inbound traffic to
	// this VM arrives on. It is forwarded to the VM's fixed in-guest
	// port (Config.GuestPort).
	Port int
}

// TapName is the name of the tap device netshim creates on the bridge for
// this VM.
func (vm VM) TapName() string {
	return tapPrefix + vm.Name
}

// ControlTapName is the name of the tap netshim creates for this VM's
// flat-mode control link -- the private path between the namespace and
// the guest that survives the guest taking over the namespace's own
// address. Unused in NAT mode.
func (vm VM) ControlTapName() string {
	return controlTapPrefix + vm.Name
}

// Config holds everything netshim needs to wire up one pod's network.
type Config struct {
	// Mode is ModeNAT or ModeFlat. It selects which of Setup/SetupFlat
	// applies, and therefore which of the fields below are meaningful:
	// flat mode uses Bridge, ExternalIface, the Control* fields and a
	// single entry in VMs, and ignores the rest.
	Mode string

	// ControlAddr and ControlNet are the address netshim holds on the
	// flat-mode control link, and its subnet. Both nil when the control
	// link is disabled (NETSHIM_CONTROL_CIDR set to the empty string),
	// which leaves the guest reachable only from the container network.
	ControlAddr net.IP
	ControlNet  *net.IPNet

	// Bridge is the name of the Linux bridge netshim creates. Every VM's
	// tap device is attached to it.
	Bridge string

	// BridgeAddr is the bridge's own address, and BridgeNet the subnet
	// it (and every VM) lives on. Parsed from NETSHIM_BRIDGE_CIDR.
	BridgeAddr net.IP
	BridgeNet  *net.IPNet

	// ExternalIface is the pod's primary interface, the one carrying the
	// pod IP that inbound traffic actually arrives on.
	ExternalIface string

	// DNS is the nameservers the guest is told to resolve through, at
	// most maxDNS of them, parsed from NETSHIM_DNS. They reach the guest
	// on its ip= boot parameter (FlatGuestConfig), where
	// kontur-configure-dns writes them into /etc/resolv.conf. Empty
	// leaves the guest resolving through whatever its image ships with.
	//
	// Flat mode only: in NAT mode konturctl derives the whole ip=
	// parameter itself, nameservers included (internal/staticpod), and
	// netshim never sees it.
	DNS []net.IP

	// GuestPort is the single fixed port every VM is expected to listen
	// on internally. Each VM gets its own external port (VM.Port) on the
	// pod IP, but all of them forward to this same in-guest port.
	GuestPort int

	VMs []VM
}

// FromEnv builds a Config from the process environment and validates it,
// dispatching on NETSHIM_MODE to whichever of the two modes' own settings
// apply.
func FromEnv() (Config, error) {
	switch mode := getEnvDefault(envMode, defaultMode); mode {
	case ModeNAT:
		return natFromEnv()
	case ModeFlat:
		return flatFromEnv()
	default:
		return Config{}, fmt.Errorf("%s: unknown mode %q, want %q or %q",
			envMode, mode, ModeNAT, ModeFlat)
	}
}

// flatFromEnv builds a flat-mode Config. Flat mode is one VM per network
// namespace by construction -- there is exactly one identity to take over
// -- so it takes a single VM name rather than NETSHIM_VMS' list, and
// needs neither an address nor a port for it: docker (or the CNI) already
// chose the address, and ports are published on the sandbox the ordinary
// way rather than forwarded by rules of netshim's own.
func flatFromEnv() (Config, error) {
	cfg := Config{
		Mode:          ModeFlat,
		Bridge:        getEnvDefault(envBridge, defaultBridge),
		ExternalIface: getEnvDefault(envExternalIface, defaultExternalIface),
	}

	dns, err := dnsFromEnv()
	if err != nil {
		return Config{}, err
	}
	cfg.DNS = dns

	name := strings.TrimSpace(os.Getenv(envVM))
	if name == "" {
		return Config{}, fmt.Errorf("%s is required in %s mode", envVM, ModeFlat)
	}
	vm := VM{Name: name}
	if len(vm.TapName()) > maxIfaceName {
		return Config{}, fmt.Errorf("%s: VM %q name too long: tap device name %q exceeds %d characters",
			envVM, name, vm.TapName(), maxIfaceName)
	}
	cfg.VMs = []VM{vm}

	// An explicitly empty NETSHIM_CONTROL_CIDR disables the control link
	// altogether, for a guest that only ever needs to be reached from
	// the container network. That costs "kontur exec" and the memory
	// agent, both of which run inside this namespace and so have no
	// other way to the guest once it holds the namespace's own address.
	raw, ok := os.LookupEnv(envControlCIDR)
	if ok && raw == "" {
		return cfg, nil
	}
	addr, ipnet, err := net.ParseCIDR(getEnvDefault(envControlCIDR, defaultControlCIDR))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", envControlCIDR, err)
	}
	if addr.To4() == nil {
		return Config{}, fmt.Errorf("%s: %s is not an IPv4 address", envControlCIDR, addr)
	}
	if len(vm.ControlTapName()) > maxIfaceName {
		return Config{}, fmt.Errorf("%s: VM %q name too long: control tap device name %q exceeds %d characters",
			envVM, name, vm.ControlTapName(), maxIfaceName)
	}
	cfg.ControlAddr = addr.To4()
	cfg.ControlNet = ipnet

	return cfg, nil
}

// natFromEnv builds a NAT-mode Config: the private bridge subnet, one tap
// per VM, and the port each VM is reached on through the namespace's
// shared IP.
func natFromEnv() (Config, error) {
	cfg := Config{
		Mode:          ModeNAT,
		Bridge:        getEnvDefault(envBridge, defaultBridge),
		ExternalIface: getEnvDefault(envExternalIface, defaultExternalIface),
	}

	addr, ipnet, err := net.ParseCIDR(getEnvDefault(envBridgeCIDR, defaultBridgeCIDR))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", envBridgeCIDR, err)
	}
	cfg.BridgeAddr = addr
	cfg.BridgeNet = ipnet

	cfg.GuestPort, err = getEnvInt(envGuestPort, defaultGuestPort)
	if err != nil {
		return Config{}, err
	}
	if err := validatePort(cfg.GuestPort); err != nil {
		return Config{}, fmt.Errorf("%s: %w", envGuestPort, err)
	}

	vmsSpec := os.Getenv(envVMs)
	if vmsSpec == "" {
		return Config{}, fmt.Errorf("%s is required", envVMs)
	}
	seen := make(map[string]bool)
	for _, spec := range strings.Split(vmsSpec, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		vm, err := parseVM(spec)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", envVMs, err)
		}
		if seen[vm.Name] {
			return Config{}, fmt.Errorf("%s: duplicate VM name %q", envVMs, vm.Name)
		}
		seen[vm.Name] = true
		if !cfg.BridgeNet.Contains(vm.IP) {
			return Config{}, fmt.Errorf("%s: VM %q address %s is not in bridge subnet %s", envVMs, vm.Name, vm.IP, cfg.BridgeNet)
		}
		if len(vm.TapName()) > maxIfaceName {
			return Config{}, fmt.Errorf("%s: VM %q name too long: tap device name %q exceeds %d characters", envVMs, vm.Name, vm.TapName(), maxIfaceName)
		}
		cfg.VMs = append(cfg.VMs, vm)
	}
	if len(cfg.VMs) == 0 {
		return Config{}, fmt.Errorf("%s: no VMs specified", envVMs)
	}

	return cfg, nil
}

// dnsFromEnv reads NETSHIM_DNS. Unset means DefaultDNS; set to the empty
// string means no nameservers at all, the same "explicitly empty disables
// it" shape NETSHIM_CONTROL_CIDR already has -- which is how a guest whose
// image ships a resolver of its own keeps it.
func dnsFromEnv() ([]net.IP, error) {
	raw, ok := os.LookupEnv(envDNS)
	if !ok {
		raw = DefaultDNS
	}
	dns, err := ParseDNS(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envDNS, err)
	}
	return dns, nil
}

// ParseDNS parses a comma-separated list of nameserver addresses, as
// NETSHIM_DNS and konturctl's own -dns both spell them. The empty string
// (or a list of nothing but separators and spaces) parses to no
// nameservers, which is how both spell "leave the guest's resolver
// alone".
//
// IPv4 only, and at most two: they travel to the guest in the ip= boot
// parameter's dns0/dns1 fields, which are neither more nor other than
// that. Rejected here rather than silently truncated -- an operator who
// listed three resolvers is owed the news that the third is unreachable
// from the guest.
func ParseDNS(spec string) ([]net.IP, error) {
	var dns []net.IP
	for _, field := range strings.Split(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		ip := net.ParseIP(field)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("invalid IPv4 nameserver address %q", field)
		}
		dns = append(dns, ip.To4())
	}
	if len(dns) > maxDNS {
		return nil, fmt.Errorf("at most %d nameservers fit the guest's ip= boot parameter, got %d", maxDNS, len(dns))
	}
	return dns, nil
}

// parseVM parses one "name:ip:port" entry from NETSHIM_VMS.
func parseVM(spec string) (VM, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		return VM{}, fmt.Errorf("malformed VM spec %q, want \"name:ip:port\"", spec)
	}
	name, ipStr, portStr := parts[0], parts[1], parts[2]
	if name == "" {
		return VM{}, fmt.Errorf("empty VM name in %q", spec)
	}
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.To4() == nil {
		return VM{}, fmt.Errorf("invalid IPv4 address %q in %q", ipStr, spec)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return VM{}, fmt.Errorf("invalid port %q in %q", portStr, spec)
	}
	if err := validatePort(port); err != nil {
		return VM{}, fmt.Errorf("%w in %q", err, spec)
	}
	return VM{Name: name, IP: ip.To4(), Port: port}, nil
}

func validatePort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("port %d out of range 1-65535", p)
	}
	return nil
}

func getEnvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", key, v)
	}
	return n, nil
}
