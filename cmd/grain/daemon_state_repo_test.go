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
	"io"
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

// TestATaskSurvivesARestartWithAStateRepository is tests/container's own
// "a task survives the container that created it", at the level the state
// repository actually lives at -- and it is the regression that shipped
// with the first cut of this: the exporter runs on a timer, so a task
// filed seconds before a stop exists only in the database, and a start
// that imported the repository unconditionally rolled it straight back
// out again.
func TestATaskSurvivesARestartWithAStateRepository(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"),
		[]byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// First daemon: file a task, then stop straight away -- before the
	// sync timer has ticked, which is the whole point.
	uiAddr := freeTCPAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, config{
			dataDir: dataDir, sandboxDir: t.TempDir(), maxWorkers: 1, pollInterval: time.Hour,
			githubHost: "127.0.0.1:0", githubInsecureHTTP: true, uiAddr: uiAddr, actor: "tester",
		})
	}()
	client := ui.NewHTTPClient("http://" + uiAddr)
	waitForUI(t, ctx, client)
	created, err := client.CreateTask(ctx, ui.CreateTaskRequest{Title: "Filed just before the stop", Repo: "owner/payments-api"})
	if err != nil {
		t.Fatalf("filing a task: %v", err)
	}
	cancel()
	<-runErr

	// Second daemon, same data directory: the task has to still be there.
	secondAddr := freeTCPAddr(t)
	ctx2, cancel2 := context.WithCancel(context.Background())
	runErr2 := make(chan error, 1)
	go func() {
		runErr2 <- run(ctx2, config{
			dataDir: dataDir, sandboxDir: t.TempDir(), maxWorkers: 1, pollInterval: time.Hour,
			githubHost: "127.0.0.1:0", githubInsecureHTTP: true, uiAddr: secondAddr, actor: "tester",
		})
	}()
	defer func() {
		cancel2()
		<-runErr2
	}()
	second := ui.NewHTTPClient("http://" + secondAddr)
	waitForUI(t, ctx2, second)
	if _, err := second.Task(ctx2, created.ID); err != nil {
		t.Fatalf("the task did not survive the restart: %v", err)
	}
}

// The other direction, live: a change merged into the state repository
// while the daemon is running has to take effect in the daemon that is
// running, not in the next one (bwsalmon/grain#184). What must *not*
// happen alongside it is the rest of the import: the task filed here is
// the stand-in for every row a live run holds an id from, and it has to
// still be there afterwards.
func TestAMergedSettingsChangeTakesEffectWithoutARestart(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"),
		[]byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}
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

	// A template this deployment already has, so the repository holds a
	// row to change rather than only a file to create.
	if code, body := postBody(t, "http://"+uiAddr+"/api/templates",
		`{"name":"nightly","title":"Run the nightly sweep"}`); code != http.StatusCreated {
		t.Fatalf("creating a template returned %d: %s", code, body)
	}
	created, err := client.CreateTask(ctx, ui.CreateTaskRequest{Title: "A task a run is holding", Repo: "owner/payments-api"})
	if err != nil {
		t.Fatalf("filing a task: %v", err)
	}
	if code, body := post(t, "http://"+uiAddr+"/api/state-repo/sync"); code != http.StatusOK {
		t.Fatalf("sync returned %d: %s", code, body)
	}

	// An agent's pull request against the repository, merged: the
	// template is retitled and a second one is added.
	work := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "--quiet", remote, work).CombinedOutput(); err != nil {
		t.Fatalf("cloning the remote: %v: %s", err, out)
	}
	path := filepath.Join(work, staterepo.TablesDir, "template.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the dump: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("parsing the dump: %v: %s", err, data)
	}
	if len(rows) != 1 {
		t.Fatalf("the dump does not hold the template: %s", data)
	}
	rows[0]["title"] = "Run the nightly sweep, twice"
	added := map[string]any{}
	for k, v := range rows[0] {
		added[k] = v
	}
	added["id"] = "tpl-proposed-by-an-agent"
	added["name"] = "proposed"
	added["title"] = "Filed by a merged pull request"
	rows = append(rows, added)
	merged, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(merged, '\n'), 0o644); err != nil {
		t.Fatalf("writing the merged dump: %v", err)
	}
	for _, args := range [][]string{
		{"-c", "user.email=agent@grain", "-c", "user.name=agent", "commit", "-am", "Change the templates"},
		{"push", "--quiet", "origin", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// One cycle of the daemon's own loop, reached through the same
	// endpoint the pane's "Sync now" uses -- rather than waiting out the
	// thirty-second timer that would get there on its own.
	if code, body := post(t, "http://"+uiAddr+"/api/state-repo/sync"); code != http.StatusOK {
		t.Fatalf("sync returned %d: %s", code, body)
	}

	code, body := get(t, "http://"+uiAddr+"/api/templates")
	if code != http.StatusOK {
		t.Fatalf("listing templates returned %d: %s", code, body)
	}
	var templates []ui.Template
	if err := json.Unmarshal([]byte(body), &templates); err != nil {
		t.Fatalf("decoding the templates: %v: %s", err, body)
	}
	titles := map[string]bool{}
	for _, tmpl := range templates {
		titles[tmpl.Title] = true
	}
	if !titles["Run the nightly sweep, twice"] {
		t.Fatalf("the merged edit is not live in the running daemon: %s", body)
	}
	if !titles["Filed by a merged pull request"] {
		t.Fatalf("the merged addition is not live in the running daemon: %s", body)
	}

	// And nothing else came with it: the task is still there, with the id
	// a live run would be holding.
	if _, err := client.Task(ctx, created.ID); err != nil {
		t.Fatalf("applying a settings change took a task with it: %v", err)
	}
}

// A deployment that diverged from its remote, and got itself back.
//
// The failure is the one a push that fails leaves behind: grain commits
// its export locally, the push does not go through -- here because the
// remote is briefly not where the working tree thinks it is, in the real
// thing because an installation token expired -- and a pull request
// against the state repository is merged before the next push succeeds.
// The two branches are then on either side of the same parent, which
// Pull refuses to resolve, and until this nothing cleared that refusal:
// every tick logged it again, no merged change was ever applied, and no
// export ever reached the remote again.
//
// Both directions have to come back in one cycle, and both are asserted
// here: the merged retitle is live in the running daemon, and the task
// that only existed in the stranded commit is in the remote's own copy of
// the dump.
func TestADivergedStateRepositoryRecoversOnTheNextSync(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"),
		[]byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}
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

	if code, body := postBody(t, "http://"+uiAddr+"/api/templates",
		`{"name":"nightly","title":"Run the nightly sweep"}`); code != http.StatusCreated {
		t.Fatalf("creating a template returned %d: %s", code, body)
	}
	if code, body := post(t, "http://"+uiAddr+"/api/state-repo/sync"); code != http.StatusOK {
		t.Fatalf("sync returned %d: %s", code, body)
	}

	// The push that fails. The remote is pointed somewhere that is not
	// there for as long as one export takes, which is what an expired
	// credential looks like from the working tree's side: the commit is
	// made, and it stays here.
	stateDir := filepath.Join(dataDir, "state-repo")
	gitIn(t, stateDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	stranded, err := client.CreateTask(ctx, ui.CreateTaskRequest{
		Title: "Filed while the remote was unreachable", Repo: "owner/payments-api"})
	if err != nil {
		t.Fatalf("filing a task: %v", err)
	}
	if code, _ := post(t, "http://"+uiAddr+"/api/state-repo/sync"); code == http.StatusOK {
		t.Fatal("a sync with an unreachable remote reported success")
	}
	gitIn(t, stateDir, "remote", "set-url", "origin", remote)

	// And the merge that lands before the push can be retried: an agent's
	// pull request against the settings, from a clone taken before any of
	// this happened.
	work := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "--quiet", remote, work).CombinedOutput(); err != nil {
		t.Fatalf("cloning the remote: %v: %s", err, out)
	}
	path := filepath.Join(work, staterepo.TablesDir, "template.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the dump: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("parsing the dump: %v: %s", err, data)
	}
	if len(rows) != 1 {
		t.Fatalf("the dump does not hold the template: %s", data)
	}
	rows[0]["title"] = "Retitled by a pull request nobody could push over"
	merged, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(merged, '\n'), 0o644); err != nil {
		t.Fatalf("writing the merged dump: %v", err)
	}
	gitIn(t, work, "-c", "user.email=agent@grain", "-c", "user.name=agent", "commit", "-am", "Retitle the template")
	gitIn(t, work, "push", "--quiet", "origin", "main")

	// One ordinary cycle of the daemon's own loop, which is all the
	// recovery gets: the same call the timer makes.
	if code, body := post(t, "http://"+uiAddr+"/api/state-repo/sync"); code != http.StatusOK {
		t.Fatalf("the sync that should have recovered returned %d: %s", code, body)
	}

	// Inbound: what was merged is live in the daemon that is running.
	code, body := get(t, "http://"+uiAddr+"/api/templates")
	if code != http.StatusOK {
		t.Fatalf("listing templates returned %d: %s", code, body)
	}
	var templates []ui.Template
	if err := json.Unmarshal([]byte(body), &templates); err != nil {
		t.Fatalf("decoding the templates: %v: %s", err, body)
	}
	if len(templates) != 1 || templates[0].Title != "Retitled by a pull request nobody could push over" {
		t.Fatalf("the merged change never became live: %s", body)
	}

	// Outbound: the task that existed only in the commit the recovery
	// threw away is back in the remote's own copy of the dump, written
	// there by the export that followed the reset.
	out, err := exec.Command("git", "--git-dir", remote, "show", "main:"+staterepo.TablesDir+"/task.json").CombinedOutput()
	if err != nil {
		t.Fatalf("reading the dump out of the remote: %v: %s", err, out)
	}
	if !strings.Contains(string(out), stranded.ID) {
		t.Fatalf("the stranded export never reached the remote:\n%s", out)
	}

	// And the pane says the deployment is syncing again, rather than
	// still reporting the divergence it recovered from.
	code, body = get(t, "http://"+uiAddr+"/api/state-repo")
	if code != http.StatusOK {
		t.Fatalf("status returned %d: %s", code, body)
	}
	var status ui.StateRepoStatus
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatalf("decoding the status: %v: %s", err, body)
	}
	if status.Diverged || status.Error != "" {
		t.Fatalf("the pane still reports the deployment as broken: %+v", status)
	}
}

// The same divergence found at startup rather than on a tick, which used
// to be the worse half of it: Load refuses, run() returns the refusal,
// and the deployment does not come up at all -- over a commit that holds
// nothing but a dump of the database it is refusing to start against.
func TestADivergedStateRepositoryDoesNotStopTheDaemonFromStarting(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"),
		[]byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "state.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", remote).CombinedOutput(); err != nil {
		t.Fatalf("creating the remote: %v: %s", err, out)
	}
	if err := staterepo.SaveSettings(dataDir, staterepo.Settings{Remote: remote}); err != nil {
		t.Fatal(err)
	}

	// A first daemon, long enough to file a task and push it.
	uiAddr := freeTCPAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, config{
			dataDir: dataDir, sandboxDir: t.TempDir(), maxWorkers: 1, pollInterval: time.Hour,
			githubHost: "127.0.0.1:0", githubInsecureHTTP: true, uiAddr: uiAddr, actor: "tester",
		})
	}()
	client := ui.NewHTTPClient("http://" + uiAddr)
	waitForUI(t, ctx, client)
	created, err := client.CreateTask(ctx, ui.CreateTaskRequest{Title: "Filed before the divergence", Repo: "owner/payments-api"})
	if err != nil {
		t.Fatalf("filing a task: %v", err)
	}
	if code, body := post(t, "http://"+uiAddr+"/api/state-repo/sync"); code != http.StatusOK {
		t.Fatalf("sync returned %d: %s", code, body)
	}
	cancel()
	<-runErr

	// An export committed on this host and never pushed -- grain's own
	// author, and nothing but the dump in it -- with a merge landing on
	// the remote on the other side of the same parent.
	stateDir := filepath.Join(dataDir, "state-repo")
	dump := filepath.Join(stateDir, staterepo.TablesDir, "task.json")
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("reading the dump: %v", err)
	}
	if err := os.WriteFile(dump, []byte(strings.Replace(string(data),
		"Filed before the divergence", "Retitled by an export that never got out", 1)), 0o644); err != nil {
		t.Fatalf("writing the dump: %v", err)
	}
	gitIn(t, stateDir, "-c", "user.email=grain@a-container-that-has-gone", "-c", "user.name=grain",
		"commit", "-am", "Update grain state")

	work := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "--quiet", remote, work).CombinedOutput(); err != nil {
		t.Fatalf("cloning the remote: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(work, "NOTES.md"), []byte("merged while grain was down\n"), 0o644); err != nil {
		t.Fatalf("writing the merged file: %v", err)
	}
	gitIn(t, work, "add", "--all", ".")
	gitIn(t, work, "-c", "user.email=agent@grain", "-c", "user.name=agent", "commit", "-m", "A merged pull request")
	gitIn(t, work, "push", "--quiet", "origin", "main")

	// The deployment comes back up, on the remote's history, with the
	// task it had.
	secondAddr := freeTCPAddr(t)
	ctx2, cancel2 := context.WithCancel(context.Background())
	runErr2 := make(chan error, 1)
	go func() {
		runErr2 <- run(ctx2, config{
			dataDir: dataDir, sandboxDir: t.TempDir(), maxWorkers: 1, pollInterval: time.Hour,
			githubHost: "127.0.0.1:0", githubInsecureHTTP: true, uiAddr: secondAddr, actor: "tester",
		})
	}()
	defer func() {
		cancel2()
		<-runErr2
	}()
	second := ui.NewHTTPClient("http://" + secondAddr)
	waitForUI(t, ctx2, second)
	if _, err := second.Task(ctx2, created.ID); err != nil {
		t.Fatalf("the task did not survive the recovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "NOTES.md")); err != nil {
		t.Fatalf("the merged change never arrived: %v", err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
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
	return postBody(t, url, "{}")
}

func postBody(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	// Read to the end rather than into a fixed buffer: a caller here
	// decodes the body as JSON, and a truncated one fails in a way that
	// says nothing about what is being tested.
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}
