package dockervm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	if len(got) != 3 {
		t.Fatalf("Create() issued %d docker calls, want 3:\n%v", len(got), got)
	}

	netnsCall, netshimCall, vmCall := got[0], got[1], got[2]

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

	vmCall := calls()[2]
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

func TestCreate_WritableDiskMountsDiskHostPathReadWrite(t *testing.T) {
	d, calls := testDocker(t)
	spec := testSpec()
	spec.DiskReadOnly = false
	spec.DiskHostPath = "/var/lib/kontur/vm-disks"

	if err := Create(context.Background(), d, spec, &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	vmCall := calls()[2]
	if !containsArg(vmCall, "-v") || !containsArg(vmCall, "/var/lib/kontur/vm-disks/web:/disk") {
		t.Errorf("VM call = %v, want a read-write /disk mount of the writable disk dir", vmCall)
	}
	if !containsArg(vmCall, "CHV_DISK_IMAGE=/disk/disk.qcow2") {
		t.Errorf("VM call = %v, want CHV_DISK_IMAGE pointing at the writable overlay", vmCall)
	}
}

func TestCreate_ReadOnlyDiskHasNoDiskMount(t *testing.T) {
	d, calls := testDocker(t)

	if err := Create(context.Background(), d, testSpec(), &strings.Builder{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	vmCall := calls()[2]
	for _, a := range vmCall {
		if strings.HasSuffix(a, ":/disk") {
			t.Errorf("VM call = %v, unexpectedly contains a /disk mount for a read-only disk", vmCall)
		}
	}
	if !containsArg(vmCall, "CHV_DISK_IMAGE=/images/disk.img") {
		t.Errorf("VM call = %v, want CHV_DISK_IMAGE unchanged for a read-only disk", vmCall)
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
	if len(got) != 3 {
		t.Fatalf("Create() issued %d docker calls, want 3 (holder, failing netshim, cleanup rm):\n%v", len(got), got)
	}
	cleanup := got[2]
	if cleanup[0] != "rm" || !containsArg(cleanup, "kontur-vm-web-netns") {
		t.Errorf("cleanup call = %v, want `docker rm -f` of the netns holder", cleanup)
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
