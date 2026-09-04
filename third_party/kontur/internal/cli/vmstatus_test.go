package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestVMStatus_ReadyGuest is the question "vm wait" only answers by
// blocking: asked once, answered now, and answered through the exit
// status too so a script can branch on it.
func TestVMStatus_ReadyGuest(t *testing.T) {
	withFakeDocker(t)
	stateDir := createDockerVM(t, "web")
	callLog(t)

	stdout, stderr, err := runVMArgs(t, "status", "web", "--state-dir", stateDir)
	if err != nil {
		t.Fatalf("status error = %v, stderr = %s", err, stderr)
	}
	for _, want := range []string{"kontur-vm-web", "running", "ready"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to mention %q", stdout, want)
		}
	}
}

// A guest that is still booting is an answer, not a failure -- but it is
// a non-zero one, so "konturctl vm status web && ..." doesn't run the
// rest against a VM that cannot take it yet.
func TestVMStatus_GuestNotUpYet(t *testing.T) {
	withFakeDocker(t)
	stateDir := createDockerVM(t, "web")
	callLog(t)
	t.Setenv("FAKEDOCKER_PROBE_FAIL", "-1")

	stdout, _, err := runVMArgs(t, "status", "web", "--state-dir", stateDir)
	var status *exitStatusError
	if !errors.As(err, &status) {
		t.Fatalf("status error = %v, want a non-zero exit status carrying no message of its own", err)
	}
	if status.code != 1 {
		t.Errorf("exit status = %d, want 1", status.code)
	}
	if !strings.Contains(stdout, "not ready") {
		t.Errorf("stdout = %q, want it to say the guest is not ready", stdout)
	}
	// Why it isn't ready is the whole reason to ask without waiting.
	if !strings.Contains(stdout, "not reachable yet") {
		t.Errorf("stdout = %q, want the probe's own reason in it", stdout)
	}
}

// It has to ask once and return: a status that quietly waited out a boot
// would be "vm wait" under another name.
func TestVMStatus_AsksOnceRatherThanWaiting(t *testing.T) {
	withFakeDocker(t)
	stateDir := createDockerVM(t, "web")
	calls := callLog(t)
	t.Setenv("FAKEDOCKER_PROBE_FAIL", "-1")

	if _, _, err := runVMArgs(t, "status", "web", "--state-dir", stateDir); err == nil {
		t.Fatal("status on a guest that never answers = nil error, want a non-zero status")
	}
	if n := countCalls(calls(), "exec"); n != 1 {
		t.Errorf("%d probes, want exactly 1: status asks, it doesn't wait", n)
	}
}

// A container that exited has no guest to ask through, and the answer
// has to point at the logs rather than at the guest.
func TestVMStatus_ContainerNotRunning(t *testing.T) {
	withFakeDocker(t)
	stateDir := createDockerVM(t, "web")
	callLog(t)
	t.Setenv("FAKEDOCKER_RUNNING", "false")

	stdout, _, err := runVMArgs(t, "status", "web", "--state-dir", stateDir)
	if err == nil {
		t.Fatal("status of a VM whose container exited = nil error, want a non-zero status")
	}
	if !strings.Contains(stdout, "not running") {
		t.Errorf("stdout = %q, want it to say the container is not running", stdout)
	}
	if !strings.Contains(stdout, "docker logs kontur-vm-web") {
		t.Errorf("stdout = %q, want it to point at the container's logs", stdout)
	}
}

func TestVMStatus_UnknownVM(t *testing.T) {
	withFakeDocker(t)
	stateDir := filepath.Join(t.TempDir(), "state")

	_, _, err := runVMArgs(t, "status", "nosuch", "--state-dir", stateDir)
	if err == nil {
		t.Fatal("status of an unknown VM = nil error, want one")
	}
	// Not an exitStatusError: the question could not be asked, which is a
	// konturctl failure rather than an answer of "no".
	var status *exitStatusError
	if errors.As(err, &status) {
		t.Errorf("error = %v, want a reported failure rather than a bare exit status", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to say the VM wasn't found", err)
	}
}

// Same limitation as exec and wait, said the same way: readiness is
// decided by running a command in the guest, and there is no way into a
// static-pod VM's container that konturctl can drive.
func TestVMStatus_StaticPodBackendPointsAtCrictl(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	podDir := filepath.Join(t.TempDir(), "manifests")
	if _, stderr, err := runVMArgs(t, "create", "web",
		"--disk", "/images/disk.img",
		"--state-dir", stateDir,
		"--static-pod-path", podDir,
	); err != nil {
		t.Fatalf("create error = %v, stderr = %s", err, stderr)
	}

	_, _, err := runVMArgs(t, "status", "web", "--state-dir", stateDir)
	if err == nil {
		t.Fatal("status of a static-pod VM = nil error, want one")
	}
	if !strings.Contains(err.Error(), "crictl") {
		t.Errorf("error = %v, want it to point at crictl", err)
	}
}
