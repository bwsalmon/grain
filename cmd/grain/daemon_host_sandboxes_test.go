// grain/task-15: a deployment dispatching every agent into a directory
// on the daemon's own machine -- no isolation from its filesystem, its
// network or its credentials -- looked, from the UI, exactly like one
// giving each run a VM of its own. It is a standing banner now, and this
// is the wiring under it: run() branches on cfg.konturSandboxes to build
// the backend, and startUIServer hands the same field to
// ui.Config.HostSandboxes, so the banner cannot say one thing while the
// process does another.
//
// Driven through run() rather than by reading the struct literal in
// startUIServer, because the failure worth catching is exactly the one a
// unit test of that literal cannot see: the two reading different fields.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/ui"
)

func TestRunReportsHostSandboxingToTheUI(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"), []byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	uiAddr := freeTCPAddr(t)
	// No -kontur-sandboxes, which is what every host-mode deployment's
	// own unit file looks like: sandboxDir and nothing else.
	cfg := config{
		dataDir: dataDir, sandboxDir: t.TempDir(), maxWorkers: 1, pollInterval: time.Hour,
		githubHost: "127.0.0.1:0", githubInsecureHTTP: true,
		uiAddr: uiAddr, actor: "tester",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()
	defer func() {
		cancel()
		<-runErr
	}()

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
		t.Fatalf("UI/API server never came up: %v", lastErr)
	}

	// The raw body, for the reason TestRunKeepsTheUIServerUpWhenTheRest
	// OfTheDaemonFails reads one: ui.HTTPClient carries no field for
	// this, and the wire shape is what the frontend actually draws from.
	resp, err := http.Get("http://" + uiAddr + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config: %v", err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding GET /api/config: %v", err)
	}
	if got["hostSandboxes"] != true {
		t.Fatalf("config.hostSandboxes = %v, want true for a daemon built on orchestrator.HostSandboxes", got["hostSandboxes"])
	}
}
