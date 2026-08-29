// Package e2e ties every layer v2 owns today into one pipeline: a task
// filed the way a human would, dispatch.Cycle deciding when it runs, an agent
// (agent/gemini) actually driving it, and gitproxy actually authorizing
// and forwarding the git push that results -- against a real embedded
// SQLite store and a real (local, git http-backend) stand-in for GitHub.
// That is the same discipline gitproxy/live_test.go already holds to one
// layer down; what this package adds is dispatch.Cycle's own dispatch
// decision, more than one task/run, and the state transitions that
// follow a run (completed, awaiting_reply, closed), not just the push
// itself.
//
// v2/README.md's "What this does not have yet" section is why this
// package stops where it does: no host adapter means no real sandbox VM
// (NewSandboxTools' root stands in, as it does everywhere else in v2 —
// see world.roots), and this harness builds no github.Client at all, so
// "the PR opened" and "the issue closed" are simulated with the same
// store.Observe calls model/simulate_test.go's GitHub-sync helpers
// already use, rather than a real API response. A merge is simulated the
// same way: the test itself merges the pushed branch with a plain `git
// merge`, standing in for GitHub's own merge button. pkg/orchestrator/
// live_test.go is the same two scenarios this file proves by hand, driven
// instead through a real github.Client against githubsim -- a real
// CreatePullRequest, and a real close-out once githubsim reports the PR
// merged, rather than either being simulated here.
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/agent/gemini"
	"github.com/bwsalmon/grain/v2/pkg/dispatch"
	"github.com/bwsalmon/grain/v2/pkg/gitproxy"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
)

var baseTime = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

func human(id string) model.Principal { return model.Principal{Kind: model.PrincipalHuman, ID: id} }
func bot(id string) model.Principal   { return model.Principal{Kind: model.PrincipalAutomation, ID: id} }

// world is everything one test needs standing in for the outside world: a
// real store, a real "upstream" (the GitHub stand-in, one directory of
// bare repos served over smart HTTP), and a real gitproxy in front of it,
// authorized off the store the same way the production proxy would be.
type world struct {
	t           *testing.T
	store       *model.Store
	ctx         context.Context
	upstreamDir string
	proxyURL    string
	roots       map[string]string // slot -> its sandbox-stand-in directory
}

// newWorld builds one world with a fixed slot pool, credentialed up
// front exactly as a long-lived sandbox's git config would be at
// provisioning time -- see mcp/git_credentials.go's own docstring on why
// that only needs doing once per slot, not once per task.
func newWorld(t *testing.T, slots []string) *world {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}

	upstreamDir := t.TempDir()
	backend := newTestServer(t, gitHTTPBackend(upstreamDir))
	backendHost := mustHost(t, backend)

	dataDir := t.TempDir()
	mustWriteFile(t, filepath.Join(dataDir, "secrets", "github", "credentials.json"), `{"*": "anonymous"}`)

	tokens := map[string]string{}
	for _, s := range slots {
		tokens[s] = s + "-token"
	}
	tokensJSON, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dataDir, "secrets", "sandbox-tokens.json"), string(tokensJSON))

	proxy, err := gitproxy.BuildProxy(gitproxy.BuildConfig{
		DataDir: dataDir, Store: store, ForwardHost: backendHost, ForwardTLS: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := newTestServer(t, gitproxy.NewHandler(proxy))

	w := &world{
		t: t, store: store, ctx: ctx, upstreamDir: upstreamDir,
		proxyURL: proxyURL, roots: map[string]string{},
	}
	for _, s := range slots {
		root := t.TempDir()
		// The path in this dummy remote is never used -- only its scheme
		// and host are, to build the credential-store line -- so any repo
		// name stands in here even though this slot may later run tasks
		// against several different repos through the same proxy.
		if err := mcp.ConfigureGitCredentials(root, proxyURL+"/placeholder/placeholder.git", tokens[s]); err != nil {
			t.Fatalf("configuring git credentials for %s: %v", s, err)
		}
		w.roots[s] = root
	}
	return w
}

// newRepo creates a bare repo at upstream/owner/name.git seeded with one
// commit on main -- this world's stand-in for a real GitHub repo the
// controller already knows about.
func (w *world) newRepo(owner, name string) {
	w.t.Helper()
	bare := filepath.Join(w.upstreamDir, owner, name+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		w.t.Fatal(err)
	}
	run(w.t, w.upstreamDir, "git", "init", "--bare", "-b", "main", bare)
	// GIT_HTTP_EXPORT_ALL only covers the read side; git http-backend
	// denies git-receive-pack (push) by default even with it set --
	// matching gitproxy/live_test.go's own note on the same rig.
	run(w.t, w.upstreamDir, "git", "-C", bare, "config", "http.receivepack", "true")

	seedParent := w.t.TempDir()
	run(w.t, seedParent, "git", "clone", bare, "seed")
	seed := filepath.Join(seedParent, "seed")
	run(w.t, seed, "git", "config", "user.email", "seed@example.com")
	run(w.t, seed, "git", "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		w.t.Fatal(err)
	}
	run(w.t, seed, "git", "add", "README.md")
	run(w.t, seed, "git", "commit", "-q", "-m", "initial commit")
	run(w.t, seed, "git", "push", "origin", "main")
}

// remote is the proxy URL a sandbox's agent would clone/push against for
// owner/name -- what a real dispatch would write into its prompt or
// discover by running `git remote -v` after a real host adapter clones
// the repo for it.
func (w *world) remote(owner, name string) string {
	return w.proxyURL + "/" + owner + "/" + name + ".git"
}

// runDispatch drives one dispatch.Cycle Dispatch to completion in its slot's
// sandbox-stand-in directory, through a scripted (not live) gemini
// agent, and calls FinishRun once the agent's turn ends. It does not
// touch task_observation -- that is the GitHub-sync stand-in's job,
// applied by the caller from the returned result, the same separation
// model/simulate_test.go's components hold to.
func (w *world) runDispatch(d dispatch.Dispatch, script []*genai.GenerateContentResponse, at time.Time) *agent.Result {
	w.t.Helper()
	root := w.roots[d.Slot]
	if root == "" {
		w.t.Fatalf("dispatch landed on slot %q, which this world never credentialed", d.Slot)
	}
	fw := gemini.NewForTest(&scriptedGenerator{responses: script})
	result, err := fw.Run(w.ctx, agent.RunConfig{Prompt: "work the task", SandboxRoot: root})
	if err != nil {
		w.t.Fatalf("agent run for %s failed outright: %v", d.RunID, err)
	}
	outcome := "succeeded"
	for _, c := range result.ToolCalls {
		if c.IsError {
			outcome = "failed"
		}
	}
	if err := w.store.FinishRun(w.ctx, d.RunID, at, outcome, ""); err != nil {
		w.t.Fatalf("FinishRun(%s): %v", d.RunID, err)
	}
	return result
}

// askedQuestion returns the question argument of the first ask_question
// call in result, if any -- the harness's only way to see what the
// mocked escape-hatch tool recorded, since gemini.Framework.Run's own
// MockSink is internal and discarded when Run returns (mcp/mock_tools.go);
// result.ToolCalls is the seam v2 leaves for exactly this.
func askedQuestion(result *agent.Result) (string, bool) {
	for _, c := range result.ToolCalls {
		if c.Name == "ask_question" {
			if q, ok := c.Arguments["question"].(string); ok {
				return q, true
			}
			return "", true
		}
	}
	return "", false
}

// pushedOK reports whether result's tool calls look like a clean
// clone/commit/push -- at least one non-error run_command call and no
// error at all.
func pushedOK(result *agent.Result) bool {
	sawRunCommand := false
	for _, c := range result.ToolCalls {
		if c.IsError {
			return false
		}
		if c.Name == "run_command" {
			sawRunCommand = true
		}
	}
	return sawRunCommand
}

// mergeBranchIntoDefault simulates GitHub's own merge button: a plain
// git merge of branch into defaultBranch, performed directly against the
// bare upstream repo -- nothing in v2 owns this action (v2/README.md,
// "What this does not have yet: TrackedPullRequest ... anything that
// reads or writes GitHub's REST API"), so a test standing in for GitHub
// is the only way to exercise the git side of "the PR merged."
func (w *world) mergeBranchIntoDefault(owner, name, branch, defaultBranch string) {
	w.t.Helper()
	bare := filepath.Join(w.upstreamDir, owner, name+".git")
	dir := w.t.TempDir()
	run(w.t, dir, "git", "clone", bare, "merge")
	wd := filepath.Join(dir, "merge")
	run(w.t, wd, "git", "config", "user.email", "github@example.com")
	run(w.t, wd, "git", "config", "user.name", "github (simulated merge)")
	run(w.t, wd, "git", "fetch", "origin", branch)
	run(w.t, wd, "git", "checkout", defaultBranch)
	run(w.t, wd, "git", "merge", "--no-ff", "origin/"+branch, "-m", "Merge "+branch)
	run(w.t, wd, "git", "push", "origin", defaultBranch)
}

// branchExists reports whether ref exists in owner/name's bare repo.
func (w *world) branchExists(owner, name, ref string) bool {
	bare := filepath.Join(w.upstreamDir, owner, name+".git")
	cmd := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify", ref)
	return cmd.Run() == nil
}

// branchContains reports whether ref's history already includes
// ancestor -- how a check that "the merge really landed" tells that
// apart from "a same-named ref exists but was never actually merged."
func (w *world) branchContains(owner, name, ref, ancestor string) bool {
	bare := filepath.Join(w.upstreamDir, owner, name+".git")
	cmd := exec.Command("git", "--git-dir", bare, "merge-base", "--is-ancestor", ancestor, ref)
	return cmd.Run() == nil
}

// log1 is `git log -1 --format=format` against owner/name's bare repo at
// ref, trimmed -- how every assertion below reads back what actually
// landed in the upstream stand-in, independent of anything the model or
// proxy believes happened.
func (w *world) log1(owner, name, ref, format string) string {
	w.t.Helper()
	bare := filepath.Join(w.upstreamDir, owner, name+".git")
	return strings.TrimSpace(runOutput(w.t, w.upstreamDir, "git", "--git-dir", bare, "log", "-1", "--format="+format, ref))
}

// --- small scripting helpers for the gemini agent, mirroring
// gitproxy/live_test.go's own (package-private, deliberately
// duplicated -- see that file's comment on why) ---------------------

type scriptedGenerator struct {
	responses []*genai.GenerateContentResponse
	calls     int
}

func (g *scriptedGenerator) GenerateContent(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	if g.calls >= len(g.responses) {
		g.calls++
		return nil, nil
	}
	resp := g.responses[g.calls]
	g.calls++
	return resp, nil
}

func finalText(text string) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Content: genai.NewContentFromText(text, genai.RoleModel)}},
	}
}

func toolCall(name string, args map[string]any) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Content: genai.NewContentFromFunctionCall(name, args, genai.RoleModel)}},
	}
}

// pushScript is the scripted turn a "succeeding" agent run takes: clone
// the task's target repo, make a small change identifying itself by
// taskID, and push it to a fresh branch -- never main, matching
// model.BranchName's own contract that a task's work lands on its own
// branch.
func pushScript(remote, branch, taskID string) []*genai.GenerateContentResponse {
	cmd := "git clone " + remote + " work && cd work && " +
		"git checkout -b " + branch + " && " +
		"echo 'change for " + taskID + "' >> NOTES.md && " +
		"git add NOTES.md && git commit -q -m 'agent commit for " + taskID + "' && " +
		"git push origin " + branch
	return []*genai.GenerateContentResponse{
		toolCall("run_command", map[string]any{"command": cmd}),
		finalText("pushed " + branch),
	}
}

// failScript is a run that never touches git at all, so a retry after it
// never risks colliding with a branch a real push would have made.
func failScript(reason string) []*genai.GenerateContentResponse {
	return []*genai.GenerateContentResponse{
		toolCall("run_command", map[string]any{"command": "echo '" + reason + "' >&2; exit 1"}),
		finalText("could not complete the task: " + reason),
	}
}

// askScript has the agent call the ask_question escape hatch instead of
// touching git -- mcp/mock_tools.go's own docstring: "this ends your
// turn ... do not take any further actions," which this script honors by
// following it with nothing but a short close-out turn, matching how a
// human reading the tool's own contract would expect it to behave.
func askScript(question string) []*genai.GenerateContentResponse {
	return []*genai.GenerateContentResponse{
		toolCall("ask_question", map[string]any{"question": question}),
		finalText("waiting on a reply"),
	}
}

// --- generic process/HTTP plumbing, duplicated per-package the same
// way gitproxy/live_test.go duplicates it rather than sharing a testutil
// package nothing else in v2 has yet ---------------------------------

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func runOutput(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x:" + token))
}

// newTestServer starts h on an httptest server, registers its own
// cleanup, and returns its URL -- a one-line version of the
// httptest.NewServer/defer Close() pair every caller here would
// otherwise repeat.
func newTestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

// mustHost strips httptest.NewServer's fixed "http://" scheme, since
// gitproxy.BuildConfig.ForwardHost wants a bare host:port -- RealForwarder
// (gitproxy/forward.go) prepends the scheme itself from ForwardTLS.
func mustHost(t *testing.T, rawURL string) string {
	t.Helper()
	host, ok := strings.CutPrefix(rawURL, "http://")
	if !ok {
		t.Fatalf("expected an http:// URL from httptest.NewServer, got %q", rawURL)
	}
	return host
}

// gitHTTPBackend serves projectRoot over the smart-HTTP CGI contract git
// itself defines, by shelling out to `git http-backend` per request --
// the same technique gitproxy/live_test.go's gitHTTPBackend (and, before
// it, v1's tests/test_vm_integration.py) uses to stand in for GitHub
// without a live credential. Duplicated here rather than exported from
// gitproxy_test, which is an internal test package another package
// cannot import.
func gitHTTPBackend(projectRoot string) http.HandlerFunc {
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
