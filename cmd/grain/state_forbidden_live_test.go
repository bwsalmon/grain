package main

// Adopting a state repository under a *running* daemon, from the git
// proxy's point of view.
//
// forbiddenRepos answers one question -- does this deployment's state
// repository carry grain's encrypted secrets file, and must the proxy
// therefore refuse it to every sandbox (state_secrets_test.go) -- and
// startGitProxy asks it once, when the daemon starts. That was the whole
// of it, which left the gap this file guards: the Settings pane's State
// tab and `grain state adopt` both point an installation at a different
// repository, and the pane does it while the daemon runs, so a
// deployment that adopted a secrets-carrying repository went on serving
// it to every sandbox until somebody restarted the process.
//
// The proxy here is a real one, serving on a real port, and the request
// is the one a sandbox's own `git clone` makes.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/dispatch"
	"github.com/bwsalmon/grain/pkg/gitproxy"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/staterepo"
	"github.com/bwsalmon/grain/pkg/ui"
)

func TestAdoptingASecretsCarryingRepositoryIsRefusedWithoutARestart(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	const owner, name = "acme", "grain-state"

	// The repository about to be adopted: a real remote whose history
	// holds secrets.enc, exactly as one written by a build that kept the
	// file in the state repository does. Removed from the tip, and still
	// in every clone -- which is the point.
	remote := bareRemoteHoldingSecrets(t, owner, name)

	// A deployment that is already up: a local-only state repository
	// (nothing forbidden), a store, and a git proxy serving against it.
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets", "github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "github", "credentials.json"),
		[]byte(`{"*": "anonymous"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, db, err := openStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// A sandbox with a live run whose target is that same repository --
	// the case that has to be refused, since a task's own Target is what
	// would otherwise allow the fetch that carries the ciphertext out.
	sandbox := dispatchedSandboxTargeting(t, store, model.RepoRef{Owner: owner, Name: name})

	// Somewhere for an allowed request to be forwarded to, so that
	// "served" and "refused" are told apart by what the proxy did rather
	// than by which flavour of 403 came back.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("upstream"))
	}))
	defer upstream.Close()

	forbidden := gitproxy.NewForbiddenSet()
	proxyURL, stopProxy, err := startGitProxy(dataDir, store, upstream.Listener.Addr().String(), true, "", forbidden)
	if err != nil {
		t.Fatalf("starting the git proxy: %v", err)
	}
	defer stopProxy(ctx)

	tokens := gitproxy.NewSandboxTokenStore(filepath.Join(dataDir, "secrets", "sandbox-tokens.json"))
	token, err := tokens.EnsureToken(sandbox)
	if err != nil {
		t.Fatal(err)
	}

	// Before the adopt this deployment's state lives nowhere in
	// particular, so the repository is an ordinary target and the proxy
	// serves it.
	status, body := fetchThroughProxy(t, proxyURL, token, owner, name)
	if status != http.StatusOK {
		t.Fatalf("fetch before the adopt = %d (%q), want the task's own target served", status, body)
	}

	// The adopt itself, through the manager the Settings pane's State tab
	// talks to -- the daemon still running, the proxy still serving.
	repo, err := openStateRepo(ctx, dataDir)
	if err != nil {
		t.Fatalf("opening the state repository: %v", err)
	}
	manager := newStateManager(dataDir, db, repo, openSecrets(dataDir), forbidden)
	if _, err := manager.Adopt(ctx, ui.AdoptRequest{Remote: remote}); err != nil {
		t.Fatalf("adopting %s: %v", remote, err)
	}

	status, body = fetchThroughProxy(t, proxyURL, token, owner, name)
	if status != http.StatusForbidden {
		t.Fatalf("fetch after the adopt = %d (%q), want 403: the adopted state repository carries "+
			"grain's encrypted secrets file and must be refused without waiting for a restart", status, body)
	}
	if !strings.Contains(body, "secrets") {
		t.Errorf("the refusal does not say why it is one: %q", body)
	}
}

// Adopting a repository that has never held the file is the other half:
// a deployment already refusing its own state repository has to stop
// refusing it once it is no longer the one grain's state lives in, or
// the fix for the condition would itself need a restart.
func TestAdoptingACleanRepositoryLiftsTheRefusalWithoutARestart(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	const owner, name = "acme", "grain-state"

	// A deployment whose state repository already carries the file, and
	// a proxy started against it: the refusal is in place from the start
	// here, not acquired later.
	dataDir := t.TempDir()
	stateRepoWithSecrets(t, dataDir, true)
	store, db, err := openStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sandbox := dispatchedSandboxTargeting(t, store, model.RepoRef{Owner: owner, Name: name})

	forbidden := gitproxy.NewForbiddenSet()
	proxyURL, stopProxy, err := startGitProxy(dataDir, store, "127.0.0.1:1", true, "", forbidden)
	if err != nil {
		t.Fatalf("starting the git proxy: %v", err)
	}
	defer stopProxy(ctx)

	tokens := gitproxy.NewSandboxTokenStore(filepath.Join(dataDir, "secrets", "sandbox-tokens.json"))
	token, err := tokens.EnsureToken(sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := fetchThroughProxy(t, proxyURL, token, owner, name); status != http.StatusForbidden {
		t.Fatalf("fetch at startup = %d (%q), want the secrets-carrying repository refused", status, body)
	}

	repo, err := openStateRepo(ctx, dataDir)
	if err != nil {
		t.Fatalf("opening the state repository: %v", err)
	}
	manager := newStateManager(dataDir, db, repo, openSecrets(dataDir), forbidden)
	if _, err := manager.Adopt(ctx, ui.AdoptRequest{Remote: bareRemote(t)}); err != nil {
		t.Fatalf("adopting an empty repository: %v", err)
	}

	// The forward target above is a port nothing is listening on, so an
	// allowed request fails at the forward (502) rather than succeeding.
	// That is enough: what is under test is that it got past the refusal
	// at all.
	status, body := fetchThroughProxy(t, proxyURL, token, owner, name)
	if status == http.StatusForbidden {
		t.Fatalf("fetch after adopting a repository that never held the secrets file = %d (%q), "+
			"want the refusal lifted without a restart", status, body)
	}
}

// bareRemoteHoldingSecrets is a repository somebody could adopt: a bare
// remote at <owner>/<name>.git whose history holds secrets.enc and whose
// tip does not, which is what this build's own migration leaves behind
// (moveSecretsOutOfStateRepo) and what no sandbox may be allowed to
// clone.
func bareRemoteHoldingSecrets(t *testing.T, owner, name string) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), owner, name+".git")
	if err := os.MkdirAll(filepath.Dir(remote), 0o755); err != nil {
		t.Fatal(err)
	}
	runLive(t, "", "git", "init", "--bare", "--initial-branch=main", remote)

	seed := filepath.Join(t.TempDir(), "seed")
	runLive(t, "", "git", "clone", remote, seed)
	runLive(t, seed, "git", "config", "user.email", "seed@example.com")
	runLive(t, seed, "git", "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(seed, staterepo.SecretsFile), []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLive(t, seed, "git", "add", staterepo.SecretsFile)
	runLive(t, seed, "git", "commit", "-q", "-m", "an older build's secrets file")
	runLive(t, seed, "git", "rm", "-q", staterepo.SecretsFile)
	runLive(t, seed, "git", "commit", "-q", "-m", "move it out of the repository")
	runLive(t, seed, "git", "push", "-q", "origin", "main")
	return remote
}

// dispatchedSandboxTargeting puts a live run on record whose target is
// target, and returns the sandbox id it holds -- what gitproxy's
// Authorizer resolves a request's scope through, and what makes an
// allowed request here allowed for the strongest possible reason.
func dispatchedSandboxTargeting(t *testing.T, store *model.Store, target model.RepoRef) string {
	t.Helper()
	ctx := context.Background()
	task := model.Task{
		ID:     "forbidden-repo-check",
		Intent: model.IntentImplement,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "tester"}},
			Reason:      model.ReasonDirect,
		},
		Binding: model.BindingDirective,
		Target:  &target,
	}
	task.Approval = &model.Attribution{Actor: task.Origin.Attribution.Actor}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	dispatches, err := dispatch.Cycle(ctx, store, model.Limits{Workers: 1}, time.Now().UTC())
	if err != nil || len(dispatches) != 1 {
		t.Fatalf("expected exactly one dispatch, got %v (err=%v)", dispatches, err)
	}
	if err := store.SetRunSandbox(ctx, dispatches[0].RunID, dispatches[0].RunID); err != nil {
		t.Fatal(err)
	}
	return dispatches[0].RunID
}

// fetchThroughProxy makes the request a sandbox's own `git clone` makes
// first, authenticated as that sandbox, and reports what came back.
func fetchThroughProxy(t *testing.T, proxyURL, token, owner, repo string) (int, string) {
	t.Helper()
	url := fmt.Sprintf("%s/%s/%s.git/info/refs?service=git-upload-pack", proxyURL, owner, repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "git/2.39.2")
	req.Header.Set("Accept", "*/*")
	req.SetBasicAuth("grain", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("requesting %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}
