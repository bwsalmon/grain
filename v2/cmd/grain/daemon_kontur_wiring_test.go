package main

// TestRunConfiguresAKonturBackedSlotUsingCreateArgs is the daemon-level
// counterpart to pkg/orchestrator/kontur_sandboxes_test.go's own coverage
// of KonturSandboxes.ConfigureGitCredentials: that file proves the method
// itself does the right thing over SSH; this proves run() -- the exact
// function daemon() (daemon.go) calls -- actually reaches it when
// -kontur-vm-name-prefix is set, with -kontur-create-arg's values landing
// in the real `konturctl vm create` invocation verbatim. That plumbing
// (flags into a real KonturConfig.CreateArgs, in a binary that actually
// constructs orchestrator.KonturSandboxes) is what bwsalmon/agents#274
// asked this repo to wire up, once bwsalmon/kontur's own image flag was
// reachable to confirm -- it still wasn't, from this sandbox, so
// -kontur-create-arg carries whatever flag/value a deployment's operator
// confirms against bwsalmon/kontur's own `-h` output, rather than a guess
// baked in here.
//
// It fakes `konturctl` (the operator-facing binary Create/Delete actually
// exec -- not the distinct, container-facing "kontur" binary bwsalmon/
// kontur's own cmd/kontur is, per pkg/kontur's package doc comment),
// `crictl` and `ssh` on PATH (the same style kontur_sandboxes_test.go's
// own writeFakeKontur/writeFakeCrictl use, plus an ssh double), so it runs
// fast and needs neither a real kontur VM nor a real GitHub/Gemini
// endpoint: run() only needs to get through its own setup (git proxy,
// per-slot sandbox token, git credential configuration) before this test
// cancels its context, since that setup -- not a completed dispatch cycle
// -- is where -kontur-create-arg's plumbing actually happens.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeFakeKonturBinary(t *testing.T, argvLog string, port int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake konturctl script is POSIX shell only")
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
	install(t, dir, "konturctl", script)
}

func writeFakeCrictlBinary(t *testing.T, ip string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake crictl script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *pods*) echo '{"items":[{"id":"abc123"}]}' ;;
  *inspectp*) echo '{"status":{"network":{"ip":"%s"}}}' ;;
esac
`, ip)
	install(t, dir, "crictl", script)
}

// writeFakeSSHBinary stands in for a real sshd: it ignores every
// connection flag SSHRunner.Run passes and runs its trailing shell-quoted
// command (SSHRunner.Run's own doc comment) against homeDir, the same
// technique pkg/orchestrator/kontur_sandboxes_test.go's own writeFakeSSH
// uses.
func writeFakeSSHBinary(t *testing.T, homeDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/bash
cd %q && exec bash -c "${@: -1}"
`, homeDir)
	install(t, dir, "ssh", script)
}

func install(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunConfiguresAKonturBackedSlotUsingCreateArgs(t *testing.T) {
	// One slot, maxConcurrent 1: model.SlotNames(1) is "1", so that is the
	// slot name this test's own expectations (the VM name, its .git-
	// credentials file) key off below.
	const slot = "1"
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKonturBinary(t, argvLog, 30080)
	writeFakeCrictlBinary(t, "10.100.5.7")
	vmHome := t.TempDir()
	writeFakeSSHBinary(t, vmHome)

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"), []byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	geminiKeyFile := filepath.Join(t.TempDir(), "gemini.key")
	if err := os.WriteFile(geminiKeyFile, []byte("fake-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		dataDir: dataDir, maxConcurrent: 1, pollInterval: time.Hour,
		geminiAPIKeyFile: geminiKeyFile,
		githubHost:       "127.0.0.1:0", githubInsecureHTTP: true,

		konturVMNamePrefix: "grain-graind-test-",
		konturStateDir:     t.TempDir(),
		criRuntimeEndpoint: "unix:///run/containerd/containerd.sock",
		konturSSHUser:      "debian",
		konturSSHKey:       "/key",
		konturWorkspace:    "/workspace",
		konturCreateArgs:   []string{"-image", "gs://bucket/kontur-guest-deadbeef.qcow2"},
	}

	// run() only returns once ctx is cancelled (it then drives
	// pkg/orchestrator's reconcile loop forever, cfg.pollInterval apart),
	// but everything this test cares about -- creating the slot's VM and
	// configuring its git credentials over SSH -- happens synchronously
	// in run()'s own setup, before reconcile() is ever entered. So rather
	// than race a fixed sleep against that setup (flaky under real disk/
	// CPU contention, e.g. a slow embedded-SQLite open), this polls for the
	// setup's own last side effect -- the VM's .git-credentials file --
	// and only cancels ctx (letting run() return) once that has actually
	// happened.
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	credentialsPath := filepath.Join(vmHome, ".git-credentials")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if info, err := os.Stat(credentialsPath); err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("run() never configured the kontur VM's git credentials within the timeout (%s never appeared)", credentialsPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("run() = %v", err)
	}

	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("kontur was never invoked: %v", err)
	}
	want := "vm create " + cfg.konturVMNamePrefix + slot + " -state-dir " + cfg.konturStateDir + " -image gs://bucket/kontur-guest-deadbeef.qcow2\n"
	if string(data) != want {
		t.Errorf("kontur invoked as %q, want %q", data, want)
	}

	credentials, err := os.ReadFile(filepath.Join(vmHome, ".git-credentials"))
	if err != nil {
		t.Fatalf(".git-credentials was never written on the VM: %v", err)
	}
	if len(credentials) == 0 {
		t.Error(".git-credentials on the VM is empty")
	}
	if _, err := os.Stat(filepath.Join(vmHome, ".gitconfig")); err != nil {
		t.Errorf(".gitconfig was never written on the VM: %v", err)
	}
}
