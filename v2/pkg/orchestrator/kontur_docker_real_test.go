package orchestrator_test

// TestKonturSandboxesAgainstARealDockerBackedVM is the one place
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
// same "skip rather than run" shape tests/test_vm_integration.py's
// own gate uses for v1's equivalent live suite (that file's own comment:
// "none of which a hosted runner has, so they skip rather than run").
// The guest image build below used to add passwordless sudo plus
// debootstrap/mke2fs to that list; since packer/kontur converged on
// kontur's own guest build it needs nothing beyond the docker already
// required here (packer/kontur/README.md, "Converged on kontur's own
// guest build").
// Neither of these is present on the hosted GitHub Actions runner
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
// actually runs inside it over SSH, not just that Acquire resolves an
// address.

import (
	"bytes"
	"context"
	"fmt"
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
	// buildKonturGuestImage below needs no privilege check of its own:
	// packer/kontur/build-guest.sh is one `docker build`, against the
	// same daemon already proven reachable above. It deliberately gets no
	// skip of its own for a docker too old for BuildKit's --output --
	// that is a broken host rather than a missing prerequisite, and a
	// build failure naming it says far more than a silent skip would.
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

// buildKonturGuestImage runs packer/kontur/build-guest.sh for real -- one
// `docker build` against third_party/kontur's Dockerfile, which
// debootstraps the rootfs and packs it with `mke2fs -d` inside the build,
// with packer/kontur/guest-setup.sh handed to its GUEST_SETUP_SCRIPT hook
// to add git/build tooling/docker/gcloud/terraform -- and returns the
// directory holding the kernel/initramfs/disk triple konturctl's own
// -kernel/-initramfs/-disk flags point at directly.
//
// No SSH key goes into it. This helper used to generate a throwaway
// keypair and hand build-guest.sh the public half as
// OPERATOR_SSH_PUBLIC_KEY, because that was the only entry in the guest's
// authorized_keys. kontur now generates a keypair inside each VM's own
// container at boot and hands the guest the public half on the kernel
// command line (bwsalmon/kontur#35), so the image this builds carries no
// key and the callers below configure no ExecKeyPath.
//
// OUTPUT_DIR is what makes that directory this function's own to name:
// build-guest.sh writes "vmlinuz"/"initrd.img"/"disk.img" straight into
// it, so nothing here has to find, copy out of, or clean up the
// version-stamped output/<image>-<sha>-<timestamp>/ directory a bare run
// of that script produces (which is also why this no longer needs sudo to
// chown the result back: the build writes as the user running it).
//
// Cached under a stable directory beneath os.TempDir(), like
// fetchTestKernel used to: the rootfs build plus ~120MB of package
// downloads is the most expensive single step in this whole test, and
// nothing in it needs to change between runs of an already-expensive,
// opt-in test. Now that no per-run key is baked in, docker's own layer
// cache would mostly cover this too -- the guest-setup.sh text is the
// same on every run -- but this directory is also what OUTPUT_DIR points
// at, so it stays.
func buildKonturGuestImage(t *testing.T) (imagesDir string) {
	t.Helper()
	imagesDir = filepath.Join(os.TempDir(), "grain-kontur-e2e-test-guest-image")
	if _, err := os.Stat(filepath.Join(imagesDir, "disk.img")); err == nil {
		return imagesDir
	}
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatalf("creating guest image cache directory: %v", err)
	}

	packerKonturDir, err := filepath.Abs("../../../packer/kontur")
	if err != nil {
		t.Fatalf("resolving packer/kontur's absolute path: %v", err)
	}
	cmd := exec.Command("./build-guest.sh")
	cmd.Dir = packerKonturDir
	// KONTUR_IMAGE_BUCKET and SANDBOX_SETUP_SCRIPT are pinned empty
	// rather than inherited: set in the environment of whoever runs this
	// test, the first would publish this throwaway image to a real GCS
	// bucket and the second would bake unreviewed content into the guest
	// the assertions below then run against. build-guest.sh treats both
	// as unset when empty.
	cmd.Env = append(os.Environ(),
		"OUTPUT_DIR="+imagesDir,
		"KONTUR_IMAGE_BUCKET=",
		"SANDBOX_SETUP_SCRIPT=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running packer/kontur/build-guest.sh: %v\n%s", err, out)
	}

	// build-guest.sh already fails if any of the three is missing or
	// empty, so this only catches it writing somewhere other than
	// OUTPUT_DIR -- which would otherwise surface as a VM that never
	// boots, saying nothing about why.
	for _, name := range []string{"vmlinuz", "initrd.img", "disk.img"} {
		if _, err := os.Stat(filepath.Join(imagesDir, name)); err != nil {
			t.Fatalf("build-guest.sh reported success but %s is not in OUTPUT_DIR (%s): %v\n%s", name, imagesDir, err, out)
		}
	}
	return imagesDir
}

// TestKonturSandboxesAcquireCreatesTwoRealVMsConcurrently is the
// concurrency counterpart to TestKonturSandboxesAgainstARealDockerBackedVM
// above: that test proves Acquire works against one real cloud-hypervisor
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
// TestKonturSandboxesCreatesDistinctVMsConcurrentlyNotSerially, which can
// assert wall-clock overlap precisely because its fake konturctl sleeps a
// known amount); this test is the same claim proven against the real
// thing, where "does it still work" -- two independent netshim network
// namespaces, two independent cloud-hypervisor guests -- matters more
// than exact timing.
func TestKonturSandboxesAcquireCreatesTwoRealVMsConcurrently(t *testing.T) {
	konturDockerRealTestPrereqs(t)

	konturctlDir := buildKonturctl(t)
	image := buildKonturDockerImage(t)
	imagesHostPath := buildKonturGuestImage(t)

	t.Setenv("PATH", konturctlDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateDir := t.TempDir()
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir: stateDir,
		CreateArgs: []string{
			"-kontur-image", image,
			"-images-hostpath", imagesHostPath,
			"-disk", "/images/disk.img",
			"-kernel", "/images/vmlinuz",
			"-initramfs", "/images/initrd.img",
			"-guest-port", "22",
			// kontur authorizes this boot's generated key for root; the
			// account SSHUser names has to be named too, or `kontur exec`
			// logs in as someone the guest never authorized. Same flag
			// v2/scripts/setup.sh passes in a real deployment.
			"-guest-user", "debian",
			// -disk-readonly=false/-disk-hostpath give each VM its own
			// private writable qcow2 overlay (one subdirectory per VM
			// name under -disk-hostpath, so the two slots' VMs sharing
			// this one directory is fine) instead of attaching -disk
			// itself read-only. Without this, the guest's root filesystem
			// stays read-only end to end (confirmed by hand: a real VM
			// created without it reports "/dev/vda / ext4 ro,relatime" in
			// /proc/mounts), which fails kontur-ssh-host-keys.service's
			// first-boot `ssh-keygen -A` -- so sshd never has a host key
			// to start with, and the guest never becomes reachable within
			// ReadyTimeout at all. The same pair v2/scripts/setup.sh's
			// own GRAIN_KONTUR_ENABLE=1 branch always passes in a real
			// deployment (bwsalmon/agents#510).
			"-disk-readonly=false",
			"-disk-hostpath", t.TempDir(),
		},
		SSHUser:           "debian",
		Workspace:         "/home/debian",
		ReadyTimeout:      3 * time.Minute,
		ReadyPollInterval: time.Second,
		// No IP/Port: this runs in flat mode (the default), where
		// createArgs drops both because the container runtime assigns the
		// address. They were set here to keep this test's VMs off the
		// other real test's addresses, which flat mode makes moot -- and
		// leaving them set implied this test exercises addressing, which
		// it does not.
	})

	// Two run-shaped sandbox names, distinct from the other real test's
	// below. The prefix is a constant now (orchestrator.VMNamePrefix), so
	// keeping these tests' VMs apart on a shared docker daemon is the
	// sandbox name's job rather than the prefix's.
	slots := []string{"c1-1", "c1-2"}
	names := make([]string, len(slots))
	for i, slot := range slots {
		name, err := k.VMNameFor(slot)
		if err != nil {
			t.Fatal(err)
		}
		names[i] = name
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
			sb, err := k.Acquire(context.Background(), slot, orchestrator.Shape{})
			if err != nil {
				results <- outcome{slot: slot, err: err}
				return
			}
			t.Cleanup(func() { _ = sb.Release(context.Background()) })
			tools, err := sb.Tools(context.Background())
			results <- outcome{slot: slot, tools: tools, err: err}
		}(slot)
	}
	wg.Wait()
	close(results)

	// Every outcome is reported before failing, rather than t.Fatalf on
	// whichever error came off the channel first. Which VMs failed is the
	// whole diagnosis here: one failing where the other came up is a
	// concurrency problem in this package, while both failing is the
	// guest image's own flat-mode control link never coming up, which is
	// nothing to do with how the VMs were created.
	toolsBySlot := map[string][]mcp.Tool{}
	var failures []string
	for r := range results {
		if r.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.slot, r.err))
			continue
		}
		toolsBySlot[r.slot] = r.tools
	}
	if len(failures) > 0 {
		t.Fatalf("%d of %d VMs failed against real konturctl/docker/cloud-hypervisor:\n  %s",
			len(failures), len(slots), strings.Join(failures, "\n  "))
	}

	// Both VMs are reached at the *same* guest address, and that is the
	// point rather than a defect: under flat mode (the default) netshim
	// gives each VM its own network namespace, so each guest takes the
	// same control-link address inside its own, and `kontur exec` reaches
	// it by exec'ing into that VM's own container (KONTUR_EXEC_ADDR).
	//
	// This assertion used to be the opposite -- that the two addresses
	// differed, "want BaseIP-derived distinct ones" -- which was true
	// while KonturConfig derived an -ip per slot. It would now fail on a
	// perfectly healthy pair of VMs, and it is worth more inverted than
	// deleted: two guests answering independently on one address is the
	// evidence for the claim that removing that derivation is safe.
	addrs := map[string]string{}
	for _, name := range names {
		out, err := exec.Command("docker", "inspect", "-f",
			`{{range .Config.Env}}{{println .}}{{end}}`, "kontur-vm-"+name).CombinedOutput()
		if err != nil {
			t.Fatalf("docker inspect on %s: %v\n%s", name, err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "KONTUR_EXEC_ADDR=") {
				addrs[name] = strings.TrimPrefix(line, "KONTUR_EXEC_ADDR=")
			}
		}
		if addrs[name] == "" {
			t.Fatalf("%s: no KONTUR_EXEC_ADDR in the VM container's env:\n%s", name, out)
		}
	}
	if addrs[names[0]] != addrs[names[1]] {
		t.Errorf("VMs got different guest addresses %q and %q, want the same one: under flat mode each VM has its own network namespace, so nothing derives a per-VM address",
			addrs[names[0]], addrs[names[1]])
	}

	// Each guest actually runs and answers a distinct command, for two
	// guests at once this time.
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

// TestKonturSandboxesAgainstARealDockerBackedVM is the
// docker-exec transport's (KonturConfig.DockerExec) counterpart to
// TestKonturSandboxesAgainstARealDockerBackedVM above: the same
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
//   - That `kontur exec` authenticates against *this* guest image at all,
//     with no key configured anywhere. Since bwsalmon/kontur#35 that
//     rests on a chain nothing here owns and no fake can stand in for:
//     `kontur run` generates a keypair in the VM's container, appends the
//     public half to the guest's kernel command line, and the guest's own
//     kontur-authorized-key service decodes it and installs it -- for
//     root, and for the account "-guest-user debian" names, before sshd
//     starts. Every link has to hold, and a break in any of them looks
//     identical from here: a guest that never becomes reachable. This is
//     the only test that can tell them apart from each other, or from a
//     guest that simply booted slowly.
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
func TestKonturSandboxesAgainstARealDockerBackedVM(t *testing.T) {
	konturDockerRealTestPrereqs(t)

	konturctlDir := buildKonturctl(t)
	image := buildKonturDockerImage(t)
	imagesHostPath := buildKonturGuestImage(t)

	t.Setenv("PATH", konturctlDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateDir := t.TempDir()
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir: stateDir,
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
			// kontur authorizes this boot's generated key for root; the
			// account SSHUser names has to be named too, or `kontur exec`
			// logs in as someone the guest never authorized. Same flag
			// v2/scripts/setup.sh passes in a real deployment.
			"-guest-user", "debian",
			// No -ip/-port: this VM is built in flat mode (KonturConfig's
			// own default since 7a58bec), where the container runtime
			// assigns the address the guest takes over and konturctl
			// rejects -ip outright -- "ip must not be set in \"flat\" net
			// mode". These were left behind when the default changed, and
			// failed every run of this test since. There is also nothing
			// left for them to collide with: each VM gets its own network
			// namespace under the docker backend, so the "distinct from
			// the concurrent test above" they used to carry was guarding
			// a collision that shape makes impossible.
			//
			// -disk-readonly defaults to true (staticpod.Defaults),
			// attaching -disk itself directly rather than a private
			// overlay -- fine for a guest that only ever reads it, but
			// this test also proves write_file/edit_file work over this
			// transport (below), and those need a guest whose root
			// filesystem is actually writable. Confirmed by hand against
			// a real VM without this pair: `cat /proc/mounts` inside the
			// guest reports "/dev/vda / ext4 ro,relatime", and write_file
			// fails with "Read-only file system". -disk-hostpath (a
			// directory only this test's VM uses, distinct from
			// imagesHostPath, which is always mounted read-only since
			// several VMs may share it) is where konturctl puts the
			// private qcow2 overlay -disk-readonly=false backs onto
			// -disk with -- the same pair v2/scripts/setup.sh's own
			// GRAIN_KONTUR_ENABLE=1 branch always passes in a real
			// deployment (bwsalmon/agents#510).
			"-disk-readonly=false",
			"-disk-hostpath", t.TempDir(),
		},
		SSHUser:           "debian",
		Workspace:         "/home/debian",
		ReadyTimeout:      3 * time.Minute,
		ReadyPollInterval: time.Second,
	})

	name, err := k.VMNameFor("e1-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := kontur.Delete(context.Background(), stateDir, name); err != nil {
			t.Logf("cleaning up real kontur VM %q: %v", name, err)
		}
	})

	sb, err := k.Acquire(context.Background(), "e1-1", orchestrator.Shape{})
	if err != nil {
		t.Fatalf("Acquire over docker exec against a real konturctl/docker/cloud-hypervisor VM: %v", err)
	}
	tools, err := sb.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for i := range tools {
		byName[tools[i].Name] = &tools[i]
	}
	for _, want := range []string{"run_command", "read_file", "write_file", "edit_file"} {
		if byName[want] == nil {
			t.Fatalf("Tools() returned no %s tool (got %d tools)", want, len(tools))
		}
	}

	// Confirm the variable this whole transport rests on is really set on
	// the real VM container, by the real internal/dockervm -- and points
	// at the guest's flat-mode control-link address. Nothing in this repo
	// sets it, so nothing in this repo would notice it changing.
	//
	// 169.254.100.2 rather than a -ip this test chose: under flat mode
	// (the default since 7a58bec) there is no per-VM address to assign --
	// konturctl rejects -ip outright -- so this is netshim's own fixed
	// control-link guest address (ControlGuestIP, one past its default
	// 169.254.100.1 bridge address), confirmed by hand against a real VM.
	envOut, err := exec.Command("docker", "inspect", "-f",
		`{{range .Config.Env}}{{println .}}{{end}}`, "kontur-vm-"+name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect on the real VM container: %v\n%s", err, envOut)
	}
	if want := "KONTUR_EXEC_ADDR=169.254.100.2:22"; !strings.Contains(string(envOut), want) {
		t.Errorf("VM container env = %q, want it to carry %q -- `kontur exec` has no other way to know where the guest is", envOut, want)
	}

	// Unlike the SSH test above, this needs no retry loop around the
	// first tool call. resolveEndpoint's readiness wait can only watch a
	// TCP port start answering, which happens before the guest has
	// finished booting to a usable sshd; waitForGuestExec's probe is a
	// whole command *running in the guest*, so Acquire returning here
	// already means the guest ran one. Asserting that directly, rather
	// than retrying and hiding it, is what would catch that guarantee
	// regressing.
	result := byName["run_command"].Handler(context.Background(), map[string]any{
		"command": "echo grain-kontur-dockerexec-marker; id -un; uname -s",
	})
	if result.IsError {
		t.Fatalf("run_command over docker exec failed on the first attempt, though Acquire had already run a command in this guest to decide it was ready: %s", result.Text)
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
