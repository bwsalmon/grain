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

	"github.com/bwsalmon/grain/pkg/dispatch"
	"github.com/bwsalmon/grain/pkg/kontur"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// writeFakeKontur installs a shell script named "konturctl" on PATH --
// the operator-facing binary orchestrator.KonturSandboxes actually execs
// via pkg/kontur.Create/Delete, not the container-facing "kontur" binary
// bwsalmon/kontur's own cmd/kontur is a distinct program from (see
// pkg/kontur's package doc comment) -- that answers "vm create <name>
// -state-dir <dir> ..." by writing <dir>/<name>.json with the given port
// (kontur's own real behavior, which is what Port later reads back) and
// "vm delete <name> -state-dir <dir>" by removing that same file (kontur's
// own staticpod.Delete, mirrored here so a test that rebuilds under a
// name sees, on its second "vm create", a VM that genuinely doesn't exist
// yet rather than reusing stale state) -- logs every invocation's argv (one line per call) to
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
		StateDir:    stateDir,
		SSHUser:     "debian",
		ExecKeyPath: "/images/key",
		Workspace:   "/workspace",
	})

	sb, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.ConfigureGitCredentials(context.Background(), "http://10.100.0.1:8080/owner/repo.git", "secret-token"); err != nil {
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
	// ConfigureGitCredentials runs over the runner Acquire already
	// resolved, so it needs no setup of its own; this just confirms it
	// actually took effect end to end.
	if err := sb.ConfigureGitCredentials(context.Background(), "http://10.100.0.1:8080/owner/repo.git", "second-token"); err != nil {
		t.Fatal(err)
	}
}

// The capability half of the same story: a kontur VM's only route for a
// gcp-key/gemini-key/github-sandbox placement is its own runner, and
// before konturSandbox implemented orchestrator.SandboxPlacer there was
// no route at all -- a task granting any of them failed during
// preparation on every -kontur-sandboxes deployment. The mode matters as
// much as the content: what lands here is a live service-account key.
func TestKonturSandboxesPlaceFileWritesACredentialToTheVMOverSSH(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30081)
	home := t.TempDir()
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, home)

	k := orchestrator.NewKonturSandboxes(konturTestConfig(stateDir))
	sb, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{})
	if err != nil {
		t.Fatal(err)
	}

	// The interface assertion is the point: runOne finds this route by
	// type-asserting the Sandbox it was handed, so a konturSandbox that
	// stopped satisfying it would silently go back to placing nothing.
	placer, ok := sb.(orchestrator.SandboxPlacer)
	if !ok {
		t.Fatal("a kontur sandbox does not implement orchestrator.SandboxPlacer, so its placements have nowhere to land")
	}

	// An absolute path under the fake guest's own home, standing in for
	// gcpkey.SandboxKeyPath's /home/debian/.gcp-service-account.json --
	// the real one would write outside this test's temp directories.
	target := filepath.Join(home, ".gcp-service-account.json")
	const key = `{"type":"service_account","project_id":"grain"}`
	if err := placer.PlaceFile(context.Background(), target, key, "600"); err != nil {
		t.Fatalf("PlaceFile: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the key never reached the VM: %v", err)
	}
	if string(got) != key {
		t.Errorf("key on the VM = %q, want %q", got, key)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key mode on the VM = %04o, want 0600 -- \"readable only by you\" is what the prompt tells the agent", perm)
	}
}

// A failure to place has to name the VM, not just the path: an operator
// reading a failed run's reason is looking at one slot out of a fleet.
func TestKonturSandboxesPlaceFileReportsFailureAgainstTheVMName(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30082)
	home := t.TempDir()
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, home)

	k := orchestrator.NewKonturSandboxes(konturTestConfig(stateDir))
	sb, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{})
	if err != nil {
		t.Fatal(err)
	}
	placer := sb.(orchestrator.SandboxPlacer)

	occupied := filepath.Join(home, "occupied")
	if err := os.Mkdir(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	err = placer.PlaceFile(context.Background(), occupied, "material", "600")
	if err == nil {
		t.Fatal("PlaceFile reported success for a placement that cannot have landed")
	}
	if vm := sb.(interface{ VMName() string }).VMName(); !strings.Contains(err.Error(), vm) {
		t.Errorf("error = %v, want it to name the VM %q", err, vm)
	}
}

func TestKonturSandboxesAcquireCreatesAVMAndReleaseDeletesIt(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
	})

	sb, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Name() != "t1-1" {
		t.Errorf("Name() = %q, want the sandbox's own name", sb.Name())
	}

	tools, err := sb.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantNames := map[string]bool{"run_command": true, "read_file": true, "write_file": true, "edit_file": true}
	if len(tools) != len(wantNames) {
		t.Fatalf("Tools() returned %d tools, want %d", len(tools), len(wantNames))
	}
	for _, tool := range tools {
		if !wantNames[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
	}

	if err := sb.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitNonEmptyLines(string(data))
	wantCreate := "vm create g-t1-1 -state-dir " + stateDir + " -backend docker -net flat"
	wantDelete := "vm delete g-t1-1 -state-dir " + stateDir
	var creates, deletes int
	for _, line := range lines {
		switch line {
		case wantCreate:
			creates++
		case wantDelete:
			deletes++
		}
	}
	if creates != 1 || deletes != 1 {
		t.Errorf("konturctl calls = %q, want exactly one create and one delete", lines)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "g-t1-1.json")); err == nil {
		t.Error("VM state still exists after Release, want it deleted")
	}
}

// A VM already sitting under the name a run is about to use is deleted
// and rebuilt, not adopted. Adopting one was right while a name meant a
// slot -- "reuse what's there" -- but a name means a run now, so anything
// already wearing it is a leftover whose filesystem this run must not
// start from.
func TestKonturSandboxesAcquireRebuildsAVMLeftUnderTheSameName(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "g-t1-1.json"), []byte(`{"port": 30080}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
	})

	if _, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitNonEmptyLines(string(data))
	if len(lines) < 2 || lines[0] != "vm delete g-t1-1 -state-dir "+stateDir {
		t.Fatalf("konturctl calls = %q, want the stale VM deleted before anything else", lines)
	}
	if lines[1] != "vm create g-t1-1 -state-dir "+stateDir+" -backend docker -net flat" {
		t.Errorf("konturctl calls = %q, want a create straight after the delete", lines)
	}
}

// ReapOrphans is the startup half of the same rule: at startup no VM can
// belong to this process, so every one under this deployment's prefix is
// a leftover. VMs under another prefix are left alone.
func TestKonturSandboxesReapOrphansDeletesOnlyItsOwnPrefix(t *testing.T) {
	stateDir := t.TempDir()
	for _, name := range []string{"g-t1-1", "g-t2-1", "other-vm"} {
		if err := os.WriteFile(filepath.Join(stateDir, name+".json"), []byte(`{"port": 30080}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir: stateDir, SSHUser: "debian",
		ExecKeyPath: "/images/key", Workspace: "/workspace",
	})

	reaped, err := k.ReapOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if reaped != 2 {
		t.Errorf("reaped = %d, want 2", reaped)
	}
	for _, name := range []string{"g-t1-1", "g-t2-1"} {
		if _, err := os.Stat(filepath.Join(stateDir, name+".json")); err == nil {
			t.Errorf("%s survived the reap", name)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "other-vm.json")); err != nil {
		t.Error("a VM under another prefix was reaped, want it left alone")
	}
}

func TestKonturSandboxesWaitsForVMToBecomeReady(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 3, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
		ReadyTimeout:      time.Second,
	})

	if _, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err != nil {
		t.Fatalf("Acquire() did not wait out the VM's slow start: %v", err)
	}
}

func TestKonturSandboxesGivesUpAfterReadyTimeout(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 1000, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
		ReadyTimeout:      10 * time.Millisecond,
	})

	if _, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err == nil {
		t.Fatal("Acquire() on a VM that never becomes ready: got nil error, want one")
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
// within seconds of that "success", and without this check Acquire would
// have no way to tell that apart from "still booting," so it would poll a
// dead port for the entire ReadyTimeout before finally giving up with a
// generic connection-refused error. This drives Acquire against a fake
// docker whose VM container is already "exited" from the first inspect
// call, with a ReadyTimeout generous enough that a timing-based pass would
// be meaningless (a slow CI host might legitimately not fail by then) and
// asserts instead that Acquire returns quickly, well under that timeout,
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
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: 10 * time.Millisecond,
		ReadyTimeout:      10 * time.Second,
	})

	started := time.Now()
	_, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("Acquire() against a VM container that already exited: got nil error, want one")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Acquire() took %s to fail, want well under the 10s ReadyTimeout -- it should fast-fail on the dead container instead of polling out the full deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error = %q, want it to mention the container's \"exited\" status", err)
	}
	if !strings.Contains(err.Error(), "kontur-vm-g-t1-1") {
		t.Errorf("error = %q, want it to name the container kontur-vm-g-t1-1", err)
	}
}

// Under NAT mode every VM is created with the same -ip/-port, passed
// verbatim. These used to be a *base* that each slot's own number was
// added to, on the reasoning that concurrent VMs shared one bridge; under
// the docker backend each VM has its own network namespace (its own
// netns-holder container), so they share no bridge and cannot collide.
func TestKonturSandboxesPassesIPAndPortVerbatimUnderNAT(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		NetMode:           kontur.NetModeNAT,
		IP:                "169.254.100.2",
		Port:              30080,
		ReadyPollInterval: time.Millisecond,
	})

	if _, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Acquire(context.Background(), "t2-1", orchestrator.Shape{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vm create g-t1-1 -state-dir " + stateDir + " -backend docker -net nat -ip 169.254.100.2 -port 30080",
		"vm create g-t2-1 -state-dir " + stateDir + " -backend docker -net nat -ip 169.254.100.2 -port 30080",
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

// TestKonturSandboxesCreateAppendsDefaultShape confirms
// KonturConfig.DefaultCPUs/DefaultMemoryMB/DefaultDiskGB
// (bwsalmon/agents#534, grain/task-41) reach "konturctl vm create" as
// -cpus/-memory-mb/-disk-size-gb, after CreateArgs -- so an
// operator's own -kontur-create-arg=-cpus (if not set here too) is never
// silently overridden by leaving DefaultCPUs at its zero "unset" value,
// but the deployment-wide setting wins when both are configured, the
// same "last one wins" precedent IP/Port already set for this same
// argument list. A run's own Shape overrides them per dimension.
func TestKonturSandboxesCreateAppendsDefaultShape(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		CreateArgs:        []string{"-disk", "/images/current/disk.img"},
		DefaultCPUs:       4,
		DefaultMemoryMB:   8192,
		DefaultDiskGB:     40,
		ReadyPollInterval: time.Millisecond,
	})

	if _, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := "vm create g-t1-1 -state-dir " + stateDir + " -backend docker -net flat -disk /images/current/disk.img -cpus 4 -memory-mb 8192 -disk-size-gb 40"
	got := splitNonEmptyLines(string(data))
	if len(got) != 1 || got[0] != want {
		t.Errorf("kontur invocations = %v, want [%q]", got, want)
	}
}

// SetDefaultShape is the same setting, changed while the daemon runs:
// model.Config.SandboxCPUs/SandboxMemoryMB/SandboxDiskGB edited in
// Settings reach the next sandbox built rather than only the next
// process (cmd/grain/daemon.go's liveConfig calls this once per
// reconcile tick when any of them has moved). A run that asked for a
// shape of its own still wins, per dimension, exactly as it does over
// the constructor's value.
func TestKonturSandboxesSetDefaultShapeAppliesToTheNextCreate(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		DefaultCPUs:       2,
		DefaultMemoryMB:   2048,
		ReadyPollInterval: time.Millisecond,
	})
	k.SetDefaultShape(orchestrator.Shape{CPUs: 8, MemoryMB: 16384, DiskGB: 40})

	if _, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err != nil {
		t.Fatal(err)
	}
	// And a run with its own request, to prove the new default is still
	// only a fallback: the task's CPUs win, the deployment's memory and
	// disk fill in the dimensions it left alone.
	if _, err := k.Acquire(context.Background(), "t2-1", orchestrator.Shape{CPUs: 1}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vm create g-t1-1 -state-dir " + stateDir + " -backend docker -net flat -cpus 8 -memory-mb 16384 -disk-size-gb 40",
		"vm create g-t2-1 -state-dir " + stateDir + " -backend docker -net flat -cpus 1 -memory-mb 16384 -disk-size-gb 40",
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

// Clearing every dimension back to "unset" has to reach the backend too
// -- otherwise a deployment that went back to kontur's own default VM
// size would keep building the size it had been told to forget.
func TestKonturSandboxesSetDefaultShapeCanClearTheDefault(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		DefaultCPUs:       4,
		DefaultMemoryMB:   8192,
		DefaultDiskGB:     40,
		ReadyPollInterval: time.Millisecond,
	})
	k.SetDefaultShape(orchestrator.Shape{})

	if _, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := "vm create g-t1-1 -state-dir " + stateDir + " -backend docker -net flat"
	got := splitNonEmptyLines(string(data))
	if len(got) != 1 || got[0] != want {
		t.Errorf("kontur invocations = %v, want [%q]", got, want)
	}
}

func TestKonturSandboxesVMNameForUsesPrefix(t *testing.T) {
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{})
	got, err := k.VMNameFor("t1-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "g-t1-1"; got != want {
		t.Errorf("VMNameFor() = %q, want %q", got, want)
	}
}

// The whole VM name has to fit 11 bytes: netshim derives "tap-<name>" and
// "ctl-<name>" from it, and Linux caps an interface name at 15. Catching
// it here says what the budget is and what is spending it, rather than
// letting `konturctl vm create` refuse a tap device name several layers
// down.
func TestKonturSandboxesVMNameForRejectsAnOverLongName(t *testing.T) {
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{})
	// The prefix is a constant, so the only thing that can overrun the
	// budget now is the run id itself -- a task id that has grown past
	// what maxRunNameLen leaves for it.
	if _, err := k.VMNameFor("1234567-12"); err == nil {
		t.Fatal("expected VMNameFor to refuse a name over the tap-device budget")
	}

	// Exactly at the budget is fine.
	if _, err := k.VMNameFor("123456-12"); err != nil {
		t.Errorf("VMNameFor on an 11-byte name: %v", err)
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

// writeFakeDocker installs a shell script named "docker" on PATH for the
// docker-exec transport: it answers `docker exec` by running execBody,
// answers kontur.DockerContainerStatus's own
// "docker inspect -f {{.State.Status}}" with status, and *fails* every
// other inspect -- notably any asking for NetworkSettings, which is how a
// container address lookup is spelled.
//
// That last part is the point: nothing should ever look a VM's container
// address up, so a test whose fake cannot answer that lookup proves the
// lookup never happened rather than merely asserting its absence from a
// log. The same goes for the guest's sshd -- these
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
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/kontur_id_ed25519",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
		ReadyTimeout:      5 * time.Second,
	}
}

// Under DockerExec, Acquire has to reach the guest without resolving any
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

	sb, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := sb.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 {
		t.Fatalf("Tools() returned %d tools, want the same 4 the SSH path returns", len(tools))
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
	sb, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := sb.Tools(context.Background())
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
		t.Fatal("Tools() returned no run_command tool")
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
		"kontur-vm-g-t1-1 kontur exec --",
	} {
		if !strings.Contains(execLine, want) {
			t.Errorf("docker exec line = %q, want it to carry %q", execLine, want)
		}
	}
}

// Acquire has to fast-fail on a dead VM container rather than wait:
// exec'ing into a container that has already exited will never start
// answering, so waiting out the full ReadyTimeout finding that out is
// just a slower way to fail.
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
	_, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("Acquire() against a VM container that already exited: got nil error, want one")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Acquire() took %s to fail, want well under the 10s ReadyTimeout", elapsed)
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error = %q, want it to mention the container's \"exited\" status", err)
	}
	if !strings.Contains(err.Error(), "kontur-vm-g-t1-1") {
		t.Errorf("error = %q, want it to name the container", err)
	}
}

// TestKonturSandboxesFlatModeOmitsAddressing covers the default mode:
// docker assigns the guest's address, so no -ip is derived or passed --
// konturctl rejects one outright under flat mode -- and no -port either,
// since nothing forwards one. IP/Port are set here anyway, as a
// deployment's own systemd unit may still carry them from before the
// switch, to confirm they are ignored rather than fatal.
func TestKonturSandboxesFlatModeOmitsAddressing(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"), filepath.Join(t.TempDir(), "counter"), 0, "")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		IP:                "169.254.100.2",
		Port:              30080,
		ReadyPollInterval: time.Millisecond,
	})

	for _, sandbox := range []string{"t1-1", "t2-1"} {
		if _, err := k.Acquire(context.Background(), sandbox, orchestrator.Shape{}); err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vm create g-t1-1 -state-dir " + stateDir + " -backend docker -net flat",
		"vm create g-t2-1 -state-dir " + stateDir + " -backend docker -net flat",
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
		StateDir:          stateDir,
		SSHUser:           "debian",
		ExecKeyPath:       "/images/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
	})
	if _, err := k.Acquire(context.Background(), "1", orchestrator.Shape{}); err != nil {
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

// The VM-name budget is now settled entirely in code -- a constant prefix
// and a constant cap -- so what is worth pinning is not that a check
// rejects a bad prefix but that the constants leave room for the run ids a
// real deployment actually produces. Task ids are a monotonically
// increasing counter (Store.NewTaskID), so this is the ceiling a
// long-lived deployment climbs toward: the two halves together get nine
// bytes, one of which the "-" spends, so eight digits of task id and
// attempt combined. Dropping the "r" dispatch.RunID used to carry bought
// exactly one of those digits.
func TestVMNameBudgetCoversRealisticRunIDs(t *testing.T) {
	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{})
	for _, tc := range []struct {
		taskID  string
		attempt int
		want    bool
		why     string
	}{
		{"1", 1, true, "the very first run a deployment ever makes"},
		{"99999", 99, true, "a five-digit task id on its 99th attempt"},
		{"999999", 99, true, "a six-digit task id on its 99th attempt, exactly at the budget"},
		{"9999999", 99, false, "a seven-digit task id on its 99th attempt is one byte over"},
		{"99999999", 1, false, "an eight-digit task id is over even on its first attempt"},
	} {
		name := dispatch.RunID(tc.taskID, tc.attempt)
		_, err := k.VMNameFor(name)
		if got := err == nil; got != tc.want {
			t.Errorf("VMNameFor(%q) fits = %v, want %v -- %s (err: %v)", name, got, tc.want, tc.why, err)
		}
	}
}

// A cancelled Acquire still tears its half-built VM down. kontur.Delete
// execs konturctl through exec.CommandContext, so cleanup that rode on
// the caller's own ctx did not merely fail on an already-cancelled one --
// it never ran, and the VM was left behind with nothing holding a handle
// to it until the next startup's ReapOrphans.
//
// This is the ordinary interruption, not a rare one: runOne's ctx is
// cancelled whenever the daemon is shutting down or the task was closed
// mid-run, which is exactly why konturSandbox.Release already detaches.
func TestKonturSandboxesAcquireDeletesTheVMWhenTheContextIsCancelled(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)
	// A guest that never answers, so Acquire is still waiting when ctx dies.
	writeFakeDockerGuest(t, filepath.Join(t.TempDir(), "docker-argv.log"),
		filepath.Join(t.TempDir(), "counter"), 1<<30, "")

	cfg := konturTestConfig(stateDir)
	cfg.ReadyTimeout = 30 * time.Second
	cfg.ReadyPollInterval = time.Millisecond
	k := orchestrator.NewKonturSandboxes(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	if _, err := k.Acquire(ctx, "t1-1", orchestrator.Shape{}); err == nil {
		t.Fatal("expected Acquire to fail once its context was cancelled")
	}
	if kontur.Exists(stateDir, "g-t1-1") {
		t.Error("the VM is still there after a cancelled Acquire -- cleanup has to run on a " +
			"context detached from the caller's, or `konturctl vm delete` never executes at all")
	}
}

// The same holds when the create itself fails: konturctl brings a VM's
// netns holder and its own container up in separate steps, so a failure
// between them leaves a half-built VM that no Release will ever reach.
func TestKonturSandboxesAcquireDeletesTheVMWhenCreateFails(t *testing.T) {
	stateDir := t.TempDir()
	dir := t.TempDir()
	// A konturctl that writes the state file and *then* fails the create,
	// standing in for a create that got partway and gave up.
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "vm" ] && [ "$2" = "create" ]; then
  echo '{"port": 1}' > %q/"$3".json
  echo "boom" >&2
  exit 1
fi
if [ "$1" = "vm" ] && [ "$2" = "delete" ]; then
  rm -f %q/"$3".json
fi
`, stateDir, stateDir)
	path := filepath.Join(dir, "konturctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	k := orchestrator.NewKonturSandboxes(konturTestConfig(stateDir))
	if _, err := k.Acquire(context.Background(), "t1-1", orchestrator.Shape{}); err == nil {
		t.Fatal("expected Acquire to fail when create does")
	}
	if kontur.Exists(stateDir, "g-t1-1") {
		t.Error("a failed create left its VM behind")
	}
}
