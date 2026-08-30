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
// It skips outright unless docker, /dev/kvm, and (for the guest image
// build below) passwordless sudo plus debootstrap/mke2fs are all usable
// -- the same "skip rather than run" shape tests/test_vm_integration.py's
// own gate uses for v1's equivalent live suite (that file's own comment:
// "none of which a hosted runner has, so they skip rather than run").
// None of these are present on the hosted GitHub Actions runner
// .github/workflows/tests.yml's go-test job runs on, so this never runs
// there, but it runs for real on any host -- this repo's own dev sandboxes
// included -- that has all of them, which is the whole point: wherever
// kontur's prerequisites genuinely exist, this suite exercises it for
// real rather than only ever exercising a fake that agrees with whatever
// the last person to touch it assumed.
//
// bwsalmon/agents#466 stopped short of proving that a dispatched task's
// tool calls actually execute inside the guest over SSH: the only kernel
// available at the time (the firecracker-ci PVH kernel bwsalmon/kontur's
// own benchmarks/kernel/build.sh fetches) panics with "Cannot open root
// device vda" against any cloud-hypervisor guest, direct-kernel-booted or
// not -- Firecracker uses virtio-mmio, cloud-hypervisor uses virtio-pci,
// and that kernel only has a driver for the former -- and packer/kontur's
// own guest image had never actually been built. bwsalmon/agents#478
// resolved both (packer/kontur/README.md, "Why no custom kernel": Debian's
// own stock kernel already has everything a cloud-hypervisor direct-
// kernel-boot guest needs, nothing built from source), so this test now
// builds that real guest image and asserts a real dispatched tool call
// actually runs inside it over SSH, not just that ToolsFor resolves an
// address.

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/kontur"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
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
	// buildKonturGuestImage below needs to run packer/kontur/build.sh as
	// root (debootstrap, bind-mounts for chroot, mke2fs -d) -- -n fails
	// fast rather than hanging this test on a password prompt it has no
	// terminal to satisfy. debootstrap/mke2fs are checked through sudo,
	// not exec.LookPath, since they typically live under /usr/sbin --
	// on root's own PATH (and so build.sh's own, since it runs under sudo
	// too) but not necessarily on this test process's unprivileged one.
	if err := exec.Command("sudo", "-n", "sh", "-c", "command -v debootstrap && command -v mke2fs").Run(); err != nil {
		t.Skipf("passwordless sudo, or debootstrap/mke2fs under it, not available (needed for packer/kontur/build.sh): %v", err)
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

// buildKonturDockerImage builds bwsalmon/kontur's own OCI image
// (third_party/kontur/Dockerfile) and returns its tag. No
// GUEST_SSH_AUTHORIZED_KEY build-arg is given: this test overrides the
// guest disk entirely (buildKonturGuestImage below, via -disk/-kernel/
// -initramfs), so the bundled reference guest this image would otherwise
// carry at /var/lib/kontur/guest/disk.img is never used.
func buildKonturDockerImage(t *testing.T) (image string) {
	t.Helper()
	image = "grain-kontur-e2e-test:latest"
	cmd := exec.Command("docker", "build", "-t", image, ".")
	cmd.Dir = "../../../third_party/kontur"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the kontur OCI image from third_party/kontur/Dockerfile: %v\n%s", err, out)
	}
	return image
}

// buildKonturGuestImage runs packer/kontur/build.sh for real -- a
// debootstrap-based Debian rootfs, provisioned with git/build tooling/
// docker/gcloud/terraform and this test's own throwaway SSH keypair,
// packed into the kernel/initramfs/disk triple konturctl's own -kernel/
// -initramfs/-disk flags point at directly -- and returns the directory
// holding all three (named exactly "vmlinuz"/"initrd.img"/"disk.img", so
// a caller's own -images-hostpath/-disk/-kernel/-initramfs arguments don't
// need to know the version-stamped name build.sh itself gives the output)
// plus the matching private key's path.
//
// Cached under a stable directory beneath os.TempDir(), like
// fetchTestKernel used to: debootstrap plus ~120MB of package downloads
// is the most expensive single step in this whole test, and neither the
// packages nor the baked-in key need to change between runs of an
// already-expensive, opt-in test.
func buildKonturGuestImage(t *testing.T) (imagesDir, sshKeyPath string) {
	t.Helper()
	imagesDir = filepath.Join(os.TempDir(), "grain-kontur-e2e-test-guest-image")
	sshKeyPath = filepath.Join(imagesDir, "id_ed25519")
	if _, err := os.Stat(filepath.Join(imagesDir, "disk.img")); err == nil {
		return imagesDir, sshKeyPath
	}
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatalf("creating guest image cache directory: %v", err)
	}
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", sshKeyPath, "-q").CombinedOutput(); err != nil {
		t.Fatalf("generating a throwaway SSH keypair: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(sshKeyPath + ".pub")
	if err != nil {
		t.Fatalf("reading generated SSH public key: %v", err)
	}

	packerKonturDir, err := filepath.Abs("../../../packer/kontur")
	if err != nil {
		t.Fatalf("resolving packer/kontur's absolute path: %v", err)
	}
	cmd := exec.Command("sudo", "-E", "./build.sh")
	cmd.Dir = packerKonturDir
	cmd.Env = append(os.Environ(), "OPERATOR_SSH_PUBLIC_KEY="+string(pub))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running packer/kontur/build.sh: %v\n%s", err, out)
	}

	// build.sh writes into its own output/<image>-<sha>-<timestamp>/
	// directory, named uniquely every run so repeated manual runs never
	// collide -- this test doesn't want that versioning, just the three
	// files under a name it already knows, so it copies them out to
	// imagesDir and removes build.sh's own copy.
	outputDirs, err := filepath.Glob(filepath.Join(packerKonturDir, "output", "*"))
	if err != nil || len(outputDirs) == 0 {
		t.Fatalf("finding build.sh's output directory under %s/output: err=%v dirs=%v", packerKonturDir, err, outputDirs)
	}
	built := outputDirs[len(outputDirs)-1]
	for _, name := range []string{"vmlinuz", "initrd.img", "disk.img"} {
		if out, err := exec.Command("sudo", "cp", filepath.Join(built, name), filepath.Join(imagesDir, name)).CombinedOutput(); err != nil {
			t.Fatalf("copying %s out of build.sh's output: %v\n%s", name, err, out)
		}
	}
	if out, err := exec.Command("sudo", "chown", "-R", strings.TrimSpace(mustRunOutput(t, "id", "-un")), imagesDir).CombinedOutput(); err != nil {
		t.Fatalf("chowning guest image cache directory back to the current user: %v\n%s", err, out)
	}
	if out, err := exec.Command("sudo", "rm", "-rf", filepath.Join(packerKonturDir, "output")).CombinedOutput(); err != nil {
		t.Fatalf("removing build.sh's own output directory: %v\n%s", err, out)
	}
	return imagesDir, sshKeyPath
}

func mustRunOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("running %s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func TestKonturSandboxesToolsForAgainstARealDockerBackedVM(t *testing.T) {
	konturDockerRealTestPrereqs(t)

	konturctlDir := buildKonturctl(t)
	image := buildKonturDockerImage(t)
	imagesHostPath, sshKeyPath := buildKonturGuestImage(t)

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
			"-disk", "/images/disk.img",
			"-kernel", "/images/vmlinuz",
			"-initramfs", "/images/initrd.img",
			// -guest-port defaults to 80 (internal/netshim/config.go's own
			// defaultGuestPort) -- netshim's DNAT rule forwards the
			// external -port below to this port inside the guest, so
			// without overriding it here every packet would be forwarded
			// to a port nothing in this guest listens on, not to sshd.
			// Confirmed by hand: connections refused indefinitely, no
			// matter how long ToolsFor waited, until this was added.
			"-guest-port", "22",
			"-ip", "169.254.100.2",
			"-port", "31080",
		},
		// "debian", not "root": packer/kontur/provision.sh bakes the
		// operator's key into the debian user's authorized_keys, matching
		// v1's own sandbox convention (grain/adapter/libvirt.py) -- see
		// that script's own comment on why the account has to be created
		// there at all rather than assumed present.
		SSHUser:           "debian",
		SSHKey:            sshKeyPath,
		Workspace:         "/home/debian",
		ReadyTimeout:      3 * time.Minute,
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
	var runCommand *mcp.Tool
	for i, tool := range tools {
		if !wantNames[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
		if tool.Name == "run_command" {
			runCommand = &tools[i]
		}
	}
	if runCommand == nil {
		t.Fatal("no run_command tool returned")
	}

	// The whole point of building a real guest image in this test (see its
	// own doc comment): a dispatched task's run_command tool call actually
	// executes inside the guest over SSH, not just that ToolsFor resolved
	// an address for a VM that may or may not ever finish booting.
	//
	// ToolsFor/resolveEndpoint above only wait for the VM's container/pod
	// to get an IP, not for the cloud-hypervisor guest inside it to finish
	// booting to sshd -- those are different points in time, confirmed by
	// hand: a docker-backed VM's container is reachable by IP the moment
	// "docker run" starts it, well before the nested guest has actually
	// booted. So the first run_command call here retries for a while
	// rather than asserting success on the first attempt.
	var result mcp.Result
	deadline := time.Now().Add(2 * time.Minute)
	for {
		result = runCommand.Handler(context.Background(), map[string]any{"command": "echo grain-kontur-e2e-marker; id -un; uname -s"})
		if !result.IsError {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run_command over SSH against the real guest never succeeded within %s: %s", 2*time.Minute, result.Text)
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(result.Text, "grain-kontur-e2e-marker") {
		t.Errorf("run_command output = %q, want it to contain the echoed marker", result.Text)
	}
	if !strings.Contains(result.Text, "debian") {
		t.Errorf("run_command output = %q, want it to show the debian user (id -un)", result.Text)
	}
	if !strings.Contains(result.Text, "Linux") {
		t.Errorf("run_command output = %q, want it to show the guest's own uname -s", result.Text)
	}

	// Confirm konturctl actually persisted a port for the VM this test
	// asked for (31080, via -port above) -- kontur.Port is how
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

	// The VM container itself should still be running: the guest booted
	// all the way to sshd (run_command above already proved that), and
	// cloud-hypervisor supervises it rather than exiting once it has.
	statusOut, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", "kontur-vm-"+name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect on the real VM container: %v\n%s", err, statusOut)
	}
	if status := string(bytes.TrimSpace(statusOut)); status != "running" {
		t.Errorf("real VM container status = %q, want %q", status, "running")
	}
}
