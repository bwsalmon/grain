// Package staticpod defines a VM's configuration (VMSpec) and its
// persistence as JSON under konturctl's state directory -- shared by both
// of konturctl's backends (see the Backend* constants below) -- plus
// BackendStaticPod's own half: generating the static pod manifests that
// the standalone kubelet set up under deploy/static-kubelet/ watches (see
// its README). BackendDocker's equivalent (running the same containers
// directly against a local docker daemon instead) lives in
// internal/dockervm. Each VM gets its own pod -- one netshim init
// container plus one VM container, as documented in the top-level README
// -- which is what lets the guest take over the pod's own address, and
// gives a simple, independent create/update/delete-by-name lifecycle.
package staticpod

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"time"

	"github.com/bwsalmon/kontur/internal/config"

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
)

// VMSpec holds every parameter needed to run one VM, under either
// backend. It is also what gets persisted to the state directory (as
// JSON) so a later "update" can start from the previous values instead of
// requiring every flag to be repeated.
type VMSpec struct {
	Name string `json:"name"`

	DiskImage    string `json:"diskImage"`
	DiskReadOnly bool   `json:"diskReadOnly"`

	// DiskMode is how the VM attaches its disk: config.DiskModeOverlay,
	// DiskModePersistent or DiskModeReadOnly, passed straight through as
	// CHV_DISK_MODE. Empty means "derive it from DiskReadOnly", which is
	// what a spec saved before this existed does.
	//
	// The overlay it names is made inside the VM's own container now, not
	// here: that is why a writable disk no longer needs a host directory
	// of its own, and why nothing on this side has to know that a qcow2
	// is involved at all.
	DiskMode string `json:"diskMode,omitempty"`

	// DiskSizeMB is how large a disk the guest is given, in MiB, passed
	// straight through as CHV_DISK_SIZE_MB: the VM's container sizes its
	// overlay to it before boot, creating it that large or growing one an
	// earlier boot left behind. Zero means the disk image's own size,
	// which is what an overlay has always been given.
	//
	// Only meaningful in config.DiskModeOverlay -- the overlay is the
	// only disk kontur creates, and the image underneath it is shared
	// with every other VM using it, so nothing here ever resizes that.
	DiskSizeMB int `json:"diskSizeMB,omitempty"`

	Kernel    string `json:"kernel,omitempty"`
	Initramfs string `json:"initramfs,omitempty"`
	Firmware  string `json:"firmware,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`

	// CmdlineAuto records whether Cmdline was derived automatically (from
	// DiskMode) rather than given explicitly, so a later "konturctl vm
	// update" that changes one of those inputs without also passing
	// --cmdline knows to recompute it instead of keeping the now stale
	// value.
	CmdlineAuto bool `json:"cmdlineAuto,omitempty"`

	CPUs     int `json:"cpus"`
	MemoryMB int `json:"memoryMB"`

	// CPUsMax and MemoryMaxMB are the ceilings CPUs and MemoryMB can be
	// grown to later with "kontur resize" (CHV_CPUS_MAX and
	// CHV_MEMORY_MAX_MB -- see the top-level README's "CPU hotplug" and
	// "Memory hotplug"). cloud-hypervisor sizes both hotplug windows once,
	// at boot, so a VM created without them can never be resized upward
	// at all: the vCPU half is refused outright ("Requested vCPUs exceed
	// maximum") and the memory half silently does nothing.
	//
	// Zero means "no headroom": omitted from the VM container's
	// environment entirely, leaving kontur run's own defaults (CHV_CPUS
	// for the vCPU ceiling, and CHV_MEMORY_MB or 2048 -- whichever is
	// larger -- for memory), which is what every spec saved before these
	// existed gets.
	CPUsMax     int `json:"cpusMax,omitempty"`
	MemoryMaxMB int `json:"memoryMaxMB,omitempty"`

	ShutdownTimeout string `json:"shutdownTimeout"`

	// GuestUser is an account inside the guest, besides root, that
	// "kontur exec" may log in as -- KONTUR_EXEC_USER on the VM
	// container.
	//
	// It is one setting rather than two because the VM container reads it
	// twice, for the two halves of the same fact: "kontur run" puts it on
	// the guest's kernel command line so the guest authorizes this boot's
	// generated key for that account (see internal/guestkey), and "kontur
	// exec" reads it to decide who to log in as. Naming the account in
	// only one of those places gives either a key authorized for someone
	// nobody logs in as, or a login as someone with no key.
	//
	// Empty means root, which is always authorized.
	GuestUser string `json:"guestUser,omitempty"`

	// Bridge holds the host end of the control link (see ControlCIDR);
	// nothing on the guest's own data path goes through it. ExternalIface
	// is the sandbox interface whose address, MAC and MTU the guest takes
	// over.
	Bridge        string `json:"bridge"`
	ExternalIface string `json:"externalIface"`

	// ControlCIDR is the address netshim holds on the control link, the
	// private second NIC that keeps "kontur exec" and the memory agent
	// able to reach a guest that now answers to the namespace's own
	// address. Set to the empty string to omit the control link
	// entirely.
	ControlCIDR string `json:"controlCIDR,omitempty"`

	// DNS is the nameserver(s) the guest resolves through, comma
	// separated, at most two of them (netshim.ParseDNS). They reach the
	// guest on its ip= boot parameter's own dns0/dns1 fields, where
	// kontur-configure-dns writes them into /etc/resolv.conf. This side
	// only carries the value: the parameter itself is assembled inside
	// the VM container, from the identity the container runtime assigned
	// (see internal/netshim.GuestConfig), so this reaches it as
	// NETSHIM_DNS.
	//
	// netshim.DefaultDNS is what "vm create" fills in when the flag is
	// left alone, and it is a public resolver on purpose: neither the
	// host's own resolver nor docker's embedded one is reachable from
	// inside a guest, so an image that inherits either resolves nothing.
	// A deployment whose network has its own resolver names it here; the
	// empty string leaves the guest with whatever /etc/resolv.conf its
	// image ships.
	DNS string `json:"dns,omitempty"`

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
// choice.
//
// DiskMode is config.DiskModeOverlay, which is what "kontur run" itself
// defaults to and the only one of the three modes a stock guest actually
// finishes booting from: a read-only root leaves systemd unable to write
// the state it needs and the boot never completes, so defaulting to it
// gave a VM nobody could use. The overlay is a thin qcow2 created inside
// the VM's own container (see config.PrepareOverlay), so this costs
// neither a host directory nor a write to the shared image -- the reason
// this used to default to read-only (that both backends mount
// -images-hostpath read-only, so a writable disk was impossible) stopped
// applying when the overlay moved into the container.
func Defaults() VMSpec {
	return VMSpec{
		DiskMode:                      config.DiskModeOverlay,
		CPUs:                          2,
		MemoryMB:                      2048,
		ShutdownTimeout:               "20s",
		ControlCIDR:                   "169.254.100.1/24",
		DNS:                           netshim.DefaultDNS,
		Bridge:                        "kontur0",
		ExternalIface:                 "eth0",
		ImagesHostPath:                "/var/lib/vm-images",
		DiskHostPath:                  "/var/lib/kontur/vm-disks",
		KonturImage:                   StaticPodImage,
		TerminationGracePeriodSeconds: 40,
		StaticPodPath:                 "/etc/kubernetes/manifests",
		Backend:                       BackendStaticPod,
	}
}

const (
	// StaticPodImage is the default KonturImage under BackendStaticPod:
	// the node-local registry "konturctl setup" installs and
	// deploy/static-kubelet/build-and-push.sh pushes to. Containerd pulls
	// by reference and cannot see an image that only exists in a local
	// docker daemon, so a registry reference is the only kind that works
	// there.
	StaticPodImage = "localhost:5000/kontur:latest"

	// DockerImage is the default KonturImage under BackendDocker: the tag
	// the top-level README's own "docker build -t kontur:latest ." makes,
	// which is an image the daemon already has and never tries to pull.
	// StaticPodImage names a registry that only exists once "konturctl
	// setup" has run, so using it here made every docker-backend caller
	// pass -kontur-image by hand.
	DockerImage = "kontur:latest"
)

// DefaultsForBackend is Defaults with the defaults that differ per
// backend applied -- currently KonturImage, whose sensible value depends
// entirely on who is going to resolve it (see StaticPodImage and
// DockerImage).
//
// An unrecognized backend gets the static-pod defaults, the same way
// BackendOrDefault treats an unset one; Validate is what rejects it,
// after the flags have been parsed and can say so properly.
func DefaultsForBackend(backend string) VMSpec {
	d := Defaults()
	if backend == BackendDocker {
		d.Backend = BackendDocker
		d.KonturImage = DockerImage
	}
	return d
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
// in a default Cmdline unless Firmware is set -- Cmdline only applies to
// direct kernel boot, which is still what happens even with Kernel left
// empty (kontur run's own CHV_KERNEL default then applies, see
// internal/config's defaultKernel).
func (s *VMSpec) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	// netshim names the VM's tap device "tap-<name>" and Linux interface
	// names are capped at 15 characters (IFNAMSIZ-1); catch an
	// over-length name here, at "vm create"/"vm update" time, rather than
	// as a netshim init container crash loop once the pod's already been
	// submitted.
	if tapName := "tap-" + s.Name; len(tapName) > 15 {
		return fmt.Errorf("name %q too long: tap device name %q would exceed 15 characters", s.Name, tapName)
	}
	// Parsed here even though nothing on this side renders it: the value
	// is assembled into an ip= parameter inside the VM container, and an
	// address rejected there would fail a VM that "vm create" had
	// already reported as started.
	if _, err := netshim.ParseDNS(s.DNS); err != nil {
		return fmt.Errorf("invalid dns %q: %w", s.DNS, err)
	}
	// An empty DiskImage means the guest disk baked into the kontur
	// image itself (internal/config's defaultDiskImage), rather than one
	// supplied from outside under ImagesMountPath. That is what a VM
	// booted from a *customized* kontur image is: its guest travels
	// inside the image, so there is no host path to name, nothing to
	// mount, and no shared backing file to overlay -- the container's own
	// writable layer is the per-VM copy. Every check below is about a
	// supplied disk and so only applies when there is one.
	if s.DiskImage == "" && s.BackendOrDefault() == BackendStaticPod {
		// The docker backend can simply omit CHV_DISK_IMAGE and let the
		// image's own default apply; the pod manifest always emits the
		// variable, and an empty value there is not the same as an
		// unset one. Rejecting it here beats submitting a manifest that
		// fails inside the kubelet.
		return fmt.Errorf("a disk image is required on the %q backend: booting the kontur image's own baked guest is supported on %q", BackendStaticPod, BackendDocker)
	}
	// A writable disk used to have to live under ImagesMountPath, because
	// the qcow2 overlay backing it was created out here and needed a host
	// path to point at. The overlay is made inside the VM's container now
	// (see config.PrepareOverlay), so any path the container can read
	// works -- including the guest baked into the kontur image itself,
	// which has no host path at all.
	s.DiskMode = s.DiskModeOrDerived()
	switch s.DiskMode {
	case config.DiskModeOverlay, config.DiskModePersistent, config.DiskModeReadOnly:
	default:
		return fmt.Errorf("disk mode must be %q, %q or %q, got %q",
			config.DiskModeOverlay, config.DiskModePersistent, config.DiskModeReadOnly, s.DiskMode)
	}
	// Caught here rather than left to the VM container, which rejects the
	// same combination on its own (see config.FromEnv): "vm create" can
	// say so before a pod is submitted or a container started, the same
	// way every other check in here does.
	if s.DiskSizeMB < 0 {
		return fmt.Errorf("disk-size-mb must not be negative, got %d", s.DiskSizeMB)
	}
	if s.DiskSizeMB > 0 && s.DiskMode != config.DiskModeOverlay {
		return fmt.Errorf("disk-size-mb needs -disk-mode=%s: only the VM's own overlay is resized, never the shared disk image (got %q)",
			config.DiskModeOverlay, s.DiskMode)
	}
	// The control link's tap is named from the VM's name too, and a VM
	// whose control tap cannot be named is a VM with no exec path in.
	if ctlTap := "ctl-" + s.Name; len(ctlTap) > 15 {
		return fmt.Errorf("name %q too long: control tap device name %q would exceed 15 characters", s.Name, ctlTap)
	}
	if s.ControlCIDR != "" {
		if _, _, err := net.ParseCIDR(s.ControlCIDR); err != nil {
			return fmt.Errorf("invalid control CIDR %q: %w", s.ControlCIDR, err)
		}
	}
	if s.CPUs < 1 {
		return fmt.Errorf("cpus must be at least 1, got %d", s.CPUs)
	}
	if s.MemoryMB < 128 {
		return fmt.Errorf("memory-mb must be at least 128, got %d", s.MemoryMB)
	}
	// Caught here rather than left to the VM container, where it would
	// surface as a container that crash-loops on kontur run's own
	// validation after the VM has already been submitted.
	if s.CPUsMax != 0 && s.CPUsMax < s.CPUs {
		return fmt.Errorf("cpus-max (%d) must be at least cpus (%d)", s.CPUsMax, s.CPUs)
	}
	if s.MemoryMaxMB != 0 && s.MemoryMaxMB < s.MemoryMB {
		return fmt.Errorf("memory-max-mb (%d) must be at least memory-mb (%d)", s.MemoryMaxMB, s.MemoryMB)
	}
	if s.Kernel != "" && s.Firmware != "" {
		return fmt.Errorf("kernel and firmware are mutually exclusive")
	}
	if _, err := time.ParseDuration(s.ShutdownTimeout); err != nil {
		return fmt.Errorf("invalid shutdown timeout %q: %w", s.ShutdownTimeout, err)
	}
	s.Backend = s.BackendOrDefault()
	if s.Backend != BackendStaticPod && s.Backend != BackendDocker {
		return fmt.Errorf("backend must be %q or %q, got %q", BackendStaticPod, BackendDocker, s.Backend)
	}
	s.CmdlineAuto = false
	if s.Firmware == "" && s.Cmdline == "" {
		root := "rw"
		if s.DiskMode == config.DiskModeReadOnly {
			root = "ro"
		}
		// No ip= parameter: the address is only knowable once the
		// sandbox exists, so the VM container appends it at boot from
		// what it reads off the namespace itself (see cmd/kontur's
		// applyNetshimNet).
		s.Cmdline = fmt.Sprintf("console=ttyS0 root=/dev/vda %s", root)
		s.CmdlineAuto = true
	}
	return nil
}

// DiskModeOrDerived returns s.DiskMode, deriving it from DiskReadOnly
// when unset -- a spec saved before DiskMode existed, or a caller still
// passing only -disk-readonly.
//
// -disk-readonly=false has always meant "give this VM a private writable
// disk", implemented as a per-VM overlay; it maps to DiskModeOverlay, so
// that intent survives the move of the overlay into the container. (Note
// this is not how the same-named CHV_DISK_READONLY maps inside the
// container, where false meant writing through to the image -- see
// config.diskModeFromEnv.)
func (s VMSpec) DiskModeOrDerived() string {
	if s.DiskMode != "" {
		return s.DiskMode
	}
	if s.DiskReadOnly {
		return config.DiskModeReadOnly
	}
	return config.DiskModeOverlay
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
