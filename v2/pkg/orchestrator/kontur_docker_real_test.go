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
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

// TestKonturSandboxesToolsForCreatesTwoRealVMsConcurrently is the
// concurrency counterpart to TestKonturSandboxesToolsForAgainstARealDockerBackedVM
// above: that test proves ToolsFor works against one real cloud-hypervisor
// VM under KVM, this one proves two slots' worth of real VMs actually come
// up side by side under real docker/netshim/cloud-hypervisor, driven the
// same way reconcileDispatch's own goroutine-per-dispatch loop
// (cycle.go) drives distinct slots -- concurrently, from independent
// goroutines, racing kontur.Create for two different VM names against
// each other for real, not against sandboxes_concurrency_test.go's fake
// konturctl double.
//
// It exists because bwsalmon/agents#528 asked for kontur to be exercised
// under real nested virtualization specifically for corner cases and
// concurrency, and because KonturSandboxes.ensure used to hold a single
// KonturSandboxes-wide lock across the whole kontur.Create subprocess
// call -- serializing every slot's VM creation behind whichever slot
// happened to start first, silently undoing the concurrency
// reconcileDispatch's own doc comment promises. That bug was caught and
// fixed against a fake (sandboxes_concurrency_test.go's
// TestKonturSandboxesCreatesDistinctSlotsVMsConcurrentlyNotSerially,
// which can assert wall-clock overlap precisely because its fake
// konturctl sleeps a known amount); this test is the same claim proven
// against the real thing, where "does it still work" -- two independent
// netshim network namespaces, two independent cloud-hypervisor guests,
// two independently-derived -ip/-port pairs (KonturConfig.BaseIP/
// BasePort) -- matters more than exact timing.
func TestKonturSandboxesToolsForCreatesTwoRealVMsConcurrently(t *testing.T) {
	konturDockerRealTestPrereqs(t)

	konturctlDir := buildKonturctl(t)
	image := buildKonturDockerImage(t)
	imagesHostPath, sshKeyPath := buildKonturGuestImage(t)

	t.Setenv("PATH", konturctlDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateDir := t.TempDir()
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		// "kdc-" + a one-digit slot number stays well under the 11-byte
		// cap KonturConfig.NamePrefix's own doc comment explains (netshim
		// names each VM's tap device "tap-"+name, and Linux caps interface
		// names at 15 bytes).
		NamePrefix: "kdc-",
		Backend:    kontur.BackendDocker,
		StateDir:   stateDir,
		CreateArgs: []string{
			"-kontur-image", image,
			"-images-hostpath", imagesHostPath,
			"-disk", "/images/disk.img",
			"-kernel", "/images/vmlinuz",
			"-initramfs", "/images/initrd.img",
			"-guest-port", "22",
		},
		SSHUser:           "debian",
		SSHKey:            sshKeyPath,
		Workspace:         "/home/debian",
		ReadyTimeout:      3 * time.Minute,
		ReadyPollInterval: time.Second,
		// Distinct from TestKonturSandboxesToolsForAgainstARealDockerBackedVM's
		// own hardcoded -ip/-port (169.254.100.2:31080) so the two tests
		// never fight over the same address if run back to back without a
		// clean teardown in between.
		BaseIP:   "169.254.100.20",
		BasePort: 31090,
	})

	slots := []string{"1", "2"}
	names := make([]string, len(slots))
	for i, slot := range slots {
		names[i] = k.VMNameFor(slot)
	}
	t.Cleanup(func() {
		for _, name := range names {
			if err := kontur.Delete(context.Background(), stateDir, name); err != nil {
				t.Logf("cleaning up real kontur VM %q: %v", name, err)
			}
		}
	})

	type outcome struct {
		slot  string
		tools []mcp.Tool
		err   error
	}
	results := make(chan outcome, len(slots))
	var wg sync.WaitGroup
	for _, slot := range slots {
		wg.Add(1)
		go func(slot string) {
			defer wg.Done()
			tools, err := k.ToolsFor(context.Background(), slot)
			results <- outcome{slot: slot, tools: tools, err: err}
		}(slot)
	}
	wg.Wait()
	close(results)

	toolsBySlot := map[string][]mcp.Tool{}
	for r := range results {
		if r.err != nil {
			t.Fatalf("ToolsFor(%s) against real konturctl/docker/cloud-hypervisor: %v", r.slot, r.err)
		}
		toolsBySlot[r.slot] = r.tools
	}

	// Each VM got its own independently-derived port -- confirms BaseIP/
	// BasePort's arithmetic actually reached real konturctl's "-port" flag
	// for both slots, not just the first.
	port1, err := kontur.Port(stateDir, names[0])
	if err != nil {
		t.Fatalf("kontur.Port(%s): %v", names[0], err)
	}
	port2, err := kontur.Port(stateDir, names[1])
	if err != nil {
		t.Fatalf("kontur.Port(%s): %v", names[1], err)
	}
	if port1 == port2 {
		t.Errorf("both slots' VMs got the same port %d, want BasePort-derived distinct ports", port1)
	}
	if port1 != 31090 || port2 != 31091 {
		t.Errorf("ports = %d, %d, want 31090, 31091 (BasePort=31090, offsets 0 and 1)", port1, port2)
	}

	// Each guest actually runs and answers a distinct command over SSH --
	// the same boot-time race TestKonturSandboxesToolsForAgainstARealDockerBackedVM's
	// own retry loop works around, run for two guests at once this time.
	for _, slot := range slots {
		var runCommand *mcp.Tool
		for i, tool := range toolsBySlot[slot] {
			if tool.Name == "run_command" {
				runCommand = &toolsBySlot[slot][i]
			}
		}
		if runCommand == nil {
			t.Fatalf("slot %s: no run_command tool returned", slot)
		}
		marker := fmt.Sprintf("grain-kontur-concurrent-marker-%s", slot)
		var result mcp.Result
		deadline := time.Now().Add(2 * time.Minute)
		for {
			result = runCommand.Handler(context.Background(), map[string]any{"command": "echo " + marker})
			if !result.IsError {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("slot %s: run_command over SSH never succeeded within %s: %s", slot, 2*time.Minute, result.Text)
			}
			time.Sleep(2 * time.Second)
		}
		if !strings.Contains(result.Text, marker) {
			t.Errorf("slot %s: run_command output = %q, want it to contain %q", slot, result.Text, marker)
		}
	}
}

// TestKonturSandboxesDockerExecAgainstARealDockerBackedVM is the
// docker-exec transport's (KonturConfig.DockerExec) counterpart to
// TestKonturSandboxesToolsForAgainstARealDockerBackedVM above: the same
// real konturctl, real docker, real cloud-hypervisor guest under real
// KVM, reached through `docker exec <vm container> kontur exec` instead
// of SSH to netshim's externally forwarded port.
//
// The fake-docker tests in kontur_sandboxes_test.go can prove that
// nothing looks a VM's address up under this transport, because a fake
// can be made to fail that lookup. What they cannot prove is the half
// this test exists for, all of which depends on code neither this repo
// nor its fakes own:
//
//   - That `kontur exec` authenticates against *this* guest image at all.
//     bwsalmon/kontur's own Dockerfile generates a dedicated keypair and
//     authorizes it on the guest rootfs that same Dockerfile builds
//     (its exec-keypair stage), but a grain deployment never boots that
//     guest -- packer/kontur/build.sh's output, whose only
//     authorized_keys entry is the operator key, is what -disk points at.
//     So this transport only works here because KONTUR_EXEC_KEY can be
//     pointed at a key the guest does authorize, which is a claim about
//     kontur's env handling and this image's authorized_keys that only a
//     real run can settle.
//   - That KONTUR_EXEC_ADDR is really set, by the real internal/dockervm,
//     to somewhere the guest really answers on. Everything else here
//     rests on it, and nothing in this repo sets it.
//   - That a guest command's exit status survives both hops (the guest's
//     sshd to `kontur exec`, then `kontur exec`'s own os.Exit to
//     `docker exec`), including the exit 1 that DockerExecRunner has to
//     tell apart from its own failure to reach the guest at all.
//   - That stdin survives both hops, which write_file depends on (it
//     pipes content into `dd` rather than embedding it in a command
//     line).
//
// It skips under exactly the same conditions the tests above do, and
// shares their build helpers, so it costs a host without docker/KVM
// nothing.
func TestKonturSandboxesDockerExecAgainstARealDockerBackedVM(t *testing.T) {
	konturDockerRealTestPrereqs(t)

	konturctlDir := buildKonturctl(t)
	image := buildKonturDockerImage(t)
	imagesHostPath, sshKeyPath := buildKonturGuestImage(t)

	t.Setenv("PATH", konturctlDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// buildKonturGuestImage generates its throwaway keypair *into*
	// imagesHostPath, and internal/dockervm mounts that same directory
	// read-only at /images in every VM container it starts -- so the
	// private key DockerExecKeyPath names is already somewhere the
	// container can read it, with nothing to copy and no mount to add.
	// That is the same arrangement KonturConfig.DockerExecKeyPath's own
	// doc comment recommends to a real deployment, exercised here rather
	// than only described.
	//
	// Asserted rather than assumed: if that helper ever puts its keypair
	// somewhere else, the failure this would otherwise produce is an
	// authentication error deep inside a guest, which says nothing about
	// why.
	if got, want := filepath.Dir(sshKeyPath), imagesHostPath; got != want {
		t.Fatalf("guest image helper put its SSH key in %s, want it inside the images directory %s that gets mounted at /images -- this test's DockerExecKeyPath depends on those being the same place", got, want)
	}
	dockerExecKey := "/images/" + filepath.Base(sshKeyPath)

	stateDir := t.TempDir()
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		// "kde-" + a one-digit slot, staying under the 11-byte cap
		// KonturConfig.NamePrefix's own doc comment explains, and
		// distinct from the two tests above so all three can run back to
		// back.
		NamePrefix: "kde-",
		Backend:    kontur.BackendDocker,
		StateDir:   stateDir,
		CreateArgs: []string{
			"-kontur-image", image,
			"-images-hostpath", imagesHostPath,
			"-disk", "/images/disk.img",
			"-kernel", "/images/vmlinuz",
			"-initramfs", "/images/initrd.img",
			// Still 22, and still not optional -- konturctl's own default
			// is 80. This transport never goes through the DNAT rule that
			// forwards to it, but konturctl validates and netshim
			// installs the rule either way, so leaving it wrong would
			// only mean building a VM whose forwarded port goes nowhere.
			"-guest-port", "22",
			// Distinct from both tests above (169.254.100.2:31080 and
			// 169.254.100.20+:31090+) so none of the three fight over an
			// address if run back to back without a clean teardown.
			"-ip", "169.254.100.40",
			"-port", "31110",
		},
		SSHUser: "debian",
		// Still set, and still the host path: nothing on this transport
		// reads it, but leaving it out would make this config differ from
		// a real deployment's in a way this test has no reason to.
		SSHKey:            sshKeyPath,
		DockerExec:        true,
		DockerExecKeyPath: dockerExecKey,
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
		t.Fatalf("ToolsFor over docker exec against a real konturctl/docker/cloud-hypervisor VM: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for i := range tools {
		byName[tools[i].Name] = &tools[i]
	}
	for _, want := range []string{"run_command", "read_file", "write_file", "edit_file"} {
		if byName[want] == nil {
			t.Fatalf("ToolsFor() returned no %s tool (got %d tools)", want, len(tools))
		}
	}

	// Confirm the variable this whole transport rests on is really set on
	// the real VM container, by the real internal/dockervm -- and points
	// at the address this VM was created with. Nothing in this repo sets
	// it, so nothing in this repo would notice it changing.
	envOut, err := exec.Command("docker", "inspect", "-f",
		`{{range .Config.Env}}{{println .}}{{end}}`, "kontur-vm-"+name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect on the real VM container: %v\n%s", err, envOut)
	}
	if want := "KONTUR_EXEC_ADDR=169.254.100.40:22"; !strings.Contains(string(envOut), want) {
		t.Errorf("VM container env = %q, want it to carry %q -- `kontur exec` has no other way to know where the guest is", envOut, want)
	}

	// Unlike the SSH test above, this needs no retry loop around the
	// first tool call. resolveEndpoint's readiness wait can only watch a
	// TCP port start answering, which happens before the guest has
	// finished booting to a usable sshd; waitForGuestExec's probe is a
	// whole command *running in the guest*, so ToolsFor returning here
	// already means the guest ran one. Asserting that directly, rather
	// than retrying and hiding it, is what would catch that guarantee
	// regressing.
	result := byName["run_command"].Handler(context.Background(), map[string]any{
		"command": "echo grain-kontur-dockerexec-marker; id -un; uname -s",
	})
	if result.IsError {
		t.Fatalf("run_command over docker exec failed on the first attempt, though ToolsFor had already run a command in this guest to decide it was ready: %s", result.Text)
	}
	for _, want := range []string{"grain-kontur-dockerexec-marker", "debian", "Linux"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("run_command output = %q, want it to contain %q", result.Text, want)
		}
	}

	// A guest command's own exit status has to survive the guest's sshd
	// -> `kontur exec` -> `docker exec` chain intact. 42 proves the
	// status is carried rather than collapsed to a success/failure bit;
	// 1 is the one that matters most, since it is exactly what
	// DockerExecRunner's own failure-to-reach-the-guest case also exits
	// with (see execFailedBeforeGuest) -- a -1 here would mean this
	// transport reports "the command never ran" for every command that
	// merely failed.
	for _, code := range []int{42, 1} {
		result := byName["run_command"].Handler(context.Background(), map[string]any{
			"command": fmt.Sprintf("exit %d", code),
		})
		if !result.IsError {
			t.Errorf("run_command `exit %d` reported success, want an error result", code)
		}
		if want := fmt.Sprintf("exit=%d", code); !strings.Contains(result.Text, want) {
			t.Errorf("run_command `exit %d` result = %q, want it to report %q", code, result.Text, want)
		}
	}

	// write_file pipes its content over stdin (sshWriteRemote's `dd`), so
	// a write/read round trip is what proves stdin survives both hops --
	// the one direction `docker exec` needs -i for, and the one a
	// command-line-only test would never exercise. The tab and the
	// trailing newline are there to catch a round trip that "works" but
	// mangles whitespace somewhere in the two shells between here and
	// that `dd`.
	//
	// The path is absolute rather than relative to Workspace: these two
	// tools pass file_path through to `cat`/`dd` verbatim (unlike
	// run_command, which cds into Workspace first), so a relative path
	// would resolve against whatever directory the guest's own login
	// leaves the session in.
	const content = "docker exec stdin round trip\nsecond line\twith a tab\n"
	const remotePath = "/home/debian/dockerexec-stdin.txt"
	if result := byName["write_file"].Handler(context.Background(), map[string]any{
		"file_path": remotePath, "content": content,
	}); result.IsError {
		t.Fatalf("write_file over docker exec: %s", result.Text)
	}

	// Byte-for-byte, through run_command: what `cat` prints lands in
	// run_command's own output unaltered, so this is the assertion that
	// actually holds stdin fidelity. read_file below cannot serve that
	// purpose -- it renders what it read as numbered lines
	// (numberedRange), which no longer contains the original text.
	catResult := byName["run_command"].Handler(context.Background(), map[string]any{
		"command": "cat -- " + remotePath,
	})
	if catResult.IsError {
		t.Fatalf("reading back what write_file piped over stdin: %s", catResult.Text)
	}
	if !strings.Contains(catResult.Text, content) {
		t.Errorf("guest file contents = %q, want it to carry back exactly what write_file piped in over stdin (%q)", catResult.Text, content)
	}

	// And through read_file itself, so the tool a dispatched task
	// actually calls is exercised over this transport too -- asserted per
	// line, since its output is numbered rather than raw.
	readBack := byName["read_file"].Handler(context.Background(), map[string]any{
		"file_path": remotePath,
	})
	if readBack.IsError {
		t.Fatalf("read_file over docker exec: %s", readBack.Text)
	}
	for _, want := range []string{"docker exec stdin round trip", "second line\twith a tab"} {
		if !strings.Contains(readBack.Text, want) {
			t.Errorf("read_file returned %q, want it to carry the line %q", readBack.Text, want)
		}
	}

	// The VM container should still be running: the guest booted all the
	// way to sshd and cloud-hypervisor supervises it rather than exiting
	// once it has -- the same end state the SSH test asserts, reached
	// without anything outside this container ever connecting to it.
	statusOut, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", "kontur-vm-"+name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect on the real VM container: %v\n%s", err, statusOut)
	}
	if status := string(bytes.TrimSpace(statusOut)); status != "running" {
		t.Errorf("real VM container status = %q, want %q", status, "running")
	}
}
