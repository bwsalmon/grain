package dockervm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/kontur/internal/netshim"
	"github.com/bwsalmon/kontur/internal/staticpod"
)

// fakedockerPath is built once per test run from testdata/fakedocker,
// which stands in for the real docker CLI: it records every invocation to
// a log file and can be told to fail specific calls, so dockervm's command
// construction and error handling can be tested without a real docker
// daemon.
var fakedockerPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakedocker-build")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	fakedockerPath = filepath.Join(dir, "fakedocker")
	cmd := exec.Command("go", "build", "-o", fakedockerPath, "./testdata/fakedocker")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("building fakedocker test helper: " + err.Error())
	}

	os.Exit(m.Run())
}

// testDocker returns a Docker pointed at fakedocker, plus a function that
// reads back the calls it recorded so far as one []string per invocation
// (its args, in order).
func testDocker(t *testing.T) (*Docker, func() [][]string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKEDOCKER_LOG", logPath)

	return &Docker{BinaryPath: fakedockerPath}, func() [][]string {
		data, err := os.ReadFile(logPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("reading fakedocker log: %v", err)
		}
		var calls [][]string
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			calls = append(calls, strings.Split(line, "\x1f"))
		}
		return calls
	}
}

func testSpec() staticpod.VMSpec {
	s := staticpod.Defaults()
	s.Name = "web"
	s.DiskImage = "/images/disk.img"
	s.Kernel = "/images/vmlinux"
	s.IP = "169.254.100.2"
	s.Port = 30080
	s.Backend = staticpod.BackendDocker
	return s
}

// containsArg reports whether call (a docker invocation's argv) contains
// arg anywhere.
func containsArg(call []string, arg string) bool {
	for _, a := range call {
		if a == arg {
			return true
		}
	}
	return false
}

func TestCreate_RunsNetnsHolderNetshimThenVM(t *testing.T) {
	d, calls := testDocker(t)
	var stdout strings.Builder

	if err := Create(context.Background(), d, testSpec(), &stdout); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := calls()
	if len(got) != 5 {
		t.Fatalf("Create() issued %d docker calls, want 5:\n%v", len(got), got)
	}

	staleVMRm, staleNetnsRm, netnsCall, netshimCall, vmCall := got[0], got[1], got[2], got[3], got[4]

	if staleVMRm[0] != "rm" || !containsArg(staleVMRm, "kontur-vm-web") {
		t.Errorf("first call = %v, want `docker rm -f kontur-vm-web` clearing any stale container", staleVMRm)
	}
	if staleNetnsRm[0] != "rm" || !containsArg(staleNetnsRm, "kontur-vm-web-netns") {
		t.Errorf("second call = %v, want `docker rm -f kontur-vm-web-netns` clearing any stale container", staleNetnsRm)
	}

	if netnsCall[0] != "run" || !containsArg(netnsCall, "kontur-vm-web-netns") || !containsArg(netnsCall, "sleep") {
		t.Errorf("first call = %v, want a `docker run` starting the kontur-vm-web-netns holder", netnsCall)
	}

	if netshimCall[0] != "run" || !containsArg(netshimCall, "container:kontur-vm-web-netns") || !containsArg(netshimCall, "netshim") {
		t.Errorf("second call = %v, want netshim attached to the netns holder", netshimCall)
	}
	if !containsArg(netshimCall, "NETSHIM_VMS=web:169.254.100.2:30080") {
		t.Errorf("netshim call = %v, missing expected NETSHIM_VMS", netshimCall)
	}

	if vmCall[0] != "run" || !containsArg(vmCall, "kontur-vm-web") || !containsArg(vmCall, "container:kontur-vm-web-netns") {
		t.Errorf("third call = %v, want the VM container attached to the netns holder", vmCall)
	}
	if !containsArg(vmCall, "CHV_NET=tap=tap-web") {
		t.Errorf("VM call = %v, missing expected CHV_NET", vmCall)
	}
	if !containsArg(vmCall, "CHV_KERNEL=/images/vmlinux") {
		t.Errorf("VM call = %v, missing expected CHV_KERNEL", vmCall)
	}
	if !containsArg(vmCall, "KONTUR_EXEC_ADDR=169.254.100.2:22") {
		t.Errorf("VM call = %v, missing expected KONTUR_EXEC_ADDR", vmCall)
	}
	if !containsArg(vmCall, "--privileged") || !containsArg(vmCall, "/dev/kvm") {
		t.Errorf("VM call = %v, want --privileged and /dev/kvm", vmCall)
	}
}

func TestCreate_OmitsUnsetOptionalEnv(t *testing.T) {
	d, calls := testDocker(t)
	spec := testSpec()
	spec.Kernel = ""
	spec.Firmware = "/images/CLOUDHV.fd"

	if err := Create(context.Background(), d, spec, &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	vmCall := calls()[4]
	for _, unwanted := range []string{"CHV_KERNEL", "CHV_INITRAMFS", "CHV_CMDLINE"} {
		for _, a := range vmCall {
			if strings.HasPrefix(a, unwanted+"=") {
				t.Errorf("VM call = %v, unexpectedly contains %s", vmCall, unwanted)
			}
		}
	}
	if !containsArg(vmCall, "CHV_FIRMWARE=/images/CLOUDHV.fd") {
		t.Errorf("VM call = %v, missing expected CHV_FIRMWARE", vmCall)
	}
}

func TestCreate_WritableDiskMountsNothingExtra(t *testing.T) {
	// A writable disk used to mean a second, read-write bind mount of the
	// host directory holding this VM's qcow2. That overlay is made inside
	// the VM's own container now, so the only mount left is the shared
	// read-only images directory, and CHV_DISK_IMAGE names the source
	// image rather than an overlay path.
	d, calls := testDocker(t)
	spec := testSpec()
	spec.DiskReadOnly = false

	if err := Create(context.Background(), d, spec, &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	vmCall := calls()[4]
	for _, a := range vmCall {
		if strings.HasSuffix(a, ":/disk") {
			t.Errorf("VM call = %v, still bind-mounts a host directory for the overlay", vmCall)
		}
	}
	if !containsArg(vmCall, "CHV_DISK_IMAGE=/images/disk.img") {
		t.Errorf("VM call = %v, want CHV_DISK_IMAGE naming the source image", vmCall)
	}
	if !containsArg(vmCall, "CHV_DISK_MODE=overlay") {
		t.Errorf("VM call = %v, want -disk-readonly=false to ask for an overlay", vmCall)
	}
	if !containsArg(vmCall, "/var/lib/vm-images:/images:ro") {
		t.Errorf("VM call = %v, want the images mount still read-only", vmCall)
	}
}

func TestCreate_ReadOnlyDiskHasNoDiskMount(t *testing.T) {
	d, calls := testDocker(t)

	if err := Create(context.Background(), d, testSpec(), &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	vmCall := calls()[4]
	for _, a := range vmCall {
		if strings.HasSuffix(a, ":/disk") {
			t.Errorf("VM call = %v, unexpectedly contains a /disk mount for a read-only disk", vmCall)
		}
	}
	if !containsArg(vmCall, "CHV_DISK_IMAGE=/images/disk.img") {
		t.Errorf("VM call = %v, want CHV_DISK_IMAGE unchanged for a read-only disk", vmCall)
	}
}

func TestCreate_NoDiskBootsTheImagesOwnGuest(t *testing.T) {
	// A customized kontur image carries its guest inside it, so there is
	// nothing on the host to mount and no CHV_DISK_IMAGE to set -- "kontur
	// run"'s own default (internal/config's defaultDiskImage) is the
	// whole point. Mounting -images-hostpath anyway would shadow nothing
	// but would make a host directory a requirement for a VM that needs
	// none.
	d, calls := testDocker(t)
	spec := testSpec()
	spec.DiskImage = ""
	spec.DiskReadOnly = false

	if err := Create(context.Background(), d, spec, &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	vmCall := calls()[4]
	for _, a := range vmCall {
		if strings.HasPrefix(a, "CHV_DISK_IMAGE=") {
			t.Errorf("VM call = %v, want no CHV_DISK_IMAGE so the image's own default applies", vmCall)
		}
		if strings.HasSuffix(a, ":/images:ro") || strings.HasSuffix(a, ":/disk") {
			t.Errorf("VM call = %v, want no disk mounts at all", vmCall)
		}
	}
	if !containsArg(vmCall, "CHV_DISK_MODE=overlay") {
		t.Errorf("VM call = %v, want the disk mode still passed through", vmCall)
	}
}

func TestCreate_NetshimFailureRemovesNetnsHolder(t *testing.T) {
	d, calls := testDocker(t)
	t.Setenv("FAKEDOCKER_FAIL_CONTAINS", "netshim")

	err := Create(context.Background(), d, testSpec(), &strings.Builder{})
	if err == nil {
		t.Fatalf("Create() error = nil, want an error from the failing netshim call")
	}

	got := calls()
	if len(got) != 5 {
		t.Fatalf("Create() issued %d docker calls, want 5 (2 stale-cleanup rm, holder, failing netshim, cleanup rm):\n%v", len(got), got)
	}
	cleanup := got[4]
	if cleanup[0] != "rm" || !containsArg(cleanup, "kontur-vm-web-netns") {
		t.Errorf("cleanup call = %v, want `docker rm -f` of the netns holder", cleanup)
	}
}

func TestCreate_RemovesStaleContainersBeforeCreating(t *testing.T) {
	d, calls := testDocker(t)

	if err := Create(context.Background(), d, testSpec(), &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := calls()
	if len(got) < 2 {
		t.Fatalf("Create() issued %d docker calls, want at least 2 leading stale-cleanup calls:\n%v", len(got), got)
	}
	if got[0][0] != "rm" || !containsArg(got[0], "kontur-vm-web") {
		t.Errorf("first call = %v, want `docker rm -f kontur-vm-web` (clearing a stale VM container left by an interrupted prior Create)", got[0])
	}
	if got[1][0] != "rm" || !containsArg(got[1], "kontur-vm-web-netns") {
		t.Errorf("second call = %v, want `docker rm -f kontur-vm-web-netns` (clearing a stale netns holder left by an interrupted prior Create)", got[1])
	}
}

func TestDelete_StopsThenRemovesBothContainers(t *testing.T) {
	d, calls := testDocker(t)

	if err := Delete(context.Background(), d, "web", 40, &strings.Builder{}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	got := calls()
	if len(got) != 3 {
		t.Fatalf("Delete() issued %d docker calls, want 3 (stop, rm VM, rm netns):\n%v", len(got), got)
	}
	if got[0][0] != "stop" || !containsArg(got[0], "-t") || !containsArg(got[0], "40") || !containsArg(got[0], "kontur-vm-web") {
		t.Errorf("first call = %v, want `docker stop -t 40 kontur-vm-web`", got[0])
	}
	if got[1][0] != "rm" || !containsArg(got[1], "kontur-vm-web") {
		t.Errorf("second call = %v, want `docker rm -f kontur-vm-web`", got[1])
	}
	if got[2][0] != "rm" || !containsArg(got[2], "kontur-vm-web-netns") {
		t.Errorf("third call = %v, want `docker rm -f kontur-vm-web-netns`", got[2])
	}
}

func TestDelete_IdempotentWhenContainersAlreadyGone(t *testing.T) {
	d, _ := testDocker(t)
	t.Setenv("FAKEDOCKER_MISSING", "kontur-vm-web,kontur-vm-web-netns")

	if err := Delete(context.Background(), d, "web", 40, &strings.Builder{}); err != nil {
		t.Errorf("Delete() of already-gone containers error = %v, want nil (idempotent)", err)
	}
}

// flatSpec is testSpec in flat mode: no -ip (the container runtime picks
// the address the guest takes over), and a couple of caller-supplied
// docker options that only the namespace holder can carry.
func flatSpec() staticpod.VMSpec {
	s := testSpec()
	s.NetMode = netshim.ModeFlat
	s.IP = ""
	s.Port = 0
	s.DockerRunOpts = []string{"--network", "mynet", "-p", "8080:80"}
	return s
}

func TestCreate_FlatMode(t *testing.T) {
	d, calls := testDocker(t)
	var stdout strings.Builder

	if err := Create(context.Background(), d, flatSpec(), &stdout); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := calls()
	if len(got) != 5 {
		t.Fatalf("Create() issued %d docker calls, want 5:\n%v", len(got), got)
	}
	netnsCall, netshimCall, vmCall := got[2], got[3], got[4]

	// Port publishing and network membership belong to the container
	// that owns the namespace; nothing joining it later can add them.
	for _, want := range []string{"--network", "mynet", "-p", "8080:80"} {
		if !containsArg(netnsCall, want) {
			t.Errorf("netns holder call = %v, missing pass-through option %q", netnsCall, want)
		}
	}
	// They must land on the holder only -- repeating a -p on a container
	// joining an existing namespace is an error, not a no-op.
	if containsArg(vmCall, "8080:80") {
		t.Errorf("VM call = %v, should not repeat the holder's published ports", vmCall)
	}

	if !containsArg(netshimCall, "NETSHIM_MODE=flat") || !containsArg(netshimCall, "NETSHIM_VM=web") {
		t.Errorf("netshim call = %v, want flat-mode settings", netshimCall)
	}
	if !containsArg(netshimCall, "NETSHIM_CONTROL_CIDR=169.254.100.1/24") {
		t.Errorf("netshim call = %v, missing expected NETSHIM_CONTROL_CIDR", netshimCall)
	}

	// Flat mode does no routing and installs no NAT rules, so it never
	// writes the sysctl that forces the NAT path to run privileged.
	if containsArg(netshimCall, "--privileged") {
		t.Errorf("netshim call = %v, want capabilities rather than --privileged in flat mode", netshimCall)
	}
	for _, want := range []string{"NET_ADMIN", "/dev/net/tun"} {
		if !containsArg(netshimCall, want) {
			t.Errorf("netshim call = %v, missing %q", netshimCall, want)
		}
	}

	// The VM container derives its own --net from the namespace, so it
	// gets netshim's settings rather than a precomputed CHV_NET.
	if !containsArg(vmCall, "NETSHIM_MODE=flat") {
		t.Errorf("VM call = %v, want the flat-mode netshim settings", vmCall)
	}
	for _, unwanted := range []string{"CHV_NET=tap=tap-web", "CHV_NET="} {
		if containsArg(vmCall, unwanted) {
			t.Errorf("VM call = %v, should not set CHV_NET in flat mode", vmCall)
		}
	}

	// exec has to go via the control link: the guest now answers to the
	// namespace's own address, so dialing that from inside the namespace
	// would reach the local stack.
	if !containsArg(vmCall, "KONTUR_EXEC_ADDR=169.254.100.2:22") {
		t.Errorf("VM call = %v, want KONTUR_EXEC_ADDR on the control link", vmCall)
	}
}

// TestCreate_FlatModeNoControlLink confirms that disabling the control
// link drops the exec address entirely rather than emitting an empty or
// bogus one that would fail at dial time.
func TestCreate_FlatModeNoControlLink(t *testing.T) {
	d, calls := testDocker(t)
	var stdout strings.Builder

	spec := flatSpec()
	spec.ControlCIDR = ""
	if err := Create(context.Background(), d, spec, &stdout); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	vmCall := calls()[4]
	for _, a := range vmCall {
		if strings.HasPrefix(a, "KONTUR_EXEC_ADDR=") {
			t.Errorf("VM call = %v, want no KONTUR_EXEC_ADDR without a control link", vmCall)
		}
	}
}

// GuestUser has to reach the VM container, because both halves of it are
// read there: "kontur run" puts it on the guest's kernel command line so
// the generated key is authorized for that account, and "kontur exec" --
// which docker exec runs with the container's own environment -- logs in
// as it. See staticpod.VMSpec.GuestUser.
func TestCreate_GuestUser(t *testing.T) {
	d, calls := testDocker(t)
	spec := testSpec()
	spec.GuestUser = "debian"
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	if err := Create(context.Background(), d, spec, &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got := calls()
	vmCall := got[len(got)-1]
	if !containsArg(vmCall, "KONTUR_EXEC_USER=debian") {
		t.Errorf("VM call = %v, missing KONTUR_EXEC_USER", vmCall)
	}
}

// Unset means root, so the variable is left out rather than sent empty --
// guestexec would read "" as an account of that name rather than as
// "unset".
func TestCreate_NoGuestUser(t *testing.T) {
	d, calls := testDocker(t)
	if err := Create(context.Background(), d, testSpec(), &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got := calls()
	for _, arg := range got[len(got)-1] {
		if strings.HasPrefix(arg, "KONTUR_EXEC_USER=") {
			t.Errorf("VM call sets %q with no guest user configured", arg)
		}
	}
}

func TestCreate_DiskSizeMB(t *testing.T) {
	d, calls := testDocker(t)
	spec := testSpec()
	spec.DiskReadOnly = false
	spec.DiskSizeMB = 8192

	if err := Create(context.Background(), d, spec, &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	vmCall := calls()[4]
	if !containsArg(vmCall, "CHV_DISK_SIZE_MB=8192") {
		t.Errorf("VM call = %v, missing expected CHV_DISK_SIZE_MB", vmCall)
	}
}

func TestCreate_OmitsDiskSizeWhenUnset(t *testing.T) {
	d, calls := testDocker(t)

	if err := Create(context.Background(), d, testSpec(), &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	vmCall := calls()[4]
	for _, a := range vmCall {
		if strings.HasPrefix(a, "CHV_DISK_SIZE_MB=") {
			t.Errorf("VM call = %v, unexpectedly sets CHV_DISK_SIZE_MB for a VM that asked for no size", vmCall)
		}
	}
}
