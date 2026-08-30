package orchestrator_test

// TestKonturSandboxesToolsForAgainstARealDockerBackedVM is the one place
// in this repo that drives orchestrator.KonturSandboxes against the real
// `konturctl` and `docker` binaries and a real cloud-hypervisor VM under
// real KVM, instead of the hand-written shell-script doubles every other
// kontur test in this repo (kontur_sandboxes_test.go,
// pkg/kontur/kontur_test.go, cmd/grain/daemon_kontur_wiring_test.go,
// cmd/grain/daemon_true_e2e_test.go) uses. Those doubles are what let the
// rest of the suite run fast and everywhere, but they can only be as
// correct as whoever wrote them already knew to make them: they are
// exactly why v2/pkg/kontur's own Create/Delete execing a binary literally
// named "kontur" (rather than "konturctl", the actual operator-facing
// binary bwsalmon/kontur ships under a different name -- see that
// package's doc comment) went unnoticed until this test was written
// against the real thing and failed with "exec: \"konturctl\": executable
// file not found in $PATH" the moment the fake stopped being named to
// match the bug.
//
// It skips outright unless docker and /dev/kvm are both usable -- the
// same "skip rather than run" shape tests/test_vm_integration.py's own
// gate uses for v1's equivalent live suite (that file's own comment:
// "none of which a hosted runner has, so they skip rather than run").
// Neither is present on the hosted GitHub Actions runner
// .github/workflows/tests.yml's go-test job runs on, so this never runs
// there, but it runs for real on any host -- this repo's own dev sandboxes
// included -- that has both, which is the whole point: wherever kontur's
// prerequisites genuinely exist, this suite exercises it for real rather
// than only ever exercising a fake that agrees with whatever the last
// person to touch it assumed.
//
// What this does NOT prove: that a dispatched task's tool calls actually
// execute inside the guest over SSH. That needs a guest kernel with
// virtio-blk/virtio-pci support to actually mount the disk image
// bwsalmon/kontur's own Dockerfile bakes in and bring up sshd --
// packer/kontur/README.md's own guest image is not yet published anywhere
// this test could fetch it from, and the readily downloadable
// firecracker-ci PVH kernel bwsalmon/kontur's own benchmarks/kernel/
// build.sh fetches panics on "Cannot open root device vda" against that
// disk image: it has no virtio-blk driver, since Firecracker itself uses
// virtio-mmio, not cloud-hypervisor's virtio-pci (confirmed by hand
// against this exact image and kernel before writing this test). So this
// test boots the VM container with that same kernel -- enough to prove
// every other real moving part (the konturctl/docker CLI invocations,
// netshim's real bridge/tap/iptables setup inside a real network
// namespace, and cloud-hypervisor actually launching under real KVM) --
// and stops at ToolsFor succeeding (VM created, real docker-assigned IP
// resolved), rather than at a tool call actually running against it.
// Guest-boot-and-SSH coverage is left to whichever future task publishes
// a working guest image (packer/kontur/README.md's own "Building and
// publishing" section, bwsalmon/agents#267's "decide, don't just wire").

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/kontur"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// konturDockerRealTestPrereqs are the binaries and devices this test
// needs on the host it runs on, beyond what every other test in this
// repo already assumes (go, a POSIX shell). Each check's own failure
// message is the skip reason, so a run against a host missing exactly one
// of these says which.
func konturDockerRealTestPrereqs(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("real kontur/docker VMs are only wired up for Linux hosts")
	}
	for _, bin := range []string{"docker", "ssh-keygen", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH: %v", bin, err)
		}
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
	// Stat, not Open: the VM container this test creates runs --privileged
	// as root and reaches /dev/kvm that way (--device /dev/kvm in
	// internal/dockervm/docker.go's own "docker run"), not as this test
	// process's own, ordinarily much less privileged, host user.
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm not present: %v", err)
	}
}

// buildKonturctl builds bwsalmon/kontur's own operator-facing CLI from the
// vendored copy this repo carries (third_party/kontur/VENDORED.md) into a
// fresh temp directory, and returns that directory so the caller can put
// it on PATH -- the exact binary name pkg/kontur.Create/Delete now exec
// (this test's entire reason to exist: proving that against the real
// thing).
func buildKonturctl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "konturctl")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/konturctl")
	cmd.Dir = "../../../third_party/kontur"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building konturctl from third_party/kontur: %v\n%s", err, out)
	}
	return dir
}

// buildKonturDockerImage generates a throwaway SSH keypair, builds
// bwsalmon/kontur's own OCI image (third_party/kontur/Dockerfile) with
// that keypair's public half baked in as the guest's only login
// credential, and returns the image tag and the private key's path.
// Rebuilding this image is the most expensive part of this test (Debian
// debootstrap plus fetching a pinned cloud-hypervisor release), but it is
// also the one honest way to exercise the exact image bwsalmon/agents#353
// asked grain's own docker backend to run, rather than assuming its
// Dockerfile still builds the way some past sandbox found it to.
func buildKonturDockerImage(t *testing.T) (image, sshKeyPath string) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", keyPath, "-q").CombinedOutput(); err != nil {
		t.Fatalf("generating a throwaway SSH keypair: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatalf("reading generated SSH public key: %v", err)
	}

	image = "grain-kontur-e2e-test:latest"
	cmd := exec.Command("docker", "build",
		"--build-arg", "GUEST_SSH_AUTHORIZED_KEY="+string(pub),
		"-t", image, ".")
	cmd.Dir = "../../../third_party/kontur"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the kontur OCI image from third_party/kontur/Dockerfile: %v\n%s", err, out)
	}
	return image, keyPath
}

// fetchTestKernel downloads the same PVH-entry kernel bwsalmon/kontur's
// own benchmarks/kernel/build.sh fetches, into a stable cache directory
// under os.TempDir() rather than t.TempDir() -- at ~38MB, redownloading it
// on every run of an already-expensive, opt-in test would only cost time
// with no benefit, since the kernel itself never changes across runs of
// this test.
func fetchTestKernel(t *testing.T) (hostDir string) {
	t.Helper()
	hostDir = filepath.Join(os.TempDir(), "grain-kontur-e2e-test-images")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatalf("creating kernel cache directory: %v", err)
	}
	kernelPath := filepath.Join(hostDir, "vmlinux")
	if _, err := os.Stat(kernelPath); err == nil {
		return hostDir
	}
	const kernelURL = "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-5.10.223"
	out, err := exec.Command("curl", "-fsSL", "-o", kernelPath, kernelURL).CombinedOutput()
	if err != nil {
		t.Skipf("fetching test kernel from %s: %v\n%s", kernelURL, err, out)
	}
	return hostDir
}

func TestKonturSandboxesToolsForAgainstARealDockerBackedVM(t *testing.T) {
	konturDockerRealTestPrereqs(t)

	konturctlDir := buildKonturctl(t)
	image, sshKeyPath := buildKonturDockerImage(t)
	imagesHostPath := fetchTestKernel(t)

	t.Setenv("PATH", konturctlDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateDir := t.TempDir()
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		// Short on purpose: netshim's docker backend names each VM's tap
		// device "tap-"+name, and Linux caps interface names at 15 bytes
		// (IFNAMSIZ-1) -- confirmed by hand while writing this test, which
		// first failed against a longer, more descriptive prefix with
		// exactly that error. See KonturConfig.NamePrefix's own doc
		// comment.
		NamePrefix: "kdrt-",
		Backend:    kontur.BackendDocker,
		StateDir:   stateDir,
		CreateArgs: []string{
			"-kontur-image", image,
			"-images-hostpath", imagesHostPath,
			"-disk", "/var/lib/kontur/guest/disk.img",
			"-kernel", "/images/vmlinux",
			"-ip", "169.254.100.2",
			"-port", "31080",
		},
		SSHUser:           "root",
		SSHKey:            sshKeyPath,
		Workspace:         "/root",
		ReadyTimeout:      2 * time.Minute,
		ReadyPollInterval: time.Second,
	})

	name := k.VMNameFor("1")
	t.Cleanup(func() {
		if err := kontur.Delete(context.Background(), stateDir, name); err != nil {
			t.Logf("cleaning up real kontur VM %q: %v", name, err)
		}
	})

	tools, err := k.ToolsFor(context.Background(), "1")
	if err != nil {
		t.Fatalf("ToolsFor against a real konturctl/docker/cloud-hypervisor VM: %v", err)
	}
	wantNames := map[string]bool{"run_command": true, "read_file": true, "write_file": true, "edit_file": true}
	if len(tools) != len(wantNames) {
		t.Fatalf("ToolsFor() returned %d tools, want %d", len(tools), len(wantNames))
	}
	for _, tool := range tools {
		if !wantNames[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
	}

	// Confirm konturctl actually persisted a port for the VM this test
	// asked for (30080, via -port above) -- kontur.Port is how
	// KonturSandboxes.resolveEndpoint above found it, but reading it back
	// independently here confirms it is real state konturctl wrote, not
	// an artifact of ToolsFor's own bookkeeping.
	port, err := kontur.Port(stateDir, name)
	if err != nil {
		t.Fatalf("kontur.Port after a real vm create: %v", err)
	}
	if port != 31080 {
		t.Errorf("kontur.Port() = %d, want 31080", port)
	}

	// Confirm the resolved address is real -- a genuine docker-assigned IP
	// on the network namespace netshim actually configured, resolved via
	// a real "docker inspect", not a fake standing in for one.
	ip, err := kontur.DockerPodIP(context.Background(), name)
	if err != nil {
		t.Fatalf("kontur.DockerPodIP after a real vm create: %v", err)
	}
	if net.ParseIP(ip) == nil {
		t.Errorf("kontur.DockerPodIP() = %q, want a valid IP address", ip)
	}

	// The VM container itself should still be running: cloud-hypervisor
	// supervises the guest rather than exiting when it panics (confirmed
	// by hand against this exact image/kernel before writing this test --
	// see this file's own doc comment on why the guest panics at all).
	// Checking this independently of ToolsFor's own success confirms that
	// success reflects a real, live container rather than stale state
	// left over from an earlier run.
	statusOut, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", "kontur-vm-"+name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect on the real VM container: %v\n%s", err, statusOut)
	}
	if status := string(bytes.TrimSpace(statusOut)); status != "running" {
		t.Errorf("real VM container status = %q, want %q", status, "running")
	}
}
