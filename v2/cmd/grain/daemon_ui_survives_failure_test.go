// bwsalmon/agents#550: "make sure ui with logs stays up even if the
// daemon has failed." Before run()/runDaemon split apart (daemon.go), a
// failure anywhere past startUIServer returned an error out of run()
// itself, which tore the UI/API server down via its own defer right
// along with everything else, then exited the process (daemon()'s own
// log.Fatalf). That is exactly the moment an operator most needs the UI
// up: to see what is wrong. This test drives run() with a data directory
// the git proxy cannot lay its own state out under, the simplest
// remaining way to make runDaemon fail immediately, and proves the
// UI/API server it already started stays reachable anyway, rather than
// asserting on cmd/grain's own private goroutine plumbing.
//
// It used to induce that failure with a -gemini-api-key-file that did
// not exist, which no longer is one: an agent credential is set from the
// UI now, so a daemon with none runs on and reports it per dispatch
// (agentFrameworks) rather than giving up -- which is the whole point,
// since the UI this test is about is where that key gets pasted in.
// TestRunKeepsReconcilingWithNoAgentCredential covers that directly.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func TestRunKeepsTheUIServerUpWhenTheRestOfTheDaemonFails(t *testing.T) {
	// reconcilerDown is process-global state (its own doc comment,
	// daemon.go), so a run in this same test binary that sets it true
	// must not leak into any other test's expectations.
	t.Cleanup(func() { reconcilerDown.Store(false) })

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"), []byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	uiAddr := freeTCPAddr(t)
	// A plain file where the git proxy needs a directory: runDaemon's
	// startGitProxy fails on its very first call (gitproxy.BuildProxy ->
	// NewFileAuditLog, which cannot create its audit.log under a regular
	// file), before credentials or the reconcile loop ever start -- the
	// earliest a real misconfiguration plausibly could. Deliberately
	// state/git-proxy and not state itself: run() creates
	// state/transcripts before it starts the UI at all, so a file in the
	// way of *that* would fail the very thing this test needs up.
	if err := os.MkdirAll(filepath.Join(dataDir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "state", "git-proxy"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		dataDir: dataDir, sandboxDir: t.TempDir(), maxConcurrent: 1, pollInterval: time.Hour,
		githubHost: "127.0.0.1:0", githubInsecureHTTP: true,
		uiAddr: uiAddr, actor: "tester",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	// The UI/API server must come up and answer, even though runDaemon
	// behind it is (or soon will be) failing on that unusable data
	// directory --
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

	// bwsalmon/agents#576: the same runDaemon failure that leaves the UI
	// up must also be visible *through* that UI, not just in this
	// process's own log -- GET /api/config's reconcilerDown is what an
	// operator (or external monitoring polling that same endpoint) sees
	// instead of having to notice and interpret a log line.
	//
	// ui.HTTPClient.Config (unlike ui.Config itself) has no field for
	// this -- Config's own RebootEnabled/AutoMergeDegraded/ReconcilerDown
	// are all polling funcs server-side, not booleans an HTTP round trip
	// can reconstitute -- so this reads the raw JSON body directly, the
	// same wire shape pkg/ui/server_test.go's own decode[map[string]any]
	// helper reads in-process.
	resp, err := http.Get("http://" + uiAddr + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config: %v", err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding GET /api/config: %v", err)
	}
	if got["reconcilerDown"] != true {
		t.Fatalf("config.reconcilerDown = %v, want true once runDaemon has failed", got["reconcilerDown"])
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
