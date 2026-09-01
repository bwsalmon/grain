package main

// TestRunRebuildsAKonturSlotLeftBehindByAPreviousProcess covers runDaemon's
// startup reset pass over every slot -- the half of bwsalmon/agents#353's
// isolation boundary that the per-task recreate in pkg/orchestrator's own
// cycle.go cannot reach.
//
// dispatch rebuilds a slot's VM after every task it finishes, so the only
// way a VM outlives the run that dirtied it is a process that died before
// getting there: killed mid-run, OOMed, or a host that rebooted. On the
// next start, KonturSandboxes.ensure deliberately adopts an
// already-existing VM rather than rebuilding it, so before this pass the
// next task dispatched onto that slot inherited the dead run's checkout,
// credentials and leftover processes. The state file this test seeds
// stands in for exactly that VM: kontur.Exists reads it, which is what
// makes the difference between the two cases observable.
//
// The fresh-deployment half -- no VM, so no `konturctl vm delete` for a
// name kontur has never heard of -- is what
// TestRunConfiguresAKonturBackedSlotUsingCreateArgs already pins down, by
// asserting the whole argv log is one `vm create`.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunRebuildsAKonturSlotLeftBehindByAPreviousProcess(t *testing.T) {
	// maxConcurrent 1, so model.SlotNames(1) is "1" and the VM a previous
	// process would have left behind is prefix+"1".
	const slot = "1"
	argvLog := filepath.Join(t.TempDir(), "kontur-argv.log")
	writeFakeKonturBinary(t, argvLog, 30080)
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

		konturVMNamePrefix: "grain-reset-test-",
		konturStateDir:     t.TempDir(),
		konturSSHUser:      "debian",
		konturExecKey:      "/images/key",
		konturWorkspace:    "/workspace",
	}

	// The VM a killed process left behind: kontur's own state file for
	// this slot's name, the same thing `konturctl vm create` writes and
	// kontur.Exists reads back.
	vmName := cfg.konturVMNamePrefix + slot
	if err := os.WriteFile(filepath.Join(cfg.konturStateDir, vmName+".json"), []byte(`{"port": 30080}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Same shape as TestRunConfiguresAKonturBackedSlotUsingCreateArgs: run()
	// returns only once ctx is cancelled, but everything asserted here
	// happens in its setup, so this polls for that setup's own last side
	// effect rather than racing a fixed sleep against it.
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
			t.Fatalf("run() never finished setting up the kontur slot within the timeout (%s never appeared)", credentialsPath)
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
	// Delete first, then create: the inherited VM is torn down rather than
	// adopted, and the slot still ends up with a working VM afterwards --
	// the credential configuration that wrote credentialsPath above ran
	// over the rebuilt one, and did not have to create it a second time.
	want := []string{
		"vm delete " + vmName + " -state-dir " + cfg.konturStateDir,
		"vm create " + vmName + " -state-dir " + cfg.konturStateDir + " -backend docker -net flat",
	}
	var got []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			got = append(got, line)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("kontur invocations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("invocation %d = %q, want %q", i, got[i], want[i])
		}
	}
}
