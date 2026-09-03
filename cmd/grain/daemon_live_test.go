package main

// TestRunLiveDispatchesAndOpensAPullRequest is the daemon's own version
// of tests/e2e/live_test.go's TestLiveIssueCompletesEndToEnd and
// pkg/orchestrator/live_test.go's TestRunCycleCompletesEndToEnd: the
// closest thing to actually running `grain daemon` this repo can check
// in. It calls run() itself -- the exact function daemon() (daemon.go,
// what main() calls for a "daemon" argv[1]) calls, not a stand-in --
// against a real embedded SQLite store, a real in-process git proxy, a
// real *github.RESTClient pointed at a local server standing in for
// GitHub (git smart-HTTP plus the handful of REST endpoints
// orchestrate.openPullRequest needs, served by githubsim.Sim), and the
// real Gemini API. Gated on GEMINI_API_KEY exactly like the other two,
// so it runs nowhere without a live key (including CI) and costs nothing
// when skipped:
//
//	GEMINI_API_KEY=... go test ./cmd/grain/... -run TestRunLiveDispatchesAndOpensAPullRequest -v
//
// One gap this test works around rather than hides: neither
// pkg/orchestrate.prepare() nor pkg/orchestrator.BuildPrompt tells a
// dispatched agent what URL to clone -- the sandbox's git credentials
// are scoped to the git proxy's own (ephemeral, local) host, which
// nothing puts in the prompt (see both tests/e2e/live_test.go's and
// pkg/orchestrator/live_test.go's own prompts, which hardcode the remote
// they built themselves for the same reason). Filed as its own issue
// rather than fixed here, since the daemon's run() is what this file
// tests, not pkg/orchestrate's prompt-building. The prompt below routes
// around it exactly the way a real dispatched agent would have to today:
// by reading its own ~/.git-credentials to discover the one host it has
// been handed a credential for.

import (
	"bytes"
	"context"
	"fmt"
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

	"github.com/bwsalmon/grain/pkg/agent/antigravity"
	"github.com/bwsalmon/grain/pkg/github"
	"github.com/bwsalmon/grain/pkg/github/githubsim"
	"github.com/bwsalmon/grain/pkg/model"
)

// syncedSim serializes this test's own reads of a *githubsim.Sim's
// fields against the daemon's concurrent requests into it. Sim.Request
// takes the Sim's own lock now that a run outlives the cycle that
// started it (orchestrator.InFlight) and several runs really do reach
// one client at once; what that lock does not cover is a test reading
// sim.PullRequests directly while an httptest.Server's request-handling
// goroutines are inside Request, which is what this wrapper is for.
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

// githubHostServer serves one httptest.Server standing in for
// cfg.githubHost: REST API calls (/repos/...) go to sim, everything else
// -- the actual git smart-HTTP traffic the git proxy forwards -- goes to
// a real `git http-backend` over gitRoot, the same technique
// tests/e2e/harness_test.go's own gitHTTPBackend uses (duplicated here
// rather than exported, that file's own comment explains why).
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
	mux.Handle("/", gitHTTPBackendLive(gitRoot))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

// gitHTTPBackendLive serves projectRoot over the smart-HTTP CGI contract
// git itself defines, by shelling out to `git http-backend` per request
// -- duplicated from tests/e2e/harness_test.go's gitHTTPBackend rather
// than exported (that file's own comment on why: an internal test
// helper in another package's _test.go is not importable, and this
// codebase prefers the duplication to a shared exported helper for
// exactly that reason).
func gitHTTPBackendLive(projectRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cmd := exec.Command("git", "http-backend")
		cmd.Env = append(os.Environ(),
			"GIT_PROJECT_ROOT="+projectRoot,
			"GIT_HTTP_EXPORT_ALL=1",
			"PATH_INFO="+r.URL.Path,
			"QUERY_STRING="+r.URL.RawQuery,
			"REQUEST_METHOD="+r.Method,
			"CONTENT_TYPE="+r.Header.Get("Content-Type"),
			"GATEWAY_INTERFACE=CGI/1.1",
			"SERVER_PROTOCOL=HTTP/1.1",
		)
		cmd.Stdin = bytes.NewReader(body)
		out, err := cmd.Output()
		if err != nil {
			http.Error(w, fmt.Sprintf("git http-backend: %v", err), http.StatusInternalServerError)
			return
		}

		sep := []byte("\r\n\r\n")
		headEnd := bytes.Index(out, sep)
		if headEnd == -1 {
			sep = []byte("\n\n")
			headEnd = bytes.Index(out, sep)
		}
		headerBlock := string(out[:headEnd])
		respBody := out[headEnd+len(sep):]

		status := http.StatusOK
		for _, line := range strings.Split(headerBlock, "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			key, value, _ := strings.Cut(line, ":")
			value = strings.TrimSpace(value)
			if strings.EqualFold(key, "Status") {
				if code, err := strconv.Atoi(strings.Fields(value)[0]); err == nil {
					status = code
				}
				continue
			}
			w.Header().Add(key, value)
		}
		w.WriteHeader(status)
		w.Write(respBody)
	}
}

func runLive(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func TestRunLiveDispatchesAndOpensAPullRequest(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live grain daemon integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	const owner, repoName = "acme", "widgets"
	taskID := "graind-live"
	branch := model.BranchName(taskID)

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

	// Seed the task before starting the daemon: run() opens its own
	// store from cfg.dataDir and dispatches on its very first tick, with
	// no hook for a test to insert a task in between, so it has to
	// already be there -- the same "-data-dir already has work queued"
	// shape a real restart finds.
	store, db, err := openStore(dataDir)
	if err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	task := model.Task{
		ID:     taskID,
		Intent: model.IntentImplement,
		Title:  "graind live test",
		Body: "Call your run_command tool exactly once with the single shell command below " +
			"(it already includes every step: discovering the one git host your sandbox has " +
			"credentials for, cloning, branching, editing, committing and pushing), then reply " +
			"with a short confirmation once it succeeds. Do not run anything else first.\n\n" +
			"HOST=$(sed -n 's#.*@\\([^/]*\\).*#\\1#p' ~/.git-credentials) && " +
			"git clone http://$HOST/" + owner + "/" + repoName + ".git work && " +
			"cd work && git checkout -b " + branch + " && " +
			"echo 'graind live test' >> NOTES.md && " +
			"git add NOTES.md && git commit -q -m 'graind live test' && " +
			"git push origin " + branch,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "tester"}},
			Reason:      model.ReasonDirect,
		},
		Binding: model.BindingDirective,
		Target:  &model.RepoRef{Owner: owner, Name: repoName},
	}
	task.Approval = &model.Attribution{Actor: task.Origin.Attribution.Actor}
	if err := store.PutTask(context.Background(), task); err != nil {
		db.Close()
		t.Fatalf("seeding the task: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing the seeding connection: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, config{
			dataDir: dataDir, sandboxDir: t.TempDir(), maxWorkers: 1, pollInterval: 5 * time.Second,
			geminiAPIKeyFile: writeKeyFile(t, apiKey), geminiModel: antigravity.DefaultModel, maxAgentTurns: 15,
			githubHost: githubHost, githubInsecureHTTP: true,
		})
	}()

	// Poll the real bare repo -- not sim, so no lock needed here -- for
	// the branch a successful live run pushes.
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
	// in the same tick (cycle.go's runOne, through finish.go's
	// ProcessResult), so give it a
	// short grace window rather than assuming it already happened the
	// instant the branch appeared.
	prOpened := false
	for deadline2 := time.Now().Add(20 * time.Second); time.Now().Before(deadline2); {
		if sim.pullRequestCount() > 0 {
			prOpened = true
			break
		}
		time.Sleep(1 * time.Second)
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

	if !prOpened {
		t.Fatalf("branch %s was pushed but no pull request was opened for it", branch)
	}
}

func writeKeyFile(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gemini-api-key")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
