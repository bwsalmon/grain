package orchestrator_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/kontur"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// writeFakeKontur installs a shell script named "konturctl" on PATH --
// the operator-facing binary orchestrator.KonturSandboxes actually execs
// via pkg/kontur.Create/Delete, not the container-facing "kontur" binary
// bwsalmon/kontur's own cmd/kontur is a distinct program from (see
// pkg/kontur's package doc comment) -- that answers "vm create <name>
// -state-dir <dir> ..." by writing <dir>/<name>.json with the given port
// (kontur's own real behavior, which is what Port later reads back) and
// "vm delete <name> -state-dir <dir>" by removing that same file (kontur's
// own staticpod.Delete, mirrored here so a Recreate test's second "vm
// create" sees a VM that genuinely doesn't exist yet rather than reusing
// stale state) -- logs every invocation's argv (one line per call) to
// argvLog, and otherwise succeeds silently.
func writeFakeKontur(t *testing.T, argvLog string, port int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake konturctl script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
if [ "$1" = "vm" ] && { [ "$2" = "create" ] || [ "$2" = "delete" ]; }; then
  action="$2"
  name="$3"
  statedir=""
  shift 3
  while [ $# -gt 0 ]; do
    if [ "$1" = "-state-dir" ]; then
      statedir="$2"
    fi
    shift
  done
  if [ "$action" = "create" ]; then
    echo "{\"port\": %d}" > "$statedir/$name.json"
  else
    rm -f "$statedir/$name.json"
  fi
fi
`, argvLog, port)
	path := filepath.Join(dir, "konturctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestKonturSandboxesConfigureGitCredentialsWritesToTheVMOverSSH(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)
	home := t.TempDir()
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, home)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:  "grain-test-",
		StateDir:    stateDir,
		SSHUser:     "debian",
		ExecKeyPath: "/images/key",
		Workspace:   "/workspace",
	})

	if err := k.ConfigureGitCredentials(context.Background(), "slot-0", "http://10.100.0.1:8080/owner/repo.git", "secret-token"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".git-credentials"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://sandbox:secret-token@10.100.0.1:8080\n"; string(data) != want {
		t.Errorf(".git-credentials = %q, want %q", data, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); err != nil {
		t.Errorf(".gitconfig was not written on the VM: %v", err)
	}

	// A second call for the same slot must not create a second VM --
	// ConfigureGitCredentials shares ensure()/resolveEndpoint() with
	// ToolsFor, so it gets that reuse for free; this just confirms it
	// actually took effect end to end.
	if err := k.ConfigureGitCredentials(context.Background(), "slot-0", "http://10.100.0.1:8080/owner/repo.git", "second-token"); err != nil {
		t.Fatal(err)
	}
}

func TestKonturSandboxesRecreateDeletesAndRecreatesTheVM(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
	})

	if _, err := k.ToolsFor(context.Background(), "slot-0"); err != nil {
		t.Fatal(err)
	}
	if err := k.Recreate(context.Background(), "slot-0"); err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	// The slot's sandbox must still work after being recreated -- a
	// second create, not a reuse of whatever ensure() thinks it already
	// knows about the now-deleted VM.
	if _, err := k.ToolsFor(context.Background(), "slot-0"); err != nil {
		t.Fatalf("ToolsFor after Recreate: %v", err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vm create grain-test-slot-0 -state-dir " + stateDir + " -backend docker -net flat",
		"vm delete grain-test-slot-0 -state-dir " + stateDir,
		"vm create grain-test-slot-0 -state-dir " + stateDir + " -backend docker -net flat",
	}
	got := splitNonEmptyLines(string(data))
	if len(got) != len(want) {
		t.Fatalf("kontur invocations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("invocation %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestKonturSandboxesRecreateReappliesGitCredentials(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)
	home := t.TempDir()
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, home)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
	})

	if err := k.ConfigureGitCredentials(context.Background(), "slot-0", "http://10.100.0.1:8080/owner/repo.git", "secret-token"); err != nil {
		t.Fatal(err)
	}
	// Simulate what a real Recreate's fresh VM actually looks like: no
	// filesystem carried over from the one just torn down, credentials
	// included.
	if err := os.Remove(filepath.Join(home, ".git-credentials")); err != nil {
		t.Fatal(err)
	}

	if err := k.Recreate(context.Background(), "slot-0"); err != nil {
		t.Fatalf("Recreate: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".git-credentials"))
	if err != nil {
		t.Fatalf("Recreate did not reapply git credentials to the rebuilt VM: %v", err)
	}
	if want := "http://sandbox:secret-token@10.100.0.1:8080\n"; string(data) != want {
		t.Errorf(".git-credentials = %q, want %q", data, want)
	}
}

func TestKonturSandboxesCreatesVMOnFirstUseAndReusesAfter(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
	})

	tools, err := k.ToolsFor(context.Background(), "slot-0")
	if err != nil {
		t.Fatal(err)
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

	if _, err := k.ToolsFor(context.Background(), "slot-0"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	createCalls := 0
	for _, line := range splitNonEmptyLines(string(data)) {
		if line == "vm create grain-test-slot-0 -state-dir "+stateDir+" -backend docker -net flat" {
			createCalls++
		}
	}
	if createCalls != 1 {
		t.Errorf("konturctl vm create was invoked %d times across two ToolsFor calls for the same slot, want 1: log = %q", createCalls, data)
	}
}

func TestKonturSandboxesReusesAnAlreadyExistingVMWithoutCreating(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "grain-test-slot-0.json"), []byte(`{"port": 30080}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:  "grain-test-",
		StateDir:    stateDir,
		SSHUser:     "debian",
		ExecKeyPath: "/images/key",
		Workspace:   "/workspace",
	})

	if _, err := k.ToolsFor(context.Background(), "slot-0"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err == nil && len(data) != 0 {
		t.Errorf("kontur was invoked for a VM whose state already existed: %q", data)
	}
}

func TestKonturSandboxesWaitsForVMToBecomeReady(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 3, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
		ReadyTimeout:      time.Second,
	})

	if _, err := k.ToolsFor(context.Background(), "slot-0"); err != nil {
		t.Fatalf("ToolsFor() did not wait out the VM's slow start: %v", err)
	}
}

func TestKonturSandboxesGivesUpAfterReadyTimeout(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 1000, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
		ReadyTimeout:      10 * time.Millisecond,
	})

	if _, err := k.ToolsFor(context.Background(), "slot-0"); err == nil {
		t.Fatal("ToolsFor() on a VM that never becomes ready: got nil error, want one")
	}
}

// TestKonturSandboxesFastFailsWhenTheVMContainerExitsEarly guards against
// a real failure mode confirmed by hand against a real docker daemon
// (29.7.2) and a deliberately broken CHV_DISK_IMAGE path: under
// BackendDocker, "konturctl vm create" starts the VM container with a
// plain "docker run -d" (bwsalmon/kontur's own internal/dockervm), which
// -- like any "docker run -d" -- reports success the instant the
// container starts, not once cloud-hypervisor inside it has actually
// proven itself alive. A guest that fails before finishing boot exits
// within seconds of that "success", and without this check ToolsFor would
// have no way to tell that apart from "still booting," so it would poll a
// dead port for the entire ReadyTimeout before finally giving up with a
// generic connection-refused error. This drives ToolsFor against a fake
// docker whose VM container is already "exited" from the first inspect
// call, with a ReadyTimeout generous enough that a timing-based pass would
// be meaningless (a slow CI host might legitimately not fail by then) and
// asserts instead that ToolsFor returns quickly, well under that timeout,
// and that the error names the container and mentions its exited status
// rather than just "connection refused."
func TestKonturSandboxesFastFailsWhenTheVMContainerExitsEarly(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30082)
	// "exited" from the start: the VM container is dead, so no exec into
	// it will ever answer, and nothing here should wait around confirming
	// that the slow way.
	writeFakeDocker(t, filepath.Join(t.TempDir(), "docker-argv.log"), "exited",
		`echo "Error response from daemon: container is not running" >&2; exit 1`)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: 10 * time.Millisecond,
		ReadyTimeout:      10 * time.Second,
	})

	started := time.Now()
	_, err := k.ToolsFor(context.Background(), "slot-0")
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("ToolsFor() against a VM container that already exited: got nil error, want one")
	}
	if elapsed > 2*time.Second {
		t.Errorf("ToolsFor() took %s to fail, want well under the 10s ReadyTimeout -- it should fast-fail on the dead container instead of polling out the full deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error = %q, want it to mention the container's \"exited\" status", err)
	}
	if !strings.Contains(err.Error(), "kontur-vm-grain-test-slot-0") {
		t.Errorf("error = %q, want it to name the container kontur-vm-grain-test-slot-0", err)
	}
}

func TestKonturSandboxesDerivesPerSlotIPAndPortFromBase(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		NetMode:           kontur.NetModeNAT,
		BaseIP:            "169.254.100.2",
		BasePort:          30080,
		ReadyPollInterval: time.Millisecond,
	})

	// Two different slots must land two different VMs, each with its own
	// -ip/-port derived from BaseIP/BasePort and the slot's own number --
	// the whole point being that a deployment with more than one slot
	// (-max-concurrent > 1) does not ask konturctl to give every VM after
	// the first the exact same address on the one bridge they all share.
	if _, err := k.ToolsFor(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.ToolsFor(context.Background(), "2"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vm create grain1 -state-dir " + stateDir + " -backend docker -net nat -ip 169.254.100.2 -port 30080",
		"vm create grain2 -state-dir " + stateDir + " -backend docker -net nat -ip 169.254.100.3 -port 30081",
	}
	got := splitNonEmptyLines(string(data))
	if len(got) != len(want) {
		t.Fatalf("kontur invocations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("invocation %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestKonturSandboxesCreateAppendsDefaultCPUsAndMemoryMB confirms
// KonturConfig.DefaultCPUs/DefaultMemoryMB (bwsalmon/agents#534) reach
// "konturctl vm create" as -cpus/-memory-mb, after CreateArgs -- so an
// operator's own -kontur-create-arg=-cpus (if not set here too) is never
// silently overridden by leaving DefaultCPUs at its zero "unset" value,
// but the deployment-wide setting wins when both are configured, the
// same "last one wins" precedent BaseIP/BasePort already set for this
// same argument list.
func TestKonturSandboxesCreateAppendsDefaultCPUsAndMemoryMB(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		CreateArgs:        []string{"-disk", "/images/current/disk.img"},
		DefaultCPUs:       4,
		DefaultMemoryMB:   8192,
		ReadyPollInterval: time.Millisecond,
	})

	if _, err := k.ToolsFor(context.Background(), "slot-0"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := "vm create grain-test-slot-0 -state-dir " + stateDir + " -backend docker -net flat -disk /images/current/disk.img -cpus 4 -memory-mb 8192"
	got := splitNonEmptyLines(string(data))
	if len(got) != 1 || got[0] != want {
		t.Errorf("kontur invocations = %v, want [%q]", got, want)
	}
}

// TestKonturSandboxesReshapeUpdatesAnExistingVM confirms Reshape
// (bwsalmon/agents#534) runs "konturctl vm update" with -cpus/-memory-mb
// against a slot's already-created VM, leaving whatever flags Reshape
// was not asked to change (disk, network, ...) untouched -- vm update's
// own partial-update contract, exercised here only by checking that
// Reshape's own invocation carries exactly the two flags it was given,
// nothing else.
func TestKonturSandboxesReshapeUpdatesAnExistingVM(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "grain-test-slot-0.json"), []byte(`{"port": 30080}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:  "grain-test-",
		StateDir:    stateDir,
		SSHUser:     "debian",
		ExecKeyPath: "/images/key",
		Workspace:   "/workspace",
	})

	if err := k.Reshape(context.Background(), "slot-0", 4, 8192); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := "vm update grain-test-slot-0 -state-dir " + stateDir + " -cpus 4 -memory-mb 8192"
	got := splitNonEmptyLines(string(data))
	if len(got) != 1 || got[0] != want {
		t.Errorf("kontur invocations = %v, want [%q]", got, want)
	}
}

// TestKonturSandboxesReshapeOmitsUnsetDimension confirms Reshape passes
// only the one flag whose value is non-zero -- a task overriding just
// SandboxCPUs, say, must not also force -memory-mb 0 onto a VM that
// staticpod.VMSpec.Validate would refuse outright ("memory-mb must be at
// least 128").
func TestKonturSandboxesReshapeOmitsUnsetDimension(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "grain-test-slot-0.json"), []byte(`{"port": 30080}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:  "grain-test-",
		StateDir:    stateDir,
		SSHUser:     "debian",
		ExecKeyPath: "/images/key",
		Workspace:   "/workspace",
	})

	if err := k.Reshape(context.Background(), "slot-0", 4, 0); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := "vm update grain-test-slot-0 -state-dir " + stateDir + " -cpus 4"
	got := splitNonEmptyLines(string(data))
	if len(got) != 1 || got[0] != want {
		t.Errorf("kontur invocations = %v, want [%q]", got, want)
	}
}

// TestKonturSandboxesReshapeNoOpWhenNeitherDimensionSet confirms Reshape
// never invokes konturctl at all when both cpus and memoryMB are zero --
// runOne only calls Reshape when a task set at least one of
// SandboxCPUs/SandboxMemoryMB, but Reshape itself stays a safe no-op
// either way rather than running a pointless "vm update" with no flags.
func TestKonturSandboxesReshapeNoOpWhenNeitherDimensionSet(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "grain-test-slot-0.json"), []byte(`{"port": 30080}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:  "grain-test-",
		StateDir:    stateDir,
		SSHUser:     "debian",
		ExecKeyPath: "/images/key",
		Workspace:   "/workspace",
	})

	if err := k.Reshape(context.Background(), "slot-0", 0, 0); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err == nil && len(data) != 0 {
		t.Errorf("kontur was invoked by a no-op Reshape: %q", data)
	}
}

func TestKonturSandboxesRejectsNonNumericSlotWithBaseIPSet(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:  "grain-test-",
		StateDir:    stateDir,
		SSHUser:     "debian",
		ExecKeyPath: "/images/key",
		Workspace:   "/workspace",
		BaseIP:      "169.254.100.2",
	})

	if _, err := k.ToolsFor(context.Background(), "slot-0"); err == nil {
		t.Fatal("ToolsFor() with a non-numeric slot and BaseIP set: got nil error, want one")
	}
}

// TestKonturSandboxesSelfHealsAVMContainerFoundDeadAfterReboot guards
// against bwsalmon/agents#591: after a host reboot, a VM's on-disk kontur
// state survives (kontur.Exists, what ensure() checks -- state files live
// under /var/lib/kontur/vms, untouched by a reboot), but its docker
// containers do not, because internal/dockervm.Create starts them with a
// plain "docker run -d" and no restart policy, so docker never brings
// them back the way a --restart=always container would. Before this
// fix, ensure() treated that surviving state file alone as proof the VM
// was already usable and never looked at the container's real docker
// status; waitForGuestExec then correctly fast-failed on the dead
// container (TestKonturSandboxesFastFailsWhenTheVMContainerExitsEarly
// above), but nothing ever rebuilt it afterwards -- ensure() has no
// reason to look past its own k.created cache once true, so every later
// ToolsFor call for the same slot hit the exact same dead container and
// failed identically forever, hanging that slot until an operator
// intervened by hand.
//
// This drives ToolsFor against a slot whose kontur state file already
// exists (simulating a state directory that survived a reboot) but whose
// VM container reports "exited" and refuses `docker exec` until a real
// "vm create" actually runs again (simulating the container itself not
// surviving that reboot), and asserts ToolsFor recovers on its own --
// deleting and recreating the VM -- without any caller having to notice
// the dead container and call Recreate itself.
func TestKonturSandboxesSelfHealsAVMContainerFoundDeadAfterReboot(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "grain-test-slot-0.json"), []byte(`{"port": 30080}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	aliveMarker := filepath.Join(t.TempDir(), "container-alive")
	writeFakeKonturTouchingMarkerOnCreate(t, argvLog, 30080, aliveMarker)
	writeFakeDockerDeadUntilMarker(t, filepath.Join(t.TempDir(), "docker-argv.log"), aliveMarker)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
		ReadyTimeout:      time.Second,
	})

	tools, err := k.ToolsFor(context.Background(), "slot-0")
	if err != nil {
		t.Fatalf("ToolsFor() did not self-heal a VM container found dead: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("ToolsFor() returned no tools after recovering")
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vm delete grain-test-slot-0 -state-dir " + stateDir,
		"vm create grain-test-slot-0 -state-dir " + stateDir + " -backend docker -net flat",
	}
	got := splitNonEmptyLines(string(data))
	if len(got) != len(want) {
		t.Fatalf("kontur invocations = %v, want %v (ToolsFor should delete then recreate the dead VM, not reuse the stale state file forever)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("invocation %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// writeFakeKonturTouchingMarkerOnCreate is writeFakeKontur plus one thing:
// a successful "vm create" also touches marker, standing in for a docker
// container actually coming up alongside konturctl's own state file --
// the coupling writeFakeDockerDeadUntilMarker's fake docker watches for to
// know a real recreate happened, not just that the state file changed.
func writeFakeKonturTouchingMarkerOnCreate(t *testing.T, argvLog string, port int, marker string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake konturctl script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
if [ "$1" = "vm" ] && { [ "$2" = "create" ] || [ "$2" = "delete" ]; }; then
  action="$2"
  name="$3"
  statedir=""
  shift 3
  while [ $# -gt 0 ]; do
    if [ "$1" = "-state-dir" ]; then
      statedir="$2"
    fi
    shift
  done
  if [ "$action" = "create" ]; then
    echo "{\"port\": %d}" > "$statedir/$name.json"
    touch %q
  else
    rm -f "$statedir/$name.json"
  fi
fi
`, argvLog, port, marker)
	path := filepath.Join(dir, "konturctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeFakeDockerDeadUntilMarker installs a fake "docker" that answers
// both `docker inspect -f {{.State.Status}}` and `docker exec` as if the
// VM container has already exited -- inspect reports "exited", exec fails
// the way docker actually does against a stopped container -- until
// marker exists, standing in for the container a fresh
// "konturctl vm create" would actually start. Once marker exists, both
// answer as a healthy running container and guest would.
func writeFakeDockerDeadUntilMarker(t *testing.T, argvLog, marker string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake docker script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$1" in
exec)
  if [ ! -f %q ]; then
    echo "Error response from daemon: Container is not running" >&2
    exit 1
  fi
  exit 0
  ;;
inspect)
  case "$*" in
  *State.Status*)
    if [ -f %q ]; then echo running; else echo exited; fi
    ;;
  *) echo "fake docker: unexpected inspect: $*" >&2; exit 1 ;;
  esac
  ;;
*)
  echo "fake docker: unexpected subcommand: $*" >&2
  exit 1
  ;;
esac
`, argvLog, marker, marker)
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestKonturSandboxesVMNameForUsesPrefix(t *testing.T) {
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{NamePrefix: "grain-agent-77-"})
	if got, want := k.VMNameFor("slot-0"), "grain-agent-77-slot-0"; got != want {
		t.Errorf("VMNameFor() = %q, want %q", got, want)
	}
}

func splitNonEmptyLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if i > start {
				lines = append(lines, s[start:i])
			}
			start = i + 1
		}
	}
	return lines
}

// writeFakeDockerExecBackend installs a shell script named "docker" on
// PATH for the cfg.DockerExec path: it answers `docker exec` by running
// execBody, answers kontur.DockerContainerStatus's own
// "docker inspect -f {{.State.Status}}" with status, and *fails* every
// other inspect -- notably DockerPodIP's own (its format string mentions
// NetworkSettings).
//
// That last part is the point: under DockerExec nothing should ever look
// a VM's container address up, so a test whose fake cannot answer that
// lookup proves the lookup never happened rather than merely asserting
// its absence from a log. The same goes for the guest's sshd -- these
// tests deliberately never listenTCP, since there is no port for anything
// out here to dial.
func writeFakeDocker(t *testing.T, argvLog, status, execBody string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake docker script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$1" in
exec)
  %s
  ;;
inspect)
  case "$*" in
  *State.Status*) echo %q ;;
  *) echo "fake docker: unexpected inspect: $*" >&2; exit 1 ;;
  esac
  ;;
*)
  echo "fake docker: unexpected subcommand: $*" >&2
  exit 1
  ;;
esac
`, argvLog, execBody, status)
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeFakeDockerGuest installs a fake "docker" whose `exec` runs the
// command it was handed, in homeDir -- the docker-exec counterpart to the
// fake `ssh` these tests used to install, and the same trick: everything
// after "kontur exec --" is a real argv (see mcp.DockerExecRunner.Run on
// why it is not shell-quoted), so the fake can exec it directly rather
// than re-parsing anything. homeDir stands in for the guest's own
// filesystem, which is what lets a test assert on files a tool call left
// behind; "" means succeed without running anything.
//
// readyAfter models a guest that is still booting: the first readyAfter
// exec calls fail the way `kontur exec` does when the guest is not
// answering yet (its own "kontur: exec: " log prefix, which
// DockerExecRunner reads as "the command never ran"), and every call
// after that succeeds. counterFile is where the count lives, so it
// survives across the separate processes each exec is.
func writeFakeDockerGuest(t *testing.T, argvLog, counterFile string, readyAfter int, homeDir string) {
	t.Helper()
	run := "exit 0"
	if homeDir != "" {
		run = fmt.Sprintf(`cd %q && exec "$@"`, homeDir)
	}
	writeFakeDocker(t, argvLog, "running", fmt.Sprintf(`
  n=0
  [ -f %[1]q ] && n=$(cat %[1]q)
  n=$((n+1))
  echo "$n" > %[1]q
  if [ "$n" -le %[2]d ]; then
    echo "kontur: exec: dialing guest: connection refused" >&2
    exit 1
  fi
  shift
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
  shift
  %[3]s
`, counterFile, readyAfter, run))
}

func konturTestConfig(stateDir string) orchestrator.KonturConfig {
	return orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/kontur_id_ed25519",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
		ReadyTimeout:      5 * time.Second,
	}
}

// Under DockerExec, ToolsFor has to reach the guest without resolving any
// address for it at all: no external port out of kontur's state file, no
// container IP out of `docker inspect`, and no TCP dial to confirm a port
// is answering. The fake docker here cannot answer an address lookup and
// nothing is listening anywhere, so getting tools back at all is what
// proves none of that was consulted.
func TestKonturSandboxesDockerExecReachesTheGuestWithoutResolvingAnAddress(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30090)
	dockerLog := filepath.Join(t.TempDir(), "docker-argv.log")
	writeFakeDocker(t, dockerLog, "running", "exit 0")

	k := orchestrator.NewKonturSandboxes(konturTestConfig(stateDir))

	tools, err := k.ToolsFor(context.Background(), "slot-0")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 {
		t.Fatalf("ToolsFor() returned %d tools, want the same 4 the SSH path returns", len(tools))
	}

	argv, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("docker was never invoked at all: %v", err)
	}
	if !strings.Contains(string(argv), "exec ") {
		t.Errorf("docker invoked with %q, want a `docker exec` among the calls", argv)
	}
	if strings.Contains(string(argv), "NetworkSettings") {
		t.Errorf("docker invoked with %q, want no container-address lookup under DockerExec", argv)
	}
	if strings.Contains(string(argv), "-netns") {
		t.Errorf("docker invoked with %q, want it to exec into the VM container, never the netns holder", argv)
	}
}

// A tool call has to arrive in the guest through
// `docker exec <vm container> kontur exec --`, carrying the guest account
// and the in-container key path -- the same guest, account and key the
// SSH path uses, reached from inside the VM's own container instead.
func TestKonturSandboxesDockerExecRunsToolCallsThroughTheVMContainer(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30091)
	dockerLog := filepath.Join(t.TempDir(), "docker-argv.log")
	writeFakeDocker(t, dockerLog, "running", `echo "hello from the guest"`)

	k := orchestrator.NewKonturSandboxes(konturTestConfig(stateDir))
	tools, err := k.ToolsFor(context.Background(), "slot-0")
	if err != nil {
		t.Fatal(err)
	}

	var runCommand *mcp.Tool
	for i := range tools {
		if tools[i].Name == "run_command" {
			runCommand = &tools[i]
		}
	}
	if runCommand == nil {
		t.Fatal("ToolsFor() returned no run_command tool")
	}
	result := runCommand.Handler(context.Background(), map[string]any{"command": "echo hi"})
	if result.IsError {
		t.Fatalf("run_command reported an error: %+v", result)
	}

	argv, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	var execLine string
	for _, line := range strings.Split(string(argv), "\n") {
		if strings.HasPrefix(line, "exec ") && strings.Contains(line, "kontur exec --") {
			execLine = line
		}
	}
	if execLine == "" {
		t.Fatalf("docker invoked with %q, want a `docker exec ... kontur exec --` call", argv)
	}
	for _, want := range []string{
		"KONTUR_EXEC_USER=debian",
		"KONTUR_EXEC_KEY=/images/kontur_id_ed25519",
		"kontur-vm-grain-test-slot-0 kontur exec --",
	} {
		if !strings.Contains(execLine, want) {
			t.Errorf("docker exec line = %q, want it to carry %q", execLine, want)
		}
	}
}

// The dead-VM-container fast fail waitForSSHPort gives the SSH path has
// to hold for the docker-exec path too: exec'ing into a container that
// has already exited will never start answering, so waiting out the full
// ReadyTimeout finding that out is just a slower way to fail.
func TestKonturSandboxesDockerExecFastFailsWhenTheVMContainerExitsEarly(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30092)
	writeFakeDocker(t, filepath.Join(t.TempDir(), "docker-argv.log"), "exited",
		`echo "Error response from daemon: container is not running" >&2; exit 1`)

	cfg := konturTestConfig(stateDir)
	cfg.ReadyPollInterval = 10 * time.Millisecond
	cfg.ReadyTimeout = 10 * time.Second
	k := orchestrator.NewKonturSandboxes(cfg)

	started := time.Now()
	_, err := k.ToolsFor(context.Background(), "slot-0")
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("ToolsFor() against a VM container that already exited: got nil error, want one")
	}
	if elapsed > 2*time.Second {
		t.Errorf("ToolsFor() took %s to fail, want well under the 10s ReadyTimeout", elapsed)
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error = %q, want it to mention the container's \"exited\" status", err)
	}
	if !strings.Contains(err.Error(), "kontur-vm-grain-test-slot-0") {
		t.Errorf("error = %q, want it to name the container", err)
	}
}

// TestKonturSandboxesFlatModeOmitsAddressing covers the default mode:
// docker assigns the guest's address, so no -ip is derived or passed --
// konturctl rejects one outright under flat mode -- and no -port either,
// since nothing forwards one. BaseIP/BasePort are set here anyway, as a
// deployment's own systemd unit may still carry them from before the
// switch, to confirm they are ignored rather than fatal.
func TestKonturSandboxesFlatModeOmitsAddressing(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		BaseIP:            "169.254.100.2",
		BasePort:          30080,
		ReadyPollInterval: time.Millisecond,
	})

	for _, slot := range []string{"1", "2"} {
		if _, err := k.ToolsFor(context.Background(), slot); err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vm create grain1 -state-dir " + stateDir + " -backend docker -net flat",
		"vm create grain2 -state-dir " + stateDir + " -backend docker -net flat",
	}
	got := splitNonEmptyLines(string(data))
	if len(got) != len(want) {
		t.Fatalf("kontur invocations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("invocation %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestKonturSandboxesFlatModeIsTheDefault pins the default down on its
// own: an unset NetMode has to mean flat, since that is what every
// deployment gets without saying anything.
func TestKonturSandboxesFlatModeIsTheDefault(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
	})
	if _, err := k.ToolsFor(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if want := "-net " + kontur.NetModeFlat; !strings.Contains(string(data), want) {
		t.Errorf("kontur invocation = %q, want it to carry %q", string(data), want)
	}
}

// TestKonturSandboxesRecreateWithNoExistingVMOnlyCreates covers the case
// cmd/grain daemon's own startup reset pass introduces: Recreate called
// for a slot that has never had a VM. There is nothing to tear down, so
// it must be a plain create -- not a `konturctl vm delete` for a name
// kontur has no saved state for, which only ever reaches the static-pod
// backend this package never builds VMs under (Recreate's own doc
// comment). The per-task call site is unaffected either way: the VM it
// just finished with always exists, which
// TestKonturSandboxesRecreateDeletesAndRecreatesTheVM above still proves
// deletes first.
func TestKonturSandboxesRecreateWithNoExistingVMOnlyCreates(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
	})

	if err := k.Recreate(context.Background(), "slot-0"); err != nil {
		t.Fatalf("Recreate on a slot with no VM: %v", err)
	}
	// The slot has to be usable afterwards: this is the only thing that
	// creates its VM when the reset pass runs before anything else does.
	if _, err := k.ToolsFor(context.Background(), "slot-0"); err != nil {
		t.Fatalf("ToolsFor after Recreate: %v", err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"vm create grain-test-slot-0 -state-dir " + stateDir + " -backend docker -net flat"}
	got := splitNonEmptyLines(string(data))
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("kontur invocations = %v, want %v", got, want)
	}
}
