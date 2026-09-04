// Which of its task's grants RunDispatch tells a run it holds, and where
// grain's own source is -- the facts a subprocess Framework turns into
// its forked mcpserver's -grant/-grain-src-dir (agent.GrantArgs). Unlike
// Config.GrantTools next door, which hands a run in-process tools no CLI
// framework can consume, this is what actually reaches a dispatched
// agent, so it is worth a test of its own.
package orchestrator_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// grantCapturingFramework records what one run was told about its own
// grants, then finishes the way toolCapturingFramework does.
type grantCapturingFramework struct {
	mu        sync.Mutex
	grants    []string
	sourceDir string
}

func (f *grantCapturingFramework) Run(_ context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	f.mu.Lock()
	f.grants, f.sourceDir = cfg.Grants, cfg.GrainSourceDir
	f.mu.Unlock()
	return toolResult(agent.ToolCall{Name: "comment_on_issue", Arguments: map[string]any{"comment": "done"}}), nil
}

func (f *grantCapturingFramework) seen() ([]string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grants, f.sourceDir
}

func runWithGrants(t *testing.T, grants []model.Grant, sourceDir string) ([]string, string) {
	t.Helper()
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.Grants = grants
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	framework := &grantCapturingFramework{}
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		Framework:  orchestrator.StaticFramework(framework),
		Config:     orchestrator.Config{GrainSourceDir: sourceDir},
		MaxWorkers: 1,
	}
	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	return framework.seen()
}

func TestRunCycleTellsASelfDebugTaskItHoldsTheGrant(t *testing.T) {
	grants := []model.Grant{{Capability: "self-debug", Via: model.GrantByLabel}}
	held, sourceDir := runWithGrants(t, grants, "/usr/local/share/grain/src")
	if !slices.Contains(held, "self-debug") {
		t.Errorf("RunConfig.Grants = %v, want the self-debug grant among them", held)
	}
	if sourceDir != "/usr/local/share/grain/src" {
		t.Errorf("RunConfig.GrainSourceDir = %q, want the deployment's own source checkout", sourceDir)
	}
}

// The bootstrap-playbooks grant travels the same road, which is the
// whole point of there being one road: its playbooks are embedded in the
// binary the forked mcpserver is, so the name of the grant is everything
// that has to reach it.
func TestRunCycleTellsABootstrapTaskItHoldsTheGrant(t *testing.T) {
	grants := []model.Grant{{Capability: "bootstrap-playbooks", Via: model.GrantByLabel}}
	held, sourceDir := runWithGrants(t, grants, "/usr/local/share/grain/src")
	if !slices.Contains(held, "bootstrap-playbooks") {
		t.Errorf("RunConfig.Grants = %v, want the bootstrap-playbooks grant among them", held)
	}
	// Where grain's source lives is self-debug's to know, and this task
	// does not hold that grant.
	if sourceDir != "" {
		t.Errorf("RunConfig.GrainSourceDir = %q, want it withheld from a task without self-debug", sourceDir)
	}
}

// Two tool-only grants on one task: both reach the same run at once.
func TestRunCycleCarriesEveryToolGrantATaskHolds(t *testing.T) {
	grants := []model.Grant{
		{Capability: "bootstrap-playbooks", Via: model.GrantByLabel},
		{Capability: "self-debug", Via: model.GrantByLabel},
	}
	held, _ := runWithGrants(t, grants, "/usr/local/share/grain/src")
	for _, want := range []string{"self-debug", "bootstrap-playbooks"} {
		if !slices.Contains(held, want) {
			t.Errorf("RunConfig.Grants = %v, want %s among them", held, want)
		}
	}
}

// Only the grants that decide a tool roster travel. A capability that
// mints credentials is placed in the sandbox by its own Materialize, and
// has no business being named in a subprocess's arguments.
func TestRunCycleCarriesOnlyToolGrants(t *testing.T) {
	grants := []model.Grant{{Capability: "gcp-key", Via: model.GrantByLabel}}
	held, _ := runWithGrants(t, grants, "/usr/local/share/grain/src")
	if len(held) != 0 {
		t.Errorf("RunConfig.Grants = %v, want none for a task holding no tool-granting capability", held)
	}
}

// Not gated on Interactive, unlike Config.GrantTools: that gate exists
// because selfrepair's tools need a human watching the chat, and nothing
// either of these grants offers asks anyone anything. filedTask's own
// task is not interactive, so the tests above already prove this; this
// one proves the other half -- a task without a grant is told nothing,
// including where grain's source is.
func TestRunCycleTellsAnOrdinaryTaskNothingAboutItsGrants(t *testing.T) {
	held, sourceDir := runWithGrants(t, nil, "/usr/local/share/grain/src")
	if len(held) != 0 {
		t.Errorf("RunConfig.Grants = %v, want none for a task holding no grant", held)
	}
	if sourceDir != "" {
		t.Errorf("RunConfig.GrainSourceDir = %q, want it withheld from a task without the grant", sourceDir)
	}
}
