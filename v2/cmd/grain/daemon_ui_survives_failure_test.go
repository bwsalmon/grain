// bwsalmon/agents#550: "make sure ui with logs stays up even if the
// daemon has failed." Before run()/runDaemon split apart (daemon.go), a
// failure anywhere past startUIServer -- a bad -gemini-api-key-file among
// them -- returned an error out of run() itself, which tore the UI/API
// server down via its own defer right along with everything else, then
// exited the process (daemon()'s own log.Fatalf). That is exactly the
// moment an operator most needs the UI up: to see what is wrong. This
// test drives run() with a -gemini-api-key-file that does not exist, the
// simplest way to make runDaemon fail immediately, and proves the UI/API
// server it already started stays reachable anyway, rather than either
// test asserting on cmd/grain's own private goroutine plumbing.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func TestRunKeepsTheUIServerUpWhenTheRestOfTheDaemonFails(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"), []byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	uiAddr := freeTCPAddr(t)
	cfg := config{
		dataDir: dataDir, maxConcurrent: 1, pollInterval: time.Hour,
		// A gemini API key file that was never written: runDaemon's own
		// readTrimmedFile(cfg.geminiAPIKeyFile) fails on its very first
		// call, before the git proxy, credentials, or reconcile loop ever
		// start -- the earliest a real misconfiguration plausibly could.
		geminiAPIKeyFile: filepath.Join(dataDir, "no-such-gemini-key"),
		githubHost:       "127.0.0.1:0", githubInsecureHTTP: true,
		uiAddr: uiAddr, actor: "tester",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	// The UI/API server must come up and answer, even though runDaemon
	// behind it is (or soon will be) failing on the missing key file --
	// ListTasks needs nothing from runDaemon (no store write, no git
	// proxy, no agent framework), just the store startUIServer already
	// has its own handle on.
	client := ui.NewHTTPClient("http://" + uiAddr)
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := client.ListTasks(ctx); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		cancel()
		<-runErr
		t.Fatalf("UI/API server never came up (or stopped answering) despite runDaemon failing: %v", lastErr)
	}

	// run() itself must not have already returned on its own: with
	// -ui-addr set, a runDaemon failure is logged, not fatal, and run()
	// only returns once ctx is cancelled.
	select {
	case err := <-runErr:
		t.Fatalf("run() returned (%v) before ctx was cancelled -- the UI/API server would have been torn down with it", err)
	default:
	}

	// The UI must still be reachable well after runDaemon's own failure
	// -- not just in the brief window before it gave up.
	if _, err := client.ListTasks(ctx); err != nil {
		t.Fatalf("UI/API server stopped answering after runDaemon's failure: %v", err)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run() = %v, want nil once ctx is cancelled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run() never returned after ctx was cancelled")
	}
}
