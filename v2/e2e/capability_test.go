// TestCLIAttachedCapabilityIsMaterializedAppliedAndRevokedThroughRunCycle is
// bwsalmon/agents#329's own scenario: prepareCapabilities -> applyPlacements
// -> agent run -> revokeAll, exercised through a real dispatch -> agent ->
// gitproxy-free push pipeline instead of pkg/orchestrator/run_test.go's own
// TestRunDispatchMaterializesAppliesPromptsAndRevokesACapability, which
// proves the same four moments without ever going through a real
// orchestrator.RunCycle or a real grain CLI subprocess.
//
// The capability itself is a small stand-in registered by this test, not
// the real gcpkey/geminikey providers -- those need real GCP/Gemini
// credentials this sandbox should not assume it has. It is attached to the
// task through the real CLI's `capability <id> <cap> attach`, under the one
// capability ID (self-debug) cmd/grain/main.go's ui.DefaultCapabilities
// allow-list accepts that needs no such credential, exactly the way an
// operator would toggle it on -- the orchestrator.Config.Capabilities
// registry that actually resolves and materializes it, by contrast, is
// this test's own, so nothing here ever calls out to GCP or Gemini.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	"github.com/bwsalmon/grain/v2/pkg/github"
	"github.com/bwsalmon/grain/v2/pkg/github/githubsim"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

// testCapabilityProvider is a model.CapabilityProvider standing in for
// gcpkey/geminikey, mirroring the stand-ins pkg/model/capability_test.go
// already builds: Materialize mints one SideSandbox placement and a Lease,
// and Revoke records that it was actually called.
type testCapabilityProvider struct {
	name, path, content string

	mu      sync.Mutex
	revoked bool
}

func (p *testCapabilityProvider) Spec() model.CapabilitySpec {
	return model.CapabilitySpec{Name: p.name, Provision: model.ProvisionMint}
}

func (p *testCapabilityProvider) Resolve(context.Context, model.CapabilityContext) (model.Resolution, error) {
	return model.Honoured(), nil
}

func (p *testCapabilityProvider) Materialize(ctx context.Context, cc model.CapabilityContext) (model.Materialization, error) {
	return model.Materialization{
		Lease: &model.Lease{
			Capability: p.name, Resource: "test-resource",
			MintedBy: model.CredentialRef{Name: "test"}, IssuedAt: cc.Now,
		},
		Placements: []model.Placement{
			{Side: model.SideSandbox, Path: p.path, Content: p.content},
		},
	}, nil
}

func (p *testCapabilityProvider) PromptSection(context.Context, model.CapabilityContext, []model.Placement) (string, error) {
	return "capability " + p.name + " is ready", nil
}

func (p *testCapabilityProvider) Revoke(context.Context, model.CapabilityContext, model.Lease) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoked = true
	return nil
}

func (p *testCapabilityProvider) wasRevoked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.revoked
}

// capabilityPushScript is pushScript (harness_test.go) with one extra tool
// call spliced in front of it: a run_command that reads placementPath back
// and checks its content, proving applyPlacements really wrote the
// capability's material into the sandbox root before the agent ever
// touches git, not merely that the run went on to succeed regardless.
// placementPath is always absolute, as a real provider's Path always is
// (see applyPlacements' own doc comment); the command reads it relative to
// the sandbox root instead, since run_command's cmd.Dir is root and an
// absolute path in the command string would instead name a real path on
// this test process's own host.
func capabilityPushScript(remote, branch, taskID, placementPath, wantContent string) []*genai.GenerateContentResponse {
	relPath := strings.TrimPrefix(placementPath, "/")
	readBack := fmt.Sprintf("test \"$(cat %s)\" = %q", relPath, wantContent)
	push := "git clone " + remote + " work && cd work && " +
		"git checkout -b " + branch + " && " +
		"echo 'change for " + taskID + "' >> NOTES.md && " +
		"git add NOTES.md && git commit -q -m 'agent commit for " + taskID + "' && " +
		"git push origin " + branch
	return []*genai.GenerateContentResponse{
		toolCall("run_command", map[string]any{"command": readBack}),
		toolCall("run_command", map[string]any{"command": push}),
		finalText("read the capability's placement and pushed " + branch),
	}
}

func TestCLIAttachedCapabilityIsMaterializedAppliedAndRevokedThroughRunCycle(t *testing.T) {
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

	// Step 1: file the task, then attach a capability to it, both through
	// the real CLI binary -- not a seeded store row and not a hand-built
	// Grant -- the way an operator actually would. self-debug is the
	// capability ID because it is on cmd/grain/main.go's own
	// ui.DefaultCapabilities allow-list and needs no real credential; the
	// registry that actually resolves and materializes it below is this
	// test's own, never the real one a deployment would wire up for it.
	storeDir := t.TempDir()
	created := runCLI(t, bin,
		"-data-dir", storeDir,
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
	if task.ID == "" {
		t.Fatalf("grain create did not return a task id: %s", created)
	}

	const capabilityID = "self-debug"
	attached := runCLI(t, bin, "-data-dir", storeDir, "-json", "capability", task.ID, capabilityID, "attach")
	var afterAttach ui.Task
	if err := json.Unmarshal([]byte(attached), &afterAttach); err != nil {
		t.Fatalf("parsing grain capability attach -json output: %v\n%s", err, attached)
	}
	attachedOK := false
	for _, c := range afterAttach.Capabilities {
		if c == capabilityID {
			attachedOK = true
		}
	}
	if !attachedOK {
		t.Fatalf("capabilities after attach = %v, want %q among them", afterAttach.Capabilities, capabilityID)
	}

	// Step 2: dispatch it through a real RunCycle. reconcileDispatch reads
	// the Grant the CLI just wrote, RunDispatch resolves and materializes
	// it against this test's own provider, applyPlacements writes its
	// lease into the sandbox root before the agent's first turn, the
	// scripted agent reads that placement back and then pushes as normal,
	// and revokeAll tears the lease down again once the run finishes.
	sandboxes := orchestrator.NewHostSandboxes(t.TempDir())
	const slot = "cap-e2e-1"
	root, err := sandboxes.RootFor(slot)
	if err != nil {
		t.Fatal(err)
	}
	remote := "http://" + githubHost + "/" + owner + "/" + repoName + ".git"
	if err := mcp.ConfigureGitCredentials(root, remote, "unused"); err != nil {
		t.Fatal(err)
	}

	const placementPath = "/secrets/self-debug-token"
	const placementContent = "sh-sh-sh-e2e"
	provider := &testCapabilityProvider{name: capabilityID, path: placementPath, content: placementContent}

	branch := model.BranchName(task.ID)
	client := github.NewClient(sim, nil)
	deps := orchestrator.Deps{
		Client: client, Sandboxes: sandboxes, Slots: []string{slot},
		Framework: scriptedFramework(capabilityPushScript(remote, branch, task.ID, placementPath, placementContent)),
		Config:    orchestrator.Config{Capabilities: model.NewCapabilityRegistry(provider)},
	}

	withStore(t, storeDir, func(store *model.Store, ctx context.Context) {
		deps.Store = store
		if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
			t.Fatalf("RunCycle (dispatch): %v", err)
		}
		st, err := store.State(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st != model.StateCompleted {
			t.Fatalf("state after the agent's push = %q, want completed", st)
		}

		live, err := store.LiveLeases(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(live) != 0 {
			t.Errorf("live leases after the run = %+v, want none -- store.DropLease should have cleared it", live)
		}
	})

	if !provider.wasRevoked() {
		t.Error("provider.Revoke was never called -- revokeAll should have called it once the run finished")
	}

	// The placement really did land under the sandbox root, not just get
	// read back successfully by the scripted agent's own bash -- confirmed
	// independently of the agent's tool calls, the same way harness_test.go's
	// own assertions read back the upstream repo rather than trusting the
	// agent's report of what it did.
	placed := filepath.Join(root, strings.TrimPrefix(placementPath, "/"))
	data, err := os.ReadFile(placed)
	if err != nil {
		t.Fatalf("placement was not written to %s: %v", placed, err)
	}
	if string(data) != placementContent {
		t.Errorf("placement content = %q, want %q", data, placementContent)
	}

	if sim.pullRequestCount() != 1 {
		t.Fatalf("expected grain to have opened one pull request, got %d", sim.pullRequestCount())
	}
}
