// Package netshim splices a single cloud-hypervisor VM directly onto the
// network segment its container runtime already put the sandbox on, so
// the guest takes over the address and MAC that runtime assigned. It is
// meant to run once, to completion, before the VM container starts -- as
// a Kubernetes init container, or as one "docker run" ahead of the VM's:
// by the time the VM container starts, the tap, the splice and the
// control link it creates are already in place and persist for the
// lifetime of the sandbox's network namespace.
//
// One VM per network namespace is the whole model: a pod runs a single VM
// container, and that VM is the namespace's one endpoint, so from outside
// it behaves like an ordinary container -- Services, "-p", "--network"
// membership -- because all of those are properties of the sandbox rather
// than of anything netshim installs.
package netshim

import (
	"fmt"
	"net"
	"os"
	"strings"
)

const (
	envVM            = "NETSHIM_VM"
	envControlCIDR   = "NETSHIM_CONTROL_CIDR"
	envBridge        = "NETSHIM_BRIDGE"
	envExternalIface = "NETSHIM_EXTERNAL_IFACE"
	envDNS           = "NETSHIM_DNS"

	defaultControlCIDR   = "169.254.100.1/24"
	defaultBridge        = "kontur0"
	defaultExternalIface = "eth0"

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
	// It is a default rather than a fact: a deployment on a restricted or
	// air-gapped network names its own resolver instead (NETSHIM_DNS,
	// konturctl's -dns), and one that wants the guest image's own
	// /etc/resolv.conf left alone passes the empty string.
	DefaultDNS = "8.8.8.8"

	// maxDNS is how many nameservers the guest can be told about: the
	// ip= boot parameter has exactly two fields for them (dns0 and
	// dns1), so a third has nowhere to go.
	maxDNS = 2

	// tapPrefix is prepended to the VM's name to derive its tap device
	// name. Linux interface names are capped at 15 bytes, which bounds
	// how long a VM name may be.
	tapPrefix = "tap-"

	// controlTapPrefix names the control link's tap. It is the same
	// length as tapPrefix so both derived names fit the same VM name
	// budget.
	controlTapPrefix = "ctl-"

	maxIfaceName = 15
)

// Config holds everything netshim needs to wire up one namespace's single
// VM.
type Config struct {
	// VM is the name of the guest this namespace runs. It must match the
	// name the VM container was given, since both derive the same tap
	// device name from it.
	VM string

	// ControlAddr and ControlNet are the address netshim holds on the
	// control link, and its subnet. Both nil when the control link is
	// disabled (NETSHIM_CONTROL_CIDR set to the empty string), which
	// leaves the guest reachable only from the container network.
	ControlAddr net.IP
	ControlNet  *net.IPNet

	// Bridge is the name of the Linux bridge holding the control link's
	// host end. Nothing on the guest's own data path goes through it:
	// that is the splice.
	Bridge string

	// ExternalIface is the sandbox's primary interface, the one carrying
	// the address the guest takes over.
	ExternalIface string

	// DNS is the nameservers the guest is told to resolve through, at
	// most maxDNS of them, parsed from NETSHIM_DNS. They reach the guest
	// on its ip= boot parameter (GuestConfig), where
	// kontur-configure-dns writes them into /etc/resolv.conf. Empty
	// leaves the guest resolving through whatever its image ships with.
	DNS []net.IP
}

// TapName is the name of the tap device netshim splices onto the external
// interface, and which the guest's first NIC is backed by.
func (c Config) TapName() string {
	return tapPrefix + c.VM
}

// ControlTapName is the name of the tap netshim creates for the control
// link -- the private path between this namespace and the guest that
// survives the guest taking over the namespace's own address.
func (c Config) ControlTapName() string {
	return controlTapPrefix + c.VM
}

// FromEnv builds a Config from the process environment and validates it.
// It needs neither an address nor a port for the VM: docker (or the CNI)
// already chose the address, and ports are published on the sandbox the
// ordinary way rather than forwarded by rules of netshim's own.
func FromEnv() (Config, error) {
	cfg := Config{
		Bridge:        getEnvDefault(envBridge, defaultBridge),
		ExternalIface: getEnvDefault(envExternalIface, defaultExternalIface),
	}

	dns, err := dnsFromEnv()
	if err != nil {
		return Config{}, err
	}
	cfg.DNS = dns

	cfg.VM = strings.TrimSpace(os.Getenv(envVM))
	if cfg.VM == "" {
		return Config{}, fmt.Errorf("%s is required", envVM)
	}
	if len(cfg.TapName()) > maxIfaceName {
		return Config{}, fmt.Errorf("%s: VM %q name too long: tap device name %q exceeds %d characters",
			envVM, cfg.VM, cfg.TapName(), maxIfaceName)
	}

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
	if len(cfg.ControlTapName()) > maxIfaceName {
		return Config{}, fmt.Errorf("%s: VM %q name too long: control tap device name %q exceeds %d characters",
			envVM, cfg.VM, cfg.ControlTapName(), maxIfaceName)
	}
	cfg.ControlAddr = addr.To4()
	cfg.ControlNet = ipnet

	return cfg, nil
}

// Enabled reports whether this container's environment describes a
// netshim-managed guest at all, i.e. whether NETSHIM_VM is set. The VM
// container uses it to decide whether to derive its own guest
// configuration from the namespace (see cmd/kontur's runVM) rather than
// taking CHV_NET and CHV_CMDLINE at face value -- a "kontur run" with no
// netshim in front of it (the self-contained pod example, or a plain
// "docker run kontur") has no tap to attach to and no identity to take
// over, and is left exactly as configured.
func Enabled() bool {
	return strings.TrimSpace(os.Getenv(envVM)) != ""
}

func getEnvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
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
