// TestCLICreatesIssueAgentOpensPRAndUserMergeClosesIt is bwsalmon/agents#278's
// own scenario: file a task the way an operator actually would -- through
// the real grain CLI binary, not a seeded store row or a hand-built
// githubsim.Issue -- then drive it through a real orchestrator.RunCycle (a
// scripted agent pushing over a real local git server, standing in for the
// sandboxed agent that generates the code) until grain opens a real pull
// request, then have a second, independent github.Client submit that pull
// request by merging it for real -- both the git-level merge, over the same
// local git server, and the GitHub-level PUT .../merge call -- standing in
// for a human clicking "Merge pull request" the way the CLI itself already
// stands in for a human filing the issue. The loop closing is confirmed by
// reading the issue back through the grain CLI itself, not through the
// store, since that is the one thing every other test in this package
// already checks a different way.
//
// This is the one file in package e2e that builds a real github.Client --
// harness_test.go's own package doc comment explains why every other file
// here doesn't. A subprocess CLI has no Go values to share with the test
// process, so the only way for it to see the same GitHub state RunCycle
// mutates is a real HTTP server in front of a real Sim, the same rig
// cmd/graind/live_test.go already built for the daemon's own subprocess-style
// live test -- duplicated here (syncedSim, githubHostServer) for the same
// reason every other package in this codebase duplicates small test helpers
// rather than exporting them.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/agent/gemini"
	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/github/githubsim"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

// syncedSim serializes every call into a *githubsim.Sim: the real Sim
// carries no lock of its own, fine when one goroutine drives it directly (as
// every other test in this package does), but this test drives it from an
// httptest.Server's own request-handling goroutines -- the grain CLI
// subprocess's REST calls and the "user"'s own merge call -- rather than
// only the test's own goroutine.
type syncedSim struct {
	mu  sync.Mutex
	sim *githubsim.Sim
}

func (s *syncedSim) Request(method, path string, headers map[string]string, body []byte) (github.ApiResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sim.Request(method, path, headers, body)
}

func (s *syncedSim) pullRequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sim.PullRequests)
}

func (s *syncedSim) firstPullRequestNumber() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sim.PullRequests[0].Number
}

// githubHostServer serves one httptest.Server standing in for a
// -github-host: REST API calls (/repos/...) go to sim, everything else --
// the real git smart-HTTP traffic the scripted agent's clone/push and the
// simulated user's own merge push both use -- goes to this package's own
// gitHTTPBackend (harness_test.go), reused directly since both live in
// package e2e.
func githubHostServer(t *testing.T, sim *syncedSim, gitRoot string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		headers := map[string]string{}
		for k := range r.Header {
			headers[k] = r.Header.Get(k)
		}
		apiResp, err := sim.Request(r.Method, r.URL.RequestURI(), headers, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for k, v := range apiResp.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(apiResp.Status)
		w.Write(apiResp.Body)
	})
	mux.Handle("/", gitHTTPBackend(gitRoot))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

// buildGrainCLI compiles the real cmd/grain binary once for the test -- the
// point of this file is proving the actual CLI a human runs at a shell files
// the issue and reads it back, not a Go call into pkg/ui that happens to
// share its code.
func buildGrainCLI(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}
	bin := filepath.Join(t.TempDir(), "grain")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/bwsalmon/grain/v2/cmd/grain")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building grain CLI: %v\n%s", err, out)
	}
	return bin
}

// runCLI runs the built grain binary and returns its stdout, failing the
// test with stderr attached on a non-zero exit. stdout is kept separate from
// stderr so -json output always parses cleanly.
func runCLI(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("grain %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// openCLIStore opens a real embedded Dolt store in a temp directory, the
// same discipline every other package's own tests hold to -- duplicated from
// pkg/orchestrator's own openStore (an unexported _test.go helper, not
// importable) rather than this package's own newWorld, which bundles a
// gitproxy this test has no use for.
func openCLIStore(t *testing.T) (*model.Store, context.Context) {
	t.Helper()
	db, err := dolt.Open(dolt.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded dolt: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	return store, ctx
}

// scriptedFramework turns a scripted response sequence into the
// func() agent.Framework factory orchestrator.Deps wants, one fresh
// gemini.NewForTest per dispatch -- duplicated from pkg/orchestrator's own
// live_test.go helper of the same name for the same reason openCLIStore is.
func scriptedFramework(script []*genai.GenerateContentResponse) func() agent.Framework {
	return func() agent.Framework { return gemini.NewForTest(&scriptedGenerator{responses: script}) }
}

func TestCLICreatesIssueAgentOpensPRAndUserMergeClosesIt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	bin := buildGrainCLI(t)

	const owner, repoName = "acme", "widgets"
	upstream := t.TempDir()
	bare := filepath.Join(upstream, owner, repoName+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "git", "init", "--bare", "-b", "main", bare)
	run(t, upstream, "git", "-C", bare, "config", "http.receivepack", "true")
	seedParent := t.TempDir()
	run(t, seedParent, "git", "clone", bare, "seed")
	seed := filepath.Join(seedParent, "seed")
	run(t, seed, "git", "config", "user.email", "seed@example.com")
	run(t, seed, "git", "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(seed, "NOTES.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "git", "add", "NOTES.md")
	run(t, seed, "git", "commit", "-q", "-m", "initial commit")
	run(t, seed, "git", "push", "origin", "main")

	sim := &syncedSim{sim: githubsim.New(owner, repoName, bare, "main")}
	githubHost := githubHostServer(t, sim, upstream)

	// Step 1: create the task the way an operator actually would, through
	// the real CLI binary against the fake GitHub host, approved
	// immediately so it carries the trigger label from the moment it's
	// filed (ui.Client.CreateTask's own behavior for Approved: true).
	created := runCLI(t, bin,
		"-task-repo", owner+"/"+repoName,
		"-github-host", githubHost,
		"-github-insecure-http",
		"-json",
		"create",
		"-title", "add a NOTES entry",
		"-body", "please add a line to NOTES.md",
		"-repo", owner+"/"+repoName,
		"-approve",
	)
	var task ui.Task
	if err := json.Unmarshal([]byte(created), &task); err != nil {
		t.Fatalf("parsing grain create -json output: %v\n%s", err, created)
	}
	if task.Number == 0 {
		t.Fatalf("grain create did not return an issue number: %s", created)
	}

	// Step 2: the agent generates the code and grain opens the PR --
	// orchestrator.RunCycle polls the very issue the CLI just filed,
	// dispatches it to a scripted agent that clones and pushes over the
	// same local git server the CLI's issue points at, and opens a real
	// pull request against sim once the push lands.
	store, ctx := openCLIStore(t)
	repo := model.RepoRef{Owner: owner, Name: repoName}
	cfg := orchestrator.Config{TaskRepo: repo, TriggerLabel: "grain-agent"}
	sandboxes := orchestrator.NewHostSandboxes(t.TempDir())
	const slot = "cli-e2e-1"
	root, err := sandboxes.RootFor(slot)
	if err != nil {
		t.Fatal(err)
	}
	remote := "http://" + githubHost + "/" + owner + "/" + repoName + ".git"
	if err := mcp.ConfigureGitCredentials(root, remote, "unused"); err != nil {
		t.Fatal(err)
	}

	taskID := orchestrator.TaskID(repo, task.Number)
	branch := model.BranchName(taskID)
	client := github.NewClient(sim, nil)
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: sandboxes, Config: cfg, Slots: []string{slot},
		Framework: scriptedFramework(pushScript(remote, branch, taskID)),
	}
	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle (dispatch): %v", err)
	}

	st, err := store.State(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateCompleted {
		t.Fatalf("state after the agent's push = %q, want completed", st)
	}
	if sim.pullRequestCount() != 1 {
		t.Fatalf("expected grain to have opened one pull request, got %d", sim.pullRequestCount())
	}
	prNumber := sim.firstPullRequestNumber()

	// Step 3: the user submits the PR -- a real git merge over the same
	// local git server the agent pushed to, plus a real HTTP PUT to the
	// merge endpoint, driven by a second, independent github.Client the
	// same way the grain CLI itself is a client independent of the
	// orchestrator's own, standing in for a human clicking "Merge pull
	// request" on GitHub.
	mergeParent := t.TempDir()
	run(t, mergeParent, "git", "clone", remote, "merge")
	mergeWd := filepath.Join(mergeParent, "merge")
	run(t, mergeWd, "git", "config", "user.email", "github@example.com")
	run(t, mergeWd, "git", "config", "user.name", "github (simulated merge)")
	run(t, mergeWd, "git", "fetch", "origin", branch)
	run(t, mergeWd, "git", "checkout", "main")
	run(t, mergeWd, "git", "merge", "--no-ff", "origin/"+branch, "-m", "Merge "+branch)
	run(t, mergeWd, "git", "push", "origin", "main")

	userTransport := github.NewRealTransport(githubHost)
	userTransport.UseTLS = false
	userClient := github.NewClient(userTransport, nil)
	if err := userClient.MergePullRequest(owner, repoName, prNumber); err != nil {
		t.Fatalf("submitting (merging) the pull request: %v", err)
	}

	// Step 4: another cycle syncs the now-merged PR and closes the linked
	// issue -- SyncPullRequests' own job, the same second RunCycle
	// pkg/orchestrator/live_test.go's TestRunCycleCompletesEndToEnd makes.
	if err := orchestrator.RunCycle(ctx, deps, baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("RunCycle (sync): %v", err)
	}
	st, err = store.State(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateClosed {
		t.Fatalf("state after the merge = %q, want closed", st)
	}

	// Step 5: confirm the loop closed the way an operator would actually
	// see it -- by asking the CLI itself, not by reading the store.
	got := runCLI(t, bin,
		"-task-repo", owner+"/"+repoName,
		"-github-host", githubHost,
		"-github-insecure-http",
		"-json",
		"get", strconv.Itoa(task.Number),
	)
	var detail ui.TaskDetail
	if err := json.Unmarshal([]byte(got), &detail); err != nil {
		t.Fatalf("parsing grain get -json output: %v\n%s", err, got)
	}
	if detail.GitHubState != "closed" {
		t.Fatalf("issue #%d githubState = %q after the merge, want closed", task.Number, detail.GitHubState)
	}
}
