package orchestrator_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// writeFakeKontur installs a shell script named "kontur" on PATH that
// answers "vm create <name> -state-dir <dir> ..." by writing
// <dir>/<name>.json with the given port (kontur's own real behavior,
// which is what Port later reads back), logs every invocation's argv (one
// line per call) to argvLog, and otherwise succeeds silently.
func writeFakeKontur(t *testing.T, argvLog string, port int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake kontur script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
if [ "$1" = "vm" ] && [ "$2" = "create" ]; then
  name="$3"
  statedir=""
  shift 3
  while [ $# -gt 0 ]; do
    if [ "$1" = "-state-dir" ]; then
      statedir="$2"
    fi
    shift
  done
  echo "{\"port\": %d}" > "$statedir/$name.json"
fi
`, argvLog, port)
	path := filepath.Join(dir, "kontur")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeFakeCrictl installs a shell script named "crictl" on PATH that
// answers a ready pod on `pods`/`inspectp` once readyAfter prior calls
// have already happened (tracked via a counter file), letting a test
// exercise KonturSandboxes' wait-for-ready retry loop.
func writeFakeCrictl(t *testing.T, counterFile string, readyAfter int, ip string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake crictl script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *pods*)
    n=0
    if [ -f %q ]; then n=$(cat %q); fi
    n=$((n + 1))
    echo "$n" > %q
    if [ "$n" -le %d ]; then
      echo '{"items":[]}'
    else
      echo '{"items":[{"id":"abc123"}]}'
    fi
    ;;
  *inspectp*)
    echo '{"status":{"network":{"ip":"%s"}}}'
    ;;
esac
`, counterFile, counterFile, counterFile, readyAfter, ip)
	path := filepath.Join(dir, "crictl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeFakeSSH installs a shell script named "ssh" on PATH that ignores
// every connection flag SSHRunner.Run passes and just runs its trailing
// shell-quoted command (SSHRunner.Run's own doc comment: "one shell-
// quoted string") against homeDir, standing in for "the directory a real
// SSH session starts a fresh login in" (ssh_tools_test.go's own
// localExecRunner doc comment) -- letting
// KonturSandboxes.ConfigureGitCredentials' real *mcp.SSHRunner exercise
// the exact code path a real deployment does, without a real sshd for it
// to connect to.
func writeFakeSSH(t *testing.T, homeDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/bash
cd %q && exec bash -c "${@: -1}"
`, homeDir)
	path := filepath.Join(dir, "ssh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestKonturSandboxesConfigureGitCredentialsWritesToTheVMOverSSH(t *testing.T) {
	stateDir := t.TempDir()
	writeFakeKontur(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)
	writeFakeCrictl(t, filepath.Join(t.TempDir(), "counter"), 0, "10.100.5.7")
	home := t.TempDir()
	writeFakeSSH(t, home)

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix: "grain-test-",
		StateDir:   stateDir,
		SSHUser:    "debian",
		SSHKey:     "/key",
		Workspace:  "/workspace",
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

func TestKonturSandboxesCreatesVMOnFirstUseAndReusesAfter(t *testing.T) {
	stateDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeCrictl(t, filepath.Join(t.TempDir(), "counter"), 0, "10.100.5.7")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		SSHKey:            "/key",
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
		if line == "vm create grain-test-slot-0 -state-dir "+stateDir {
			createCalls++
		}
	}
	if createCalls != 1 {
		t.Errorf("kontur vm create was invoked %d times across two ToolsFor calls for the same slot, want 1: log = %q", createCalls, data)
	}
}

func TestKonturSandboxesReusesAnAlreadyExistingVMWithoutCreating(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "grain-test-slot-0.json"), []byte(`{"port": 30080}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKontur(t, argvLog, 30080)
	writeFakeCrictl(t, filepath.Join(t.TempDir(), "counter"), 0, "10.100.5.7")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix: "grain-test-",
		StateDir:   stateDir,
		SSHUser:    "debian",
		SSHKey:     "/key",
		Workspace:  "/workspace",
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
	writeFakeCrictl(t, filepath.Join(t.TempDir(), "counter"), 3, "10.100.5.7")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		SSHKey:            "/key",
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
	writeFakeCrictl(t, filepath.Join(t.TempDir(), "counter"), 1000, "10.100.5.7")

	k := orchestrator.NewKonturSandboxes(orchestrator.KonturConfig{
		NamePrefix:        "grain-test-",
		StateDir:          stateDir,
		SSHUser:           "debian",
		SSHKey:            "/key",
		Workspace:         "/workspace",
		ReadyPollInterval: time.Millisecond,
		ReadyTimeout:      10 * time.Millisecond,
	})

	if _, err := k.ToolsFor(context.Background(), "slot-0"); err == nil {
		t.Fatal("ToolsFor() on a VM that never becomes ready: got nil error, want one")
	}
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
