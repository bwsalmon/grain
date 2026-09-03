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
// exactly why pkg/kontur's own Create/Delete execing a binary literally
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
// debootstrap/mke2fs to that list; since scripts/kontur converged on
// kontur's own guest build it needs nothing beyond the docker already
// required here (scripts/kontur/README.md, "Converged on kontur's own
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
// and that kernel only has a driver for the former -- and scripts/kontur's
// own guest image had never actually been built. bwsalmon/agents#478
// resolved both (scripts/kontur/README.md, "Why no custom kernel": Debian's
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/kontur"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// konturDockerRealTestPrereqs are the binaries and devices this test
// needs on the host it runs on, beyond what every other test in this
// repo already assumes (go, a POSIX shell). Each check's own failure
// message is the skip reason, so a run against a host missing exactly one
// of these says which.
func konturDockerRealTestPrereqs(t *testing.T) {
	t.Helper()
	// Opt-in rather than "run wherever the host happens to allow it".
	// GitHub's Linux runners do expose /dev/kvm, so before this gate the
	// checks below passed in tests.yml's go-test job too and this ~5.6
	// minute suite ran twice per commit -- once there, once in the
	// real-vm job built for it. The go-test copy had no timeout of its
	// own, so it ran against `go test`'s 600s per-package default with
	// about 1.8x headroom, and lost that race often enough to redden PRs
	// that had not touched any of this (bwsalmon/grain#519).
	//
	// Coverage is unchanged: real-vm sets this and asserts both tests
	// reported "--- PASS", so dropping the variable there fails that job
	// loudly rather than quietly skipping the suite it exists to run.
	if os.Getenv("GRAIN_REAL_VM_TESTS") == "" {
		t.Skip("GRAIN_REAL_VM_TESTS not set; this suite runs in tests.yml's real-vm job (set it to 1 to run locally)")
	}
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
	// scripts/kontur/build-guest.sh is one `docker build`, against the
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
	cmd.Dir = "../../third_party/kontur"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building konturctl from third_party/kontur: %v\n%s", err, out)
	}
	return dir
}

// buildKonturGuestImage runs scripts/kontur/build-guest.sh for real and
// returns the image reference it produced: kontur's published guest,
// booted, provisioned by scripts/kontur/guest-setup.sh with
// git/build tooling/docker/gcloud/terraform, scrubbed and committed.
//
// It returns one image reference rather than a directory of three files
// because that is what a VM needs now. This used to hand back a
// kernel/initramfs/disk triple for konturctl's -kernel/-initramfs/-disk
// flags to point at, built by a `docker build` that debootstrapped a
// rootfs from scratch; the guest travels inside the image itself since
// bwsalmon/kontur#36, so there is nothing to unpack and nothing for the
// caller to mount.
//
// No SSH key goes into it. This helper used to generate a throwaway
// keypair and hand the build the public half, because that was the only
// entry in the guest's authorized_keys. kontur generates a keypair inside
// each VM's own container at boot and hands the guest the public half on
// the kernel command line (bwsalmon/kontur#35), so the image carries no
// key and the callers below configure no ExecKeyPath.
//
// Cached by tag: the build boots a VM and installs several hundred MB of
// packages inside it, which is the most expensive single step in this
// already-expensive, opt-in test, and nothing in it changes between runs.
// A rebuild is one `docker rmi` away.
//
// The base it derives from is built by that script out of
// third_party/kontur, so this exercises the vendored tree end to end --
// konturctl, the base image and the guest all from the same source.
func buildKonturGuestImage(t *testing.T) (image string) {
	t.Helper()
	image = "grain-guest:e2e-test"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err == nil {
		return image
	}

	konturDir, err := filepath.Abs("../../scripts/kontur")
	if err != nil {
		t.Fatalf("resolving scripts/kontur's absolute path: %v", err)
	}
	cmd := exec.Command("./build-guest.sh")
	cmd.Dir = konturDir
	// Both pinned empty rather than inherited. Set in the environment of
	// whoever runs this test, GUEST_SOURCE_REPO would relabel a throwaway
	// image as if it were a published one, and KONTUR_GUEST_BASE would
	// derive from some other image entirely -- defeating the point of
	// building everything here from the vendored tree.
	cmd.Env = append(os.Environ(),
		"IMAGE="+image,
		"KONTUR_GUEST_BASE=",
		"GUEST_SOURCE_REPO=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running scripts/kontur/build-guest.sh: %v\n%s", err, out)
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Fatalf("build-guest.sh reported success but %s does not exist: %v\n%s", image, err, out)
	}
	return image
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
	image := buildKonturGuestImage(t)

	t.Setenv("PATH", konturctlDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateDir := t.TempDir()
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir: stateDir,
		CreateArgs: []string{
			// No -disk/-kernel/-initramfs, and no -images-hostpath: the
			// guest travels inside -kontur-image, so konturctl boots
			// what that image carries.
			"-kontur-image", image,
			"-guest-port", "22",
			// kontur authorizes this boot's generated key for root; the
			// account SSHUser names has to be named too, or `kontur exec`
			// logs in as someone the guest never authorized. Same flag
			// scripts/setup.sh passes in a real deployment.
			"-guest-user", "debian",
			// Each VM gets a writable root: a thin qcow2 created inside
			// its own container, backed by the image's disk, which is
			// only ever read (bwsalmon/kontur#37). konturctl's default is
			// read-only, and without a writable root the guest fails
			// kontur-ssh-host-keys.service's first-boot `ssh-keygen -A`
			// -- so sshd never has a host key, and the guest never
			// becomes reachable within ReadyTimeout at all (confirmed by
			// hand: such a VM reports "/dev/vda / ext4 ro,relatime" in
			// /proc/mounts). The same flag scripts/setup.sh's own
			// GRAIN_KONTUR_ENABLE=1 branch passes in a real deployment.
			"-disk-mode=overlay",
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
	image := buildKonturGuestImage(t)

	t.Setenv("PATH", konturctlDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateDir := t.TempDir()
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir: stateDir,
		CreateArgs: []string{
			// No -disk/-kernel/-initramfs, and no -images-hostpath: the
			// guest travels inside -kontur-image.
			"-kontur-image", image,
			// Still 22, and still not optional -- konturctl's own default
			// is 80. This transport never goes through the DNAT rule that
			// forwards to it, but konturctl validates and netshim
			// installs the rule either way, so leaving it wrong would
			// only mean building a VM whose forwarded port goes nowhere.
			"-guest-port", "22",
			// kontur authorizes this boot's generated key for root; the
			// account SSHUser names has to be named too, or `kontur exec`
			// logs in as someone the guest never authorized. Same flag
			// scripts/setup.sh passes in a real deployment.
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
			// konturctl's disk mode defaults to read-only, which is fine
			// for a guest that only ever reads its disk -- but this test
			// also proves write_file/edit_file work over this transport
			// (below), and those need a root filesystem that is actually
			// writable. Confirmed by hand against a real VM without this:
			// `cat /proc/mounts` inside the guest reports "/dev/vda /
			// ext4 ro,relatime", and write_file fails with "Read-only
			// file system". overlay gives it one for free -- a thin qcow2
			// in the VM's own container, backed by the image's disk
			// (bwsalmon/kontur#37) -- and is what scripts/setup.sh passes.
			"-disk-mode=overlay",
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

	// This needs no retry loop around the first tool call. A readiness
	// wait that only watched a TCP port start answering -- what reaching
	// the guest over a forwarded port used to do -- would clear before
	// the guest had finished booting to a usable sshd; waitForGuestExec's
	// probe is a whole command *running in the guest*, so Acquire
	// returning here already means the guest ran one. Asserting that directly, rather
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

	assertGuestHasEgress(t, byName["run_command"], name)
	assertSandboxDiskSizeApplies(t, k, stateDir, byName["run_command"])

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

// assertGuestHasEgress checks the one hop nothing else here covers: that
// the guest can reach anything at all beyond its own segment.
//
// A flat-mode guest takes over the identity the container runtime
// assigned the namespace, and the last piece of that identity is the
// namespace's default route -- which the guest can only learn from the
// "ip=" parameter netshim derives for it (its own DiscoverIdentity and
// FlatGuestConfig, in third_party/kontur). Every other part of the
// takeover fails loudly: a guest with the wrong address or MAC is a
// guest `kontur exec` cannot reach, so it never becomes ready and this
// suite fails on that. A missing gateway fails silently instead. The VM
// boots, sshd answers over the control link, every tool call in this
// test passes -- and the run inside it cannot reach the git proxy, a
// package registry, or GitHub, because the guest's routing table stops
// at its own subnet.
//
// That is exactly what happened: netshim looked for the default route by
// testing "Dst == nil", which a route read back off the kernel never has
// (the netlink library synthesizes 0.0.0.0/0 for the missing RTA_DST the
// way iproute2 does), so every guest booted with an empty gateway field
// and no egress. It survived because nothing asserted it.
//
// The gateway is compared against the one docker itself reports for the
// VM's own network namespace, rather than a literal, so this says "the
// guest took over the namespace's default route" rather than "the runner
// happened to use 172.17.0.1".
// assertSandboxDiskSizeApplies is the far end of the disk-size setting
// (model.Config.SandboxDiskGB -> Shape.DiskGB ->
// KonturConfig.createArgs' -disk-size-mb -> CHV_DISK_SIZE_MB -> the
// qcow2 overlay kontur sizes before cloud-hypervisor opens it),
// asserted from inside a guest. That whole chain is only visible at once
// here, which is what makes it worth the extra VM: a fake konturctl can
// prove the flag was passed and nothing beyond it.
//
// It builds a second sandbox rather than sizing the one the rest of this
// test uses, because the size to ask for is not a number this test can
// write down. kontur refuses a size below the disk image the overlay
// reads through to, and that image is this repo's guest -- which grows
// every time guest-setup.sh installs one more thing. So the size comes
// from the unsized guest already running: one GiB more than the disk it
// booted with, which is both certainly above kontur's floor and small
// enough that growing the filesystem onto it costs a runner nothing.
//
// runCommand belongs to that first, unsized sandbox.
func assertSandboxDiskSizeApplies(t *testing.T, k *orchestrator.KonturSandboxes, stateDir string, runCommand *mcp.Tool) {
	t.Helper()

	baseMB := guestRootDeviceMB(t, runCommand)
	wantGB := baseMB/1024 + 1
	wantMB := wantGB * 1024

	name, err := k.VMNameFor("e1-2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := kontur.Delete(context.Background(), stateDir, name); err != nil {
			t.Logf("cleaning up the sized kontur VM %q: %v", name, err)
		}
	})
	sb, err := k.Acquire(context.Background(), "e1-2", orchestrator.Shape{DiskGB: wantGB})
	if err != nil {
		t.Fatalf("Acquire of a sandbox asking for a %d GiB disk (the guest image's own %d MiB plus a GiB): %v", wantGB, baseMB, err)
	}
	t.Cleanup(func() { _ = sb.Release(context.Background()) })

	tools, err := sb.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sizedRunCommand *mcp.Tool
	for i := range tools {
		if tools[i].Name == "run_command" {
			sizedRunCommand = &tools[i]
		}
	}
	if sizedRunCommand == nil {
		t.Fatalf("the sized sandbox returned no run_command tool (got %d tools)", len(tools))
	}

	// The block device first: this is the virtual size kontur gave the
	// overlay, and it being unchanged from the unsized guest's means the
	// size never reached kontur at all.
	if gotMB := guestRootDeviceMB(t, sizedRunCommand); gotMB < wantMB {
		t.Errorf("a sandbox created with -disk-size-mb %d has a %d MiB /dev/vda; the unsized one has %d MiB, so the size never reached the overlay",
			wantMB, gotMB, baseMB)
	}

	// Then the filesystem, which is a separate claim: `df` reports what
	// the guest can actually use, and that stays the image's own until
	// grain-growfs' resize2fs runs (scripts/kontur/guest-setup.sh). A
	// device at the right size with a filesystem still at the image's is
	// the failure that looks like the setting having done nothing.
	//
	// Field 1 of `df -Pk` is the 1024-block total; -P is what guarantees
	// one line per filesystem with the fields in that order, which is why
	// this package's own health reading uses it too (health.go).
	result := sizedRunCommand.Handler(context.Background(), map[string]any{"command": "df -Pk / | tail -1"})
	if result.IsError {
		t.Fatalf("reading the sized guest's root filesystem: %s", result.Text)
	}
	fields := strings.Fields(guestStdout(t, result.Text))
	if len(fields) < 2 {
		t.Fatalf("guest df line = %q, want at least a device and a block total", result.Text)
	}
	blocksKB, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("parsing the guest's df total %q out of %q: %v", fields[1], result.Text, err)
	}
	// Not the full request: ext4's metadata means a filesystem grown onto
	// an N MiB device never reports N. Nine tenths is well under that
	// overhead and well over the unresized image's own size, which is the
	// distinction being drawn.
	if grownMB := int(blocksKB / 1024); grownMB < wantMB*9/10 {
		t.Errorf("the sized guest's root filesystem is %d MiB on a %d MiB device, want it grown onto most of the disk -- grain-growfs' resize2fs never ran or failed (see the VM's console)",
			grownMB, wantMB)
	}
}

// guestRootDeviceMB is the size of /dev/vda as the guest sees it, in
// MiB. /sys/block/vda/size is in 512-byte sectors and readable by any
// account, unlike the device itself.
func guestRootDeviceMB(t *testing.T, runCommand *mcp.Tool) int {
	t.Helper()
	result := runCommand.Handler(context.Background(), map[string]any{"command": "cat /sys/block/vda/size"})
	if result.IsError {
		t.Fatalf("reading the guest's root device size: %s", result.Text)
	}
	sectors, err := strconv.ParseInt(guestStdout(t, result.Text), 10, 64)
	if err != nil {
		t.Fatalf("parsing /sys/block/vda/size out of %q: %v", result.Text, err)
	}
	return int(sectors * 512 / (1024 * 1024))
}

// guestStdout is the command's own stdout out of a run_command result,
// which reports the exit status and both streams together
// ("exit=%d\nstdout:\n%s\nstderr:\n%s", mcp.NewSSHSandboxTools). Every
// other assertion in this file matches on a substring of the whole thing
// and so never had to care; one that parses a number does.
func guestStdout(t *testing.T, text string) string {
	t.Helper()
	_, rest, ok := strings.Cut(text, "stdout:\n")
	if !ok {
		t.Fatalf("run_command result %q has no stdout section", text)
	}
	out, _, ok := strings.Cut(rest, "\nstderr:")
	if !ok {
		t.Fatalf("run_command result %q has no stderr section to bound stdout with", text)
	}
	return strings.TrimSpace(out)
}

func assertGuestHasEgress(t *testing.T, runCommand *mcp.Tool, vmName string) {
	t.Helper()

	// Ranged over rather than dot-accessed: a container with no legacy
	// single-network attachment has no .NetworkSettings.Gateway at all,
	// and the template would error out ("map has no entry for key")
	// rather than return empty -- the same trap kontur.DockerPodIP's own
	// template already hit against a real daemon (bwsalmon/agents#466).
	// The netns holder is the container docker addressed; the VM
	// container merely joined its namespace.
	gwOut, err := exec.Command("docker", "inspect", "-f",
		`{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}`,
		"kontur-vm-"+vmName+"-netns").CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect on the netns holder: %v\n%s", err, gwOut)
	}
	// Fatal rather than a skip: this suite creates its VMs on docker's
	// default bridge, which always has one, and a skip here would take
	// the rest of this test with it (t.SkipNow on the parent) while
	// reading like coverage -- the exact trade the real-vm job's own
	// "--- PASS" assertion exists to refuse.
	gateway := string(bytes.TrimSpace(gwOut))
	if gateway == "" {
		t.Fatalf("docker reports no gateway for this VM's network namespace, so the guest has nowhere to route out through at all")
	}

	result := runCommand.Handler(context.Background(), map[string]any{
		"command": "ip -4 route show default",
	})
	if result.IsError {
		t.Fatalf("reading the guest's default route: %s", result.Text)
	}
	if !strings.Contains(result.Text, "default via "+gateway) {
		t.Errorf("guest default route = %q, want it to carry %q -- without it the guest has no route off its own segment, and so no egress at all",
			strings.TrimSpace(result.Text), "default via "+gateway)
	}
}
