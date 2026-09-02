package main

// TestRunLiveWithKonturAndRESTAPIOpensAPullRequest is bwsalmon/agents#373's
// "true e2e test": every real dependency this repo already has a live
// test for individually -- the real Gemini API
// (pkg/agent/antigravity's own live test), a real GitHub stand-in reached
// over real HTTP (githubsim, daemon_live_test.go), and kontur-backed
// sandboxing (daemon_kontur_wiring_test.go) -- combined in one run, with
// the task itself filed and approved through the real REST API
// (pkg/ui.HTTPClient against a really-listening pkg/ui.Server) instead of
// a direct store write, which is how a human or the CLI actually files
// one. No existing test combines more than two of these four; see that
// issue's own gap analysis (daemon_live_test.go's closest analog uses a
// plain "local" HostSandboxes slot and seeds its task with store.PutTask,
// skipping both kontur and the REST API).
//
// It reuses daemon_live_test.go's githubHostServer/syncedSim/runLive/
// writeKeyFile and daemon_kontur_wiring_test.go's fake kontur/crictl
// binaries verbatim (same package, same techniques) rather than
// reinventing either. The one new piece of infrastructure is
// installFakeDockerBinaryWithHome, below: writeFakeDockerBinary's fake
// stands in for a real sshd well enough for ConfigureGitCredentials'
// relative-path writes (the only thing daemon_kontur_wiring_test.go and
// kontur_sandboxes_test.go ever run over it), but this test's dispatched
// agent also runs `git push` for real, and git's own
// credential.helper=store looks its credentials up at
// $HOME/.git-credentials -- left as this process's own real $HOME rather
// than the fake VM's home directory, that lookup would miss the
// credentials ConfigureGitCredentialsOverSSH just wrote.
//
// Beyond confirming the pull request itself, this also closes the loop a
// real deployment cares about: a simulated human merges it (a real git
// merge over the same bare repo, plus a real HTTP PUT to githubsim's own
// merge endpoint from a second, independent github.Client -- the same
// technique e2e/cli_test.go uses for the scripted-agent version of this
// story), and the test then waits for the daemon's own next reconcile
// tick to notice the merge and close the task out, confirmed, again,
// through the REST API rather than the store.
//
// Gated on GEMINI_API_KEY exactly like the other three live tests, so it
// runs nowhere without a live key (including CI) and costs nothing when
// skipped:
//
//	GEMINI_API_KEY=... go test ./cmd/grain/... -run TestRunLiveWithKonturAndRESTAPIOpensAPullRequest -v

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/agent/antigravity"
	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/github/githubsim"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/ui"
)

// firstPullRequestNumber returns sim's first pull request's number --
// added on this file's own copy of syncedSim (daemon_live_test.go), since
// this is the one test in this package that needs a second, independent
// github.Client to act on a pull request sim's own dispatch already
// opened, the same way e2e/cli_test.go's own syncedSim does for its
// scripted-agent equivalent of this test.
func (s *syncedSim) firstPullRequestNumber() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sim.PullRequests[0].Number
}

// installFakeDockerBinaryWithHome installs a fake `docker` on PATH, the
// same shape writeFakeDockerBinary (daemon_kontur_wiring_test.go) uses --
// run the argv after "kontur exec --" against homeDir, standing in for a
// real sshd's own "start a fresh login session in $HOME" -- but also pins
// HOME to homeDir for that command, which writeFakeDockerBinary does not:
// see this file's own doc comment for why this test needs that and the
// other kontur tests do not.
func installFakeDockerBinaryWithHome(t *testing.T, homeDir string) {
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
  cd %[1]q && HOME=%[1]q exec "$@"
  ;;
inspect)
  echo running
  ;;
*)
  echo "fake docker: unexpected subcommand: $*" >&2
  exit 1
  ;;
esac
`, homeDir))
}

// freeTCPAddr returns a loopback address nothing is listening on yet, for
// cfg.uiAddr: startUIServer needs a concrete address up front to bind --
// run() reports back only an error, never whatever port a ":0" address
// actually bound to -- so this probes one the same way
// net.Listen("tcp", "127.0.0.1:0") itself would and frees it immediately
// for run() to bind instead.
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free TCP port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("freeing the probed TCP port: %v", err)
	}
	return addr
}

func TestRunLiveWithKonturAndRESTAPIOpensAPullRequest(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live grain daemon integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fake kontur/crictl/ssh scripts are POSIX shell only")
	}

	const owner, repoName = "acme", "gizmos"
	// maxConcurrent 1, so the seeded task's first attempt is the only
	// run this drives, and its sandbox is named after it.

	// A real bare repo standing in for the GitHub-hosted one, seeded
	// with one commit on main -- githubsim.Sim.BranchExists shells out
	// to git against this same path.
	upstream := t.TempDir()
	bare := filepath.Join(upstream, owner, repoName+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runLive(t, upstream, "git", "init", "--bare", "-b", "main", bare)
	runLive(t, upstream, "git", "-C", bare, "config", "http.receivepack", "true")
	seedDir := filepath.Join(t.TempDir(), "seed")
	runLive(t, t.TempDir(), "git", "clone", bare, seedDir)
	runLive(t, seedDir, "git", "config", "user.email", "seed@example.com")
	runLive(t, seedDir, "git", "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runLive(t, seedDir, "git", "add", "README.md")
	runLive(t, seedDir, "git", "commit", "-q", "-m", "initial commit")
	runLive(t, seedDir, "git", "push", "origin", "main")

	sim := &syncedSim{sim: githubsim.New(owner, repoName, bare, "main")}
	githubHost := githubHostServer(t, sim, upstream)

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"), []byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// The kontur "VM": a fake kontur/crictl/ssh trio, the same technique
	// daemon_kontur_wiring_test.go and kontur_sandboxes_test.go both use
	// to stand in for a real VM/sshd. Unlike either of those, this test's
	// dispatched agent runs its real tool calls (run_command/write_file)
	// through it, not just ConfigureGitCredentials, so
	// KonturSandboxes' own Acquire/Tools code path -- the whole reason a
	// deployment opts into kontur at all -- runs for real too, ending in
	// a real `git push` executed by the real Gemini-driven agent.
	konturStateDir := t.TempDir()
	vmHome := t.TempDir()
	workspace := filepath.Join(vmHome, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeKonturBinary(t, filepath.Join(t.TempDir(), "kontur-argv.log"), 30080)
	installFakeDockerBinaryWithHome(t, vmHome)

	uiAddr := freeTCPAddr(t)

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, config{
			dataDir: dataDir, maxConcurrent: 1, pollInterval: 5 * time.Second,
			geminiAPIKeyFile: writeKeyFile(t, apiKey), geminiModel: antigravity.DefaultModel, maxAgentTurns: 15,
			githubHost: githubHost, githubInsecureHTTP: true,

			uiAddr: uiAddr, actor: "tester",

			konturSandboxes: true,
			konturStateDir:  konturStateDir,
			konturSSHUser:   "debian",
			konturExecKey:   "/images/key",
			konturWorkspace: workspace,
		})
	}()

	// File the task through the real REST API run() itself just started
	// serving -- not a direct store write (contrast daemon_live_test.go's
	// own store.PutTask) -- the way a human or the CLI actually would.
	// It is filed unapproved, then updated with its real push/branch
	// command and only then approved, both still over the REST API,
	// because the branch name (model.BranchName(task.ID)) depends on the
	// ID Store.NewTaskID allocates when CreateTask runs, which nothing
	// can predict beforehand; leaving the task unapproved until that
	// command is in place keeps the daemon's own poll loop from
	// dispatching it too early.
	client := ui.NewHTTPClient("http://" + uiAddr)
	var task ui.Task
	{
		deadline := time.Now().Add(30 * time.Second)
		for {
			created, err := client.CreateTask(ctx, ui.CreateTaskRequest{
				Title:       "grain true e2e test",
				Description: "filed by TestRunLiveWithKonturAndRESTAPIOpensAPullRequest; updated below",
				Repo:        owner + "/" + repoName,
			})
			if err == nil {
				task = created
				break
			}
			if time.Now().After(deadline) {
				cancel()
				<-done
				t.Fatalf("filing the task through the REST API: %v", err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	branch := model.BranchName(task.ID)
	description := "Call your run_command tool exactly once with the single shell command " +
		"below (it already includes every step: discovering the one git host your sandbox " +
		"has credentials for, cloning, branching, editing, committing and pushing), then " +
		"reply with a short confirmation once it succeeds. Do not run anything else first.\n\n" +
		"HOST=$(sed -n 's#.*@\\([^/]*\\).*#\\1#p' ~/.git-credentials) && " +
		"git clone http://$HOST/" + owner + "/" + repoName + ".git work && " +
		"cd work && git checkout -b " + branch + " && " +
		"echo 'grain true e2e test' >> NOTES.md && " +
		"git add NOTES.md && git commit -q -m 'grain true e2e test' && " +
		"git push origin " + branch
	if _, err := client.UpdateTask(ctx, task.ID, ui.UpdateTaskRequest{Description: &description}); err != nil {
		cancel()
		<-done
		t.Fatalf("updating the task through the REST API: %v", err)
	}
	if err := client.Approve(ctx, task.ID); err != nil {
		cancel()
		<-done
		t.Fatalf("approving the task through the REST API: %v", err)
	}

	// Poll the real bare repo -- not sim, so no lock needed here -- for
	// the branch a successful live run, dispatched onto the kontur-faked
	// sandbox, pushes. This is the first proof the whole chain worked:
	// REST API -> store -> orchestrator -> real Gemini agent -> kontur's
	// SSH tools -> git proxy -> githubsim's git-http-backend.
	deadline := time.Now().Add(180 * time.Second)
	pushed := false
	for time.Now().Before(deadline) {
		if exec.Command("git", "--git-dir", bare, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil {
			pushed = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !pushed {
		cancel()
		<-done
		t.Fatalf("branch %s was never pushed to %s within the timeout -- the live agent did not complete the dispatch", branch, bare)
	}

	// openPullRequest runs synchronously right after a successful push,
	// in the same tick (orchestrate.go's runDispatch); poll the task back
	// through the REST API -- not sim directly -- for its PullRequest
	// field to confirm both that a PR was opened against githubsim *and*
	// that the daemon's own REST API surfaces it, closing the loop this
	// test is named for.
	prOpened := false
	for deadline2 := time.Now().Add(20 * time.Second); time.Now().Before(deadline2); {
		got, err := client.Task(ctx, task.ID)
		if err == nil && got.PullRequest != "" {
			prOpened = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !prOpened {
		cancel()
		<-done
		t.Fatalf("branch %s was pushed but the REST API never reported a pull request for task %s", branch, task.ID)
	}
	if sim.pullRequestCount() == 0 {
		cancel()
		<-done
		t.Fatalf("REST API reported a pull request for task %s but githubsim has none", task.ID)
	}
	prNumber := sim.firstPullRequestNumber()

	// A simulated human merges the pull request: a real git merge over
	// the same bare repo, plus a real HTTP PUT to githubsim's own merge
	// endpoint from a second, independent github.Client, standing in for
	// a person clicking "Merge pull request" on GitHub the way the REST
	// API already stood in for one filing the task in the first place.
	remote := "http://" + githubHost + "/" + owner + "/" + repoName + ".git"
	mergeDir := filepath.Join(t.TempDir(), "merge")
	runLive(t, t.TempDir(), "git", "clone", remote, mergeDir)
	runLive(t, mergeDir, "git", "config", "user.email", "github@example.com")
	runLive(t, mergeDir, "git", "config", "user.name", "github (simulated merge)")
	runLive(t, mergeDir, "git", "fetch", "origin", branch)
	runLive(t, mergeDir, "git", "checkout", "main")
	runLive(t, mergeDir, "git", "merge", "--no-ff", "origin/"+branch, "-m", "Merge "+branch)
	runLive(t, mergeDir, "git", "push", "origin", "main")

	userTransport := github.NewRealTransport(githubHost)
	userTransport.UseTLS = false
	userClient := github.NewClient(userTransport, nil)
	if err := userClient.MergePullRequest(owner, repoName, prNumber); err != nil {
		cancel()
		<-done
		t.Fatalf("submitting (merging) the pull request: %v", err)
	}

	// The daemon's own next reconcile tick syncs the now-merged pull
	// request and closes the task out -- confirmed, once more, through
	// the REST API rather than the store, so this test's whole final
	// assertion is exactly what an operator watching the same API would
	// see: the task they filed, dispatched, and had merged reads closed.
	closed := false
	for closeDeadline := time.Now().Add(60 * time.Second); time.Now().Before(closeDeadline); {
		got, err := client.Task(ctx, task.ID)
		if err == nil && got.State == model.StateClosed {
			closed = true
			break
		}
		time.Sleep(2 * time.Second)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() returned an error after being cancelled: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run() did not return within 30s of its context being cancelled")
	}

	if !closed {
		t.Fatalf("task %s never read back as closed over the REST API after its pull request was merged", task.ID)
	}
	if len(sim.sim.Issues) != 0 {
		t.Fatalf("expected no GitHub issues at all, got %+v", sim.sim.Issues)
	}
}
