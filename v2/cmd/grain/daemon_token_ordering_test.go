package main

// TestSandboxTokenMintedBeforeGitProxyStartsAuthenticates is a fast,
// keyless regression test for a real bug run()'s own ordering had until
// bwsalmon/agents#265: gitproxy.BuildProxy's own doc comment says the
// proxy's SandboxTokens "loads the map once at startup and only ever
// looks tokens up" -- it never rereads sandbox-tokens.json. run() used
// to call startGitProxy before minting any slot's token, which means the
// running proxy's token map was permanently whatever was on disk before
// any slot existed (nothing, on a fresh -data-dir): every token minted
// afterward -- i.e. every token any real deployment would ever have --
// was one the proxy could never recognize, so every git push through it
// failed closed with 401 "authentication required," for the entire
// life of the process. daemon_live_test.go's
// TestRunLiveDispatchesAndOpensAPullRequest is what actually caught this
// (a live dispatched agent's push was rejected every time); this test
// isolates the same mechanism without a live Gemini key, so it runs in
// CI and fails fast if the ordering regresses.
//
// This drives the same three calls run() itself makes -- openStore,
// NewSandboxTokenStore.EnsureToken, startGitProxy, mcp.ConfigureGitCredentials
// -- in the order run() now uses (token minted first), and then performs
// the exact clone/branch/commit/push sequence a dispatched agent would,
// by hand, against the resulting proxy.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/dispatch"
	"github.com/bwsalmon/grain/v2/pkg/github/githubsim"
	"github.com/bwsalmon/grain/v2/pkg/gitproxy"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

func TestSandboxTokenMintedBeforeGitProxyStartsAuthenticates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	const owner, repoName, slot = "acme", "widgets", "local"

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
	store, db, err := openStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	task := model.Task{
		ID:     "token-ordering-check",
		Intent: model.IntentImplement,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "tester"}},
			Reason:      model.ReasonDirect,
		},
		Binding: model.BindingDirective,
		Target:  &model.RepoRef{Owner: owner, Name: repoName},
	}
	task.Approval = &model.Attribution{Actor: task.Origin.Attribution.Actor}
	if err := store.PutTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	// gitproxy's Authorizer reads the sandbox's live dispatched task, so
	// this slot needs a real Dispatch on record, not just a queued task.
	if dispatches, err := dispatch.Cycle(context.Background(), store, []string{slot}, time.Now().UTC()); err != nil || len(dispatches) != 1 {
		t.Fatalf("expected exactly one dispatch, got %v (err=%v)", dispatches, err)
	}

	// The order under test: mint the token, *then* start the proxy --
	// matching run()'s own fixed order in main.go.
	tokens := gitproxy.NewSandboxTokenStore(filepath.Join(dataDir, "secrets", "sandbox-tokens.json"))
	token, err := tokens.EnsureToken(slot)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, stop, err := startGitProxy(dataDir, store, githubHost, true)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())

	root := t.TempDir()
	if err := mcp.ConfigureGitCredentials(root, proxyURL+"/placeholder/placeholder.git", token); err != nil {
		t.Fatal(err)
	}

	branch := model.BranchName(task.ID)
	script := "HOST=$(sed -n 's#.*@\\([^/]*\\).*#\\1#p' ~/.git-credentials) && " +
		"git clone http://$HOST/" + owner + "/" + repoName + ".git work && " +
		"cd work && git checkout -b " + branch + " && " +
		"echo 'token ordering check' >> NOTES.md && " +
		"git add NOTES.md && git commit -q -m 'token ordering check' && " +
		"git push origin " + branch

	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("push through the proxy failed even though the token was minted before the proxy started: %v\n%s", err, out)
	}
}
