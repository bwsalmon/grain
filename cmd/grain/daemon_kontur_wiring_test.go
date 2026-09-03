package main

// TestRunBuildsAKonturVMForADispatchUsingCreateArgs is the daemon-level
// counterpart to pkg/orchestrator/kontur_sandboxes_test.go's own coverage
// of KonturSandboxes.ConfigureGitCredentials: that file proves the method
// itself does the right thing over SSH; this proves run() -- the exact
// function daemon() (daemon.go) calls -- actually reaches it when
// -kontur-sandboxes is set, with -kontur-create-arg's values landing
// in the real `konturctl vm create` invocation verbatim. That plumbing
// (flags into a real KonturConfig.CreateArgs, in a binary that actually
// constructs orchestrator.KonturSandboxes) is what bwsalmon/agents#274
// asked this repo to wire up, once bwsalmon/kontur's own image flag was
// reachable to confirm -- it still wasn't, from this sandbox, so
// -kontur-create-arg carries whatever flag/value a deployment's operator
// confirms against bwsalmon/kontur's own `-h` output, rather than a guess
// baked in here.
//
// The setup it waits on used to be run()'s own: a slot's VM was created
// and credentialed at startup, before reconcile was ever entered. There
// is no such step now -- a VM is built for a run -- so this seeds one
// approved task and waits on the first reconcile tick's dispatch to build
// it, which is the path a real deployment takes too.
//
// It fakes `konturctl` (the operator-facing binary Create/Delete actually
// exec -- not the distinct, container-facing "kontur" binary bwsalmon/
// kontur's own cmd/kontur is, per pkg/kontur's package doc comment),
// and `docker` on PATH (the same style kontur_sandboxes_test.go's own
// writeFakeKontur/writeFakeDocker use), so it runs
// fast and needs neither a real kontur VM nor a real GitHub/Gemini
// endpoint: the dispatch this drives will fail once it tries to reach
// GitHub, long after `konturctl vm create` has already been invoked --
// which is the only thing under test here.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// writeFakeKonturBinary installs a fake "konturctl" that answers "vm
// create" by writing the state file kontur.Exists reads back and "vm
// delete" by removing it (kontur's own staticpod.Save/Delete), logging
// every invocation's argv to argvLog. Handling delete is what lets a test
// observe a sandbox's Release, and the startup reap pass
// (KonturSandboxes.ReapOrphans) that deletes a VM a previous process left
// behind.
func writeFakeKonturBinary(t *testing.T, argvLog string, port int) {
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
	install(t, dir, "konturctl", script)
}

// writeFakeDockerBinary installs a fake "docker" whose `exec` runs the
// command it was handed in vmHome, standing in for the guest's own
// filesystem, and reports every container as running. The docker-exec
// counterpart to the fake `ssh` (and the crictl address lookup) this
// replaced -- everything after "kontur exec --" is a real argv, so the
// fake can exec it directly.
func writeFakeDockerBinary(t *testing.T, vmHome string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake docker script is POSIX shell only")
	}
	dir := t.TempDir()
	install(t, dir, "docker", fmt.Sprintf(`#!/bin/sh
case "$1" in
exec)
  shift
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
  shift
  cd %q && exec "$@"
  ;;
inspect)
  echo running
  ;;
*)
  echo "fake docker: unexpected subcommand: $*" >&2
  exit 1
  ;;
esac
`, vmHome))
}

func install(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunBuildsAKonturVMForADispatchUsingCreateArgs(t *testing.T) {
	// A sandbox is named after its run, so the VM name this test expects
	// is the prefix plus the seeded task's own first-attempt run id.
	const taskID = "kw"
	const sandbox = taskID + "-1"
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

	// One approved task, ready to dispatch on reconcile's first tick --
	// which is what builds the VM now.
	store, db, err := openStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	task := model.Task{
		ID:     taskID,
		Intent: model.IntentImplement,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "tester"}},
			Reason:      model.ReasonDirect,
		},
		Binding: model.BindingDirective,
		Target:  &model.RepoRef{Owner: "acme", Name: "widgets"},
	}
	task.Approval = &model.Attribution{Actor: task.Origin.Attribution.Actor}
	if err := store.PutTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cfg := config{
		dataDir: dataDir, maxWorkers: 1, pollInterval: time.Hour,
		geminiAPIKeyFile: geminiKeyFile,
		// agent/antigravity runs a real binary, so agentFrameworks
		// resolves one before anything dispatches -- unlike the
		// in-process Gemini runtime it replaced, which needed only the
		// key file above. This test never lets a run get as far as
		// exec'ing it (it asserts on the VM being built and
		// credentialed), so naming a path is enough; leaving it empty
		// would fail the whole daemon on a machine with no agy
		// installed, which is every CI machine.
		agyPath:    filepath.Join(t.TempDir(), "agy"),
		githubHost: "127.0.0.1:0", githubInsecureHTTP: true,

		konturSandboxes:  true,
		konturStateDir:   t.TempDir(),
		konturSSHUser:    "debian",
		konturExecKey:    "/images/key",
		konturWorkspace:  "/workspace",
		konturCreateArgs: []string{"-image", "gs://bucket/kontur-guest-deadbeef.qcow2"},
	}

	// run() only returns once ctx is cancelled (it drives reconcile
	// forever, cfg.pollInterval apart), and the dispatch this test wants
	// happens on that loop's first tick. Rather than race a fixed sleep
	// against it (flaky under real disk/CPU contention, e.g. a slow
	// embedded-SQLite open), poll for the dispatch's own side effect --
	// the VM's .git-credentials file, written once the run has acquired
	// its sandbox -- and only then cancel.
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	credentialsPath := filepath.Join(vmHome, ".git-credentials")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if info, err := os.Stat(credentialsPath); err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("run() never built and credentialed a kontur VM for the seeded task within the timeout (%s never appeared)", credentialsPath)
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
	// The size flags are part of what this asserts: a deployment that
	// configured no sandbox shape at all still creates its VM at grain's
	// own default (kontur.DefaultCPUs/DefaultMemoryMB/DefaultDiskGB, the
	// last in MiB), rather than leaving the size to konturctl.
	want := "vm create " + orchestrator.VMNamePrefix + sandbox + " -state-dir " + cfg.konturStateDir +
		" -backend docker -net flat -image gs://bucket/kontur-guest-deadbeef.qcow2 -cpus 2 -memory-mb 8192 -disk-size-mb 30720"
	if !strings.Contains(string(data), want) {
		t.Errorf("kontur invoked as %q, want a %q among the calls", data, want)
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
