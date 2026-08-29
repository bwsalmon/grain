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
	envBridge        = "NETSHIM_BRIDGE"
	envBridgeCIDR    = "NETSHIM_BRIDGE_CIDR"
	envExternalIface = "NETSHIM_EXTERNAL_IFACE"
	envGuestPort     = "NETSHIM_GUEST_PORT"
	envVMs           = "NETSHIM_VMS"

	defaultBridge        = "kontur0"
	defaultBridgeCIDR    = "169.254.100.1/24"
	defaultExternalIface = "eth0"
	defaultGuestPort     = 80

	// tapPrefix is prepended to each VM's name to derive its tap device
	// name. Linux interface names are capped at 15 bytes, which bounds
	// how long a VM name may be.
	tapPrefix    = "tap-"
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

// Config holds everything netshim needs to wire up one pod's network.
type Config struct {
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

	// GuestPort is the single fixed port every VM is expected to listen
	// on internally. Each VM gets its own external port (VM.Port) on the
	// pod IP, but all of them forward to this same in-guest port.
	GuestPort int

	VMs []VM
}

// FromEnv builds a Config from the process environment and validates it.
func FromEnv() (Config, error) {
	cfg := Config{
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
