// bwsalmon/agents#576: the live-reproduced bug this issue reported was a
// transient failure during a kontur-backed slot's first-boot VM
// creation permanently wedging reconciliation -- runDaemon returned an
// error the first (and only) time `konturctl vm create` failed, and
// nothing ever tried again. This test proves the fix end to end, through
// the real run() a production binary calls: a fake `konturctl` that
// fails its very first invocation and succeeds every one after,
// combined with a short retry backoff (configureSlotGitCredentials'
// own default is minutes, far too slow for a test -- this drives the
// same mechanism with the daemon.go-private constants shrunk for the
// duration of the test), so a transient failure clears on its own
// without a restart.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeFlakyFakeKonturBinary is writeFakeKonturBinary (daemon_kontur_wiring_test.go)
// with one difference: `vm create` fails outright the first time it is
// invoked (no state file written, non-zero exit), succeeding on every
// call after -- attempts, a file the script itself appends one line to
// per invocation, is how this test observes that a retry actually
// happened rather than the first call having quietly succeeded.
func writeFlakyFakeKonturBinary(t *testing.T, attemptsPath string, port int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake konturctl script is POSIX shell only")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "vm" ] && [ "$2" = "create" ]; then
  echo x >> %q
  n=$(wc -l < %q)
  if [ "$n" -eq 1 ]; then
    echo "fake konturctl: simulated transient failure on first attempt" >&2
    exit 1
  fi
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
`, attemptsPath, attemptsPath, port)
	install(t, dir, "konturctl", script)
}

// shrinkSlotProvisionRetryDelaysForTest overrides daemon.go's own
// package-level slotProvisionRetryBaseDelay/slotProvisionRetryMaxDelay
// for the duration of the calling test, restoring them on cleanup --
// this test's whole point is proving a retry actually happens, which at
// the real 5-second base delay would make it needlessly slow without
// testing anything the fast, delay-agnostic retryWithBackoff unit tests
// in daemon_reconciler_retry_test.go don't already cover more directly.
func shrinkSlotProvisionRetryDelaysForTest(t *testing.T) func() {
	t.Helper()
	base, max := slotProvisionRetryBaseDelay, slotProvisionRetryMaxDelay
	slotProvisionRetryBaseDelay, slotProvisionRetryMaxDelay = 10*time.Millisecond, 50*time.Millisecond
	return func() { slotProvisionRetryBaseDelay, slotProvisionRetryMaxDelay = base, max }
}

func TestRunRecoversFromATransientSlotProvisioningFailure(t *testing.T) {
	restoreDelays := shrinkSlotProvisionRetryDelaysForTest(t)
	defer restoreDelays()

	attemptsPath := filepath.Join(t.TempDir(), "attempts.log")
	writeFlakyFakeKonturBinary(t, attemptsPath, 30081)
	vmHome := t.TempDir()
	writeFakeDockerBinary(t, vmHome)

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
		konturSSHUser:      "debian",
		konturExecKey:      "/images/key",
		konturWorkspace:    "/workspace",
		konturCreateArgs:   []string{"-image", "gs://bucket/kontur-guest-deadbeef.qcow2"},
	}

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
			t.Fatalf("run() never recovered and configured the kontur VM's git credentials within the timeout (%s never appeared)", credentialsPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("run() = %v", err)
	}

	data, err := os.ReadFile(attemptsPath)
	if err != nil {
		t.Fatalf("konturctl vm create was never invoked: %v", err)
	}
	if attempts := strings.Count(string(data), "x\n"); attempts < 2 {
		t.Fatalf("konturctl vm create logged %d attempt(s), want at least 2 (one simulated failure, then a retry that succeeded)", attempts)
	}
}
