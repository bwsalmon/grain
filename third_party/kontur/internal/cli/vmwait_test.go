package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/kontur/internal/staticpod"
)

// countCalls returns how many of the recorded docker invocations start
// with verb.
func countCalls(calls [][]string, verb string) int {
	n := 0
	for _, c := range calls {
		if len(c) > 0 && c[0] == verb {
			n++
		}
	}
	return n
}

// TestVMWait_BlocksUntilTheGuestAnswers is what callers used to write a
// shell loop around "kontur exec -- true" for: the VM's containers are
// started well before its guest can answer anything, and this is the wait
// for that difference.
func TestVMWait_BlocksUntilTheGuestAnswers(t *testing.T) {
	withFakeDocker(t)
	stateDir := createDockerVM(t, "web")
	calls := callLog(t)
	// One refused probe, then an answer: a guest that is still booting.
	t.Setenv("FAKEDOCKER_PROBE_FAIL", "1")

	stdout, stderr, err := runVMArgs(t, "wait", "web", "--state-dir", stateDir)
	if err != nil {
		t.Fatalf("wait error = %v, stderr = %s", err, stderr)
	}
	if !strings.Contains(stdout, "ready") {
		t.Errorf("stdout = %q, want it to say the VM became ready", stdout)
	}
	if n := countCalls(calls(), "exec"); n < 2 {
		t.Errorf("%d probes, want the wait to have retried the one that was refused", n)
	}
}

// TestVMWait_TimesOutNamingTheContainer covers the other end: a guest
// that never answers has to fail with something a caller can act on, not
// hang.
func TestVMWait_TimesOutNamingTheContainer(t *testing.T) {
	withFakeDocker(t)
	stateDir := createDockerVM(t, "web")
	callLog(t)
	t.Setenv("FAKEDOCKER_PROBE_FAIL", "-1")

	_, _, err := runVMArgs(t, "wait", "web", "--state-dir", stateDir, "--timeout", "1ms")
	if err == nil {
		t.Fatal("wait on a guest that never answers = nil error, want a timeout")
	}
	if !strings.Contains(err.Error(), "kontur-vm-web") {
		t.Errorf("error = %v, want it to name the container to read logs from", err)
	}
}

func TestVMWait_UnknownVM(t *testing.T) {
	withFakeDocker(t)
	stateDir := filepath.Join(t.TempDir(), "state")

	_, _, err := runVMArgs(t, "wait", "nosuch", "--state-dir", stateDir)
	if err == nil {
		t.Fatal("wait on an unknown VM = nil error, want one")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to say the VM wasn't found", err)
	}
}

// TestVMWait_StaticPodBackendPointsAtCrictl: readiness is decided by
// running a command in the guest, so waiting can only work where exec
// does -- and says so the same way exec does rather than hanging or
// pretending.
func TestVMWait_StaticPodBackendPointsAtCrictl(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")
	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}

	_, _, err := runVMArgs(t, "wait", "web", "--state-dir", stateDir)
	if err == nil {
		t.Fatal("wait on a static-pod VM = nil error, want one")
	}
	if !strings.Contains(err.Error(), "crictl") {
		t.Errorf("error = %v, want it to point at crictl", err)
	}
}

// TestVMCreate_WaitBlocksForTheGuest is the same wait folded into the
// create, which is what makes a create followed by an exec a script
// rather than a poll loop.
func TestVMCreate_WaitBlocksForTheGuest(t *testing.T) {
	withFakeDocker(t)
	calls := callLog(t)
	t.Setenv("FAKEDOCKER_PROBE_FAIL", "1")
	stateDir := filepath.Join(t.TempDir(), "state")

	stdout, stderr, err := runVMArgs(t, "create", "web",
		"--backend", "docker",
		"--state-dir", stateDir,
		"--wait",
	)
	if err != nil {
		t.Fatalf("create -wait error = %v, stderr = %s", err, stderr)
	}
	if !strings.Contains(stdout, "ready") {
		t.Errorf("stdout = %q, want it to say the VM became ready", stdout)
	}
	if n := countCalls(calls(), "exec"); n < 2 {
		t.Errorf("%d probes, want -wait to have retried the one that was refused", n)
	}
	// The VM is a real one either way: -wait only decides when the
	// command returns.
	if _, err := staticpod.Load(stateDir, "web"); err != nil {
		t.Errorf("saved state not found after create -wait: %v", err)
	}
}

// TestVMCreate_WaitOnStaticPodFailsBeforeCreating: a -wait konturctl
// cannot honour is refused up front, so there is no half-made VM to clean
// up after the refusal.
func TestVMCreate_WaitOnStaticPodFailsBeforeCreating(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")

	_, _, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
		"--wait",
	)
	if err == nil {
		t.Fatal("create -wait on the static-pod backend = nil error, want one")
	}
	if entries, _ := os.ReadDir(podDir); len(entries) != 0 {
		t.Errorf("a manifest was written despite the refusal: %v", entries)
	}
	if _, err := staticpod.Load(stateDir, "web"); err == nil {
		t.Error("state was saved despite the refusal")
	}
}
