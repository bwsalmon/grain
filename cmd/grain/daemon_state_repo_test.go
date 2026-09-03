package main

// The state repository, from the daemon's own start-up: that a running
// grain really does clone the repository it was pointed at, really does
// export what an operator files through the UI back into it, and really
// does push that to the remote. Everything here goes through run() and
// the REST API, against a bare repository on disk -- there is no fake
// git and no fake store in this file.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/staterepo"
	"github.com/bwsalmon/grain/pkg/ui"
)

func TestDaemonPushesItsDatabaseToTheStateRepository(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"),
		[]byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The repository an operator created and pointed grain at: real, and
	// completely empty, which is the "start from scratch" case.
	remote := filepath.Join(t.TempDir(), "state.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", remote).CombinedOutput(); err != nil {
		t.Fatalf("creating the remote: %v: %s", err, out)
	}
	if err := staterepo.SaveSettings(dataDir, staterepo.Settings{Remote: remote}); err != nil {
		t.Fatal(err)
	}

	uiAddr := freeTCPAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	// Stopped, and waited for, before this test's own temporary
	// directories are removed: run() writes into the working tree until
	// the moment it returns, and t.TempDir's cleanup racing that is a
	// failure with nothing to do with what is being tested.
	defer func() {
		cancel()
		<-runErr
	}()
	go func() {
		runErr <- run(ctx, config{
			dataDir: dataDir, sandboxDir: t.TempDir(), maxWorkers: 1, pollInterval: time.Hour,
			githubHost: "127.0.0.1:0", githubInsecureHTTP: true,
			uiAddr: uiAddr, actor: "tester",
		})
	}()

	client := ui.NewHTTPClient("http://" + uiAddr)
	waitForUI(t, ctx, client)

	// A task filed through the API is a row in the database, and the
	// point of all of this is that a row in the database is a file in the
	// repository.
	created, err := client.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "A task that should reach the state repository",
		Repo:  "owner/payments-api",
	})
	if err != nil {
		t.Fatalf("filing a task: %v", err)
	}

	// Synced on demand rather than by waiting out the daemon's own timer:
	// the same code path, reached through the bootstrap's own endpoint.
	if code, body := post(t, "http://"+uiAddr+"/api/state-repo/sync"); code != http.StatusOK {
		t.Fatalf("sync returned %d: %s", code, body)
	}

	out, err := exec.Command("git", "--git-dir", remote, "show", "main:"+staterepo.TablesDir+"/task.json").CombinedOutput()
	if err != nil {
		t.Fatalf("reading the dump out of the remote: %v: %s", err, out)
	}
	if !strings.Contains(string(out), created.ID) {
		t.Fatalf("the task did not reach the remote:\n%s", out)
	}
	if !strings.Contains(string(out), "A task that should reach the state repository") {
		t.Fatalf("the dump is not readable enough to review:\n%s", out)
	}

	// And what the bootstrap pane reports agrees with what actually
	// happened: a remote, a commit, and a secrets key on this host that
	// is not in the repository.
	code, body := get(t, "http://"+uiAddr+"/api/state-repo")
	if code != http.StatusOK {
		t.Fatalf("status returned %d: %s", code, body)
	}
	var status ui.StateRepoStatus
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatalf("decoding the status: %v: %s", err, body)
	}
	if !status.Available || status.Mode != "remote" || status.Remote != remote {
		t.Fatalf("status does not describe this deployment: %+v", status)
	}
	if status.Head == "" || status.SchemaVersion != status.BuildSchemaVersion {
		t.Fatalf("status does not describe the repository: %+v", status)
	}
	if !strings.HasPrefix(status.SecretsPublicKey, "grain-secret-pub-v1:") {
		t.Fatalf("no secrets key was minted for a fresh install: %+v", status)
	}
	// The private key is on this host, outside the working tree -- so a
	// clone of the remote cannot decrypt anything.
	if _, err := os.Stat(filepath.Join(dataDir, "secrets", "secrets.key")); err != nil {
		t.Fatalf("the private key is not where the deployment says it is: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "state-repo", "secrets.key")); !os.IsNotExist(err) {
		t.Fatal("the private key is inside the repository, which is pushed to the remote")
	}
}

func waitForUI(t *testing.T, ctx context.Context, client *ui.HTTPClient) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := client.ListTasks(ctx); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the UI/API server never came up: %v", lastErr)
}

func post(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	return resp.StatusCode, string(body[:n])
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	return resp.StatusCode, string(body[:n])
}
