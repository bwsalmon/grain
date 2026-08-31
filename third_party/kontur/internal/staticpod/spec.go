// Package staticpod defines a VM's configuration (VMSpec) and its
// persistence as JSON under konturctl's state directory -- shared by both
// of konturctl's backends (see the Backend* constants below) -- plus
// BackendStaticPod's own half: generating the static pod manifests that
// the standalone kubelet set up under deploy/static-kubelet/ watches (see
// its README). BackendDocker's equivalent (running the same containers
// directly against a local docker daemon instead) lives in
// internal/dockervm. Each VM gets its own single-container pod (plus the
// shared netshim init container pattern documented in the top-level
// README) rather than grouping several VMs into one pod, trading the
// port-sharing trick the hand-written multi-VM examples show for a simple,
// independent create/update/delete-by-name lifecycle.
package staticpod

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/kontur/internal/netshim"
)

const (
	// ImagesMountPath is the container path -images-hostpath is always
	// mounted at, read-only, under both backends: it's a shared
	// node-local image cache several VMs may read from concurrently, so
	// it's never made writable regardless of -disk-readonly (see
	// manifest.go and internal/dockervm). A writable -disk must live
	// under this path, since it's the only place konturctl's backends
	// ever mount images from.
	//
	// Must match the literal "/images" mountPath/hostPath entries in
	// manifestTemplateSrc (manifest.go) and the literal ":/images:ro"
	// mount in internal/dockervm/docker.go.
	ImagesMountPath = "/images"

	// DiskMountPath is the container path a VM's own private writable
	// qcow2 overlay (see WritableDiskDir, PrepareWritableDisk) is mounted
	// read-write at, used only when DiskReadOnly is false.
	//
	// Must match the literal "/disk" mountPath/hostPath entries in
	// manifestTemplateSrc (manifest.go).
	DiskMountPath = "/disk"

	// writableDiskFileName is the fixed name a VM's private writable
	// qcow2 overlay is stored under within WritableDiskDir, regardless of
	// the source image's own filename under ImagesMountPath -- so
	// ResolvedDiskImage stays stable even if -disk changes across a "vm
	// update".
	writableDiskFileName = "disk.qcow2"
)

// VMSpec holds every parameter needed to run one VM, under either
// backend. It is also what gets persisted to the state directory (as
// JSON) so a later "update" can start from the previous values instead of
// requiring every flag to be repeated.
type VMSpec struct {
	Name string `json:"name"`

	DiskImage    string `json:"diskImage"`
	DiskReadOnly bool   `json:"diskReadOnly"`
	Kernel       string `json:"kernel,omitempty"`
	Initramfs    string `json:"initramfs,omitempty"`
	Firmware     string `json:"firmware,omitempty"`
	Cmdline      string `json:"cmdline,omitempty"`

	// CmdlineAuto records whether Cmdline was derived automatically (from
	// IP/DiskReadOnly/BridgeCIDR) rather than given explicitly, so a later
	// "konturctl vm update" that changes one of those inputs without also
	// passing --cmdline knows to recompute it instead of keeping the now
	// stale value.
	CmdlineAuto bool `json:"cmdlineAuto,omitempty"`

	CPUs            int    `json:"cpus"`
	MemoryMB        int    `json:"memoryMB"`
	ShutdownTimeout string `json:"shutdownTimeout"`

	IP        string `json:"ip"`
	Port      int    `json:"port"`
	GuestPort int    `json:"guestPort"`

	Bridge        string `json:"bridge"`
	BridgeCIDR    string `json:"bridgeCIDR"`
	ExternalIface string `json:"externalIface"`

	// NetMode selects how the guest reaches the network: netshim.ModeNAT
	// (the default) puts it behind a private subnet on Bridge, sharing
	// the namespace's IP through Port. netshim.ModeFlat instead splices
	// it straight onto the namespace's own segment, where it takes over
	// the address and MAC the container runtime assigned -- so IP, Port,
	// GuestPort and BridgeCIDR are all unused, and ports are published
	// on the sandbox itself (see DockerRunOpts) like any other
	// container's.
	NetMode string `json:"netMode,omitempty"`

	// ControlCIDR is the address netshim holds on the flat-mode control
	// link, the private second NIC that keeps "kontur exec" and the
	// memory agent able to reach a guest that now answers to the
	// namespace's own address. Set to the empty string to omit the
	// control link entirely. Unused in NAT mode.
	ControlCIDR string `json:"controlCIDR,omitempty"`

	// DockerRunOpts are extra options passed verbatim to the "docker
	// run" that creates the network namespace holder (-backend docker
	// only). Port publishing, network membership and DNS all have to be
	// set on the container that owns the namespace, and cannot be added
	// afterwards by the containers that join it, so this is the only
	// place they can go -- e.g. []string{"-p", "8080:80", "--network",
	// "mynet"}.
	DockerRunOpts []string `json:"dockerRunOpts,omitempty"`

	ImagesHostPath string `json:"imagesHostPath"`

	// DiskHostPath is the host directory a VM's own private writable
	// qcow2 overlay is stored under (see WritableDiskDir), one
	// subdirectory per VM name. Only used when DiskReadOnly is false:
	// ImagesHostPath itself is always mounted read-only (see
	// ImagesMountPath), so a genuinely writable disk needs to live
	// somewhere else entirely.
	DiskHostPath string `json:"diskHostPath,omitempty"`

	// KonturImage is the single OCI image used for both the netshim init
	// container and the VM container: the same kontur binary, invoked
	// with different args ("netshim" vs "run") for each role.
	KonturImage string `json:"konturImage"`

	TerminationGracePeriodSeconds int `json:"terminationGracePeriodSeconds"`

	// StaticPodPath is the directory the rendered manifest is written to
	// (and removed from, on delete). Persisted so later "update"/"delete"
	// calls don't need it repeated. Only meaningful for BackendStaticPod.
	StaticPodPath string `json:"staticPodPath"`

	// Backend selects how this VM's containers actually get run: see the
	// Backend* constants below. Persisted (like every other field here)
	// so later "update"/"delete"/"list" calls don't need it repeated --
	// in particular so "vm update" and "vm delete" know which backend to
	// drive without it being re-specified. Empty means BackendStaticPod,
	// for specs saved before this field existed.
	Backend string `json:"backend,omitempty"`
}

const (
	// BackendStaticPod renders a static pod manifest into StaticPodPath
	// for the standalone kubelet described in
	// deploy/static-kubelet/README.md to run. This is konturctl's
	// original, default backend.
	BackendStaticPod = "static-pod"

	// BackendDocker runs the same netshim/VM container pair directly
	// against a local docker daemon (see internal/dockervm), for hosts
	// that don't want to install containerd/CNI/kubelet at all.
	BackendDocker = "docker"
)

// Defaults returns a VMSpec with every field the CLI defaults if left
// unset, matching the defaults documented for kontur in the top-level
// README wherever konturctl's own architecture doesn't force a stricter
// choice. DiskReadOnly is the one field that diverges from kontur run's
// own CHV_DISK_READONLY=false default: both backends mount -images-hostpath
// read-only into the container (a shared node-local image cache several
// VMs may read at once), so a writable disk is never actually possible
// through konturctl regardless of this flag -- defaulting it to true
// avoids a boot failure ("Read-only file system") on the flag's own
// default value.
func Defaults() VMSpec {
	return VMSpec{
		DiskReadOnly:                  true,
		CPUs:                          2,
		MemoryMB:                      2048,
		ShutdownTimeout:               "20s",
		GuestPort:                     80,
		NetMode:                       netshim.ModeNAT,
		ControlCIDR:                   "169.254.100.1/24",
		Bridge:                        "kontur0",
		BridgeCIDR:                    "169.254.100.1/24",
		ExternalIface:                 "eth0",
		ImagesHostPath:                "/var/lib/vm-images",
		DiskHostPath:                  "/var/lib/kontur/vm-disks",
		KonturImage:                   "localhost:5000/kontur:latest",
		TerminationGracePeriodSeconds: 40,
		StaticPodPath:                 "/etc/kubernetes/manifests",
		Backend:                       BackendStaticPod,
	}
}

// BackendOrDefault returns s.Backend, treating an empty value (a spec
// saved before Backend existed) as BackendStaticPod.
func (s VMSpec) BackendOrDefault() string {
	if s.Backend == "" {
		return BackendStaticPod
	}
	return s.Backend
}

// sortByName sorts specs in place by Name.
func sortByName(specs []VMSpec) {
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
}

// Validate checks that spec is complete and internally consistent, filling
// in a default Cmdline (derived from IP/BridgeCIDR) unless Firmware is set
// -- Cmdline only applies to direct kernel boot, which is still what
// happens even with Kernel left empty (kontur run's own CHV_KERNEL
// default then applies, see internal/config's defaultKernel).
func (s *VMSpec) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	// netshim names each VM's tap device "tap-<name>" and Linux interface
	// names are capped at 15 characters (IFNAMSIZ-1); catch an
	// over-length name here, at "vm create"/"vm update" time, rather than
	// as a netshim init container crash loop once the pod's already been
	// submitted.
	if tapName := "tap-" + s.Name; len(tapName) > 15 {
		return fmt.Errorf("name %q too long: tap device name %q would exceed 15 characters", s.Name, tapName)
	}
	if s.DiskImage == "" {
		return fmt.Errorf("disk image is required")
	}
	if !s.DiskReadOnly {
		if !strings.HasPrefix(s.DiskImage, ImagesMountPath+"/") {
			return fmt.Errorf("a writable disk (-disk-readonly=false) must be under %s (the shared -images-hostpath mount, the only place a source image can be used as a qcow2 backing file from), got %q", ImagesMountPath, s.DiskImage)
		}
		if s.DiskHostPath == "" {
			return fmt.Errorf("disk-hostpath is required when disk-readonly is false")
		}
	}
	s.NetMode = s.NetModeOrDefault()
	switch s.NetMode {
	case netshim.ModeNAT:
		if s.IP == "" {
			return fmt.Errorf("ip is required")
		}
		ip := net.ParseIP(s.IP)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("invalid IPv4 address %q", s.IP)
		}
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("port %d out of range 1-65535", s.Port)
		}
		if s.GuestPort < 1 || s.GuestPort > 65535 {
			return fmt.Errorf("guest port %d out of range 1-65535", s.GuestPort)
		}
	case netshim.ModeFlat:
		// Flat mode takes its address from the container runtime rather
		// than from -ip, so passing one is a sign the caller expects
		// netshim to assign it. Reject it rather than silently ignoring
		// it and handing them a guest on a different address entirely.
		if s.IP != "" {
			return fmt.Errorf("ip must not be set in %q net mode: the container runtime assigns the address the guest takes over (pass -ip \"\" when switching an existing VM over)", netshim.ModeFlat)
		}
		if ctlTap := "ctl-" + s.Name; len(ctlTap) > 15 {
			return fmt.Errorf("name %q too long: control tap device name %q would exceed 15 characters", s.Name, ctlTap)
		}
		if s.ControlCIDR != "" {
			if _, _, err := net.ParseCIDR(s.ControlCIDR); err != nil {
				return fmt.Errorf("invalid control CIDR %q: %w", s.ControlCIDR, err)
			}
		}
	default:
		return fmt.Errorf("net mode must be %q or %q, got %q", netshim.ModeNAT, netshim.ModeFlat, s.NetMode)
	}
	if s.CPUs < 1 {
		return fmt.Errorf("cpus must be at least 1, got %d", s.CPUs)
	}
	if s.MemoryMB < 128 {
		return fmt.Errorf("memory-mb must be at least 128, got %d", s.MemoryMB)
	}
	if s.Kernel != "" && s.Firmware != "" {
		return fmt.Errorf("kernel and firmware are mutually exclusive")
	}
	if _, err := time.ParseDuration(s.ShutdownTimeout); err != nil {
		return fmt.Errorf("invalid shutdown timeout %q: %w", s.ShutdownTimeout, err)
	}
	var gateway, netmask string
	if s.NetMode == netshim.ModeNAT {
		var err error
		gateway, netmask, err = gatewayAndNetmask(s.BridgeCIDR)
		if err != nil {
			return fmt.Errorf("invalid bridge CIDR %q: %w", s.BridgeCIDR, err)
		}
	}
	s.Backend = s.BackendOrDefault()
	if s.Backend != BackendStaticPod && s.Backend != BackendDocker {
		return fmt.Errorf("backend must be %q or %q, got %q", BackendStaticPod, BackendDocker, s.Backend)
	}
	s.CmdlineAuto = false
	if s.Firmware == "" && s.Cmdline == "" {
		root := "rw"
		if s.DiskReadOnly {
			root = "ro"
		}
		// Flat mode leaves the ip= parameter off: the address is only
		// knowable once the sandbox exists, so the VM container appends
		// it at boot from what it reads off the namespace itself (see
		// cmd/kontur's applyFlatNet).
		if s.NetMode == netshim.ModeFlat {
			s.Cmdline = fmt.Sprintf("console=ttyS0 root=/dev/vda %s", root)
		} else {
			s.Cmdline = fmt.Sprintf("console=ttyS0 root=/dev/vda %s ip=%s::%s:%s::eth0:off", root, s.IP, gateway, netmask)
		}
		s.CmdlineAuto = true
	}
	return nil
}

// NetModeOrDefault returns s.NetMode, treating an empty value (a spec
// saved before this field existed) as netshim.ModeNAT.
func (s VMSpec) NetModeOrDefault() string {
	if s.NetMode == "" {
		return netshim.ModeNAT
	}
	return s.NetMode
}

// IsFlat reports whether this VM is attached in flat mode.
func (s VMSpec) IsFlat() bool {
	return s.NetModeOrDefault() == netshim.ModeFlat
}

// ExecAddr is the address "kontur exec" dials to reach this VM's guest
// sshd, as seen from inside the shared network namespace.
//
// In NAT mode that is the guest's own address on the private bridge. In
// flat mode the guest holds the *namespace's* address, so dialing it from
// in here would reach the local stack instead -- the control link's
// address is the only way back in, and without a control link there is
// no path at all.
func (s VMSpec) ExecAddr() string {
	if s.NetModeOrDefault() == netshim.ModeNAT {
		return net.JoinHostPort(s.IP, "22")
	}
	if s.ControlCIDR == "" {
		return ""
	}
	addr, _, err := net.ParseCIDR(s.ControlCIDR)
	if err != nil {
		return ""
	}
	return net.JoinHostPort(netshim.ControlGuestIP(addr).String(), "22")
}

// WritableDiskDir is the host directory holding this VM's own private,
// writable qcow2 overlay of its disk image (see PrepareWritableDisk), one
// subdirectory per VM name so several VMs' overlays never collide. Only
// meaningful when DiskReadOnly is false.
func (s VMSpec) WritableDiskDir() string {
	return filepath.Join(s.DiskHostPath, s.Name)
}

// ResolvedDiskImage is this VM's disk path as seen inside its container:
// DiskImage unchanged when DiskReadOnly is true (read directly from the
// shared, read-only ImagesMountPath mount), or the path of this VM's own
// writable qcow2 overlay under DiskMountPath otherwise.
func (s VMSpec) ResolvedDiskImage() string {
	if s.DiskReadOnly {
		return s.DiskImage
	}
	return filepath.Join(DiskMountPath, writableDiskFileName)
}

// gatewayAndNetmask returns the address and dotted-decimal netmask encoded
// by a CIDR string such as "169.254.100.1/24" -- the address netshim itself
// binds to the bridge, which every VM guest uses as its default gateway.
func gatewayAndNetmask(cidr string) (gateway, netmask string, err error) {
	addr, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", err
	}
	mask := net.IP(ipnet.Mask).To4()
	if mask == nil {
		return "", "", fmt.Errorf("%s is not an IPv4 CIDR", cidr)
	}
	return addr.String(), mask.String(), nil
}

// specPath returns where a VM's spec JSON lives within stateDir.
func specPath(stateDir, name string) string {
	return stateDir + "/" + name + ".json"
}

// Load reads a previously saved VMSpec back from the state directory.
func Load(stateDir, name string) (VMSpec, error) {
	data, err := os.ReadFile(specPath(stateDir, name))
	if err != nil {
		return VMSpec{}, err
	}
	var s VMSpec
	if err := json.Unmarshal(data, &s); err != nil {
		return VMSpec{}, fmt.Errorf("parsing saved spec for %q: %w", name, err)
	}
	return s, nil
}

// Save persists spec to the state directory, creating it if needed.
func Save(stateDir string, s VMSpec) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(specPath(stateDir, s.Name), data, 0o644)
}

// Delete removes a VM's saved spec from the state directory. It is not an
// error if no spec is saved for name.
func Delete(stateDir, name string) error {
	err := os.Remove(specPath(stateDir, name))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// List returns the names of every VM with a saved spec in stateDir, sorted
// alphabetically. It returns an empty slice (not an error) if stateDir
// doesn't exist yet.
func List(stateDir string) ([]VMSpec, error) {
	entries, err := os.ReadDir(stateDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var specs []VMSpec
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 6 || name[len(name)-5:] != ".json" {
			continue
		}
		s, err := Load(stateDir, name[:len(name)-5])
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", name, err)
		}
		specs = append(specs, s)
	}
	sortByName(specs)
	return specs, nil
}
