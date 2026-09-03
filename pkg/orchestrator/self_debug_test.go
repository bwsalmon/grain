// Whether RunDispatch tells a run that its task holds the self-debug
// grant, and where grain's own source is -- the two facts a subprocess
// Framework turns into its forked mcpserver's -self-debug/-grain-src-dir
// (agent.SelfDebugArgs). Unlike Config.GrantTools next door, which hands
// a run in-process tools no CLI framework can consume, this pair is what
// actually reaches a dispatched agent, so it is worth a test of its own.
package orchestrator_test

import (
	"context"
	"sync"
	"testing"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// selfDebugCapturingFramework records what one run was told about the
// self-debug grant, then finishes the way toolCapturingFramework does.
type selfDebugCapturingFramework struct {
	mu        sync.Mutex
	selfDebug bool
	sourceDir string
}

func (f *selfDebugCapturingFramework) Run(_ context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	f.mu.Lock()
	f.selfDebug, f.sourceDir = cfg.SelfDebug, cfg.GrainSourceDir
	f.mu.Unlock()
	return toolResult(agent.ToolCall{Name: "comment_on_issue", Arguments: map[string]any{"comment": "done"}}), nil
}

func (f *selfDebugCapturingFramework) seen() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.selfDebug, f.sourceDir
}

func runWithGrants(t *testing.T, grants []model.Grant, sourceDir string) (bool, string) {
	t.Helper()
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)
	task.Grants = grants
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	framework := &selfDebugCapturingFramework{}
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
	selfDebug, sourceDir := runWithGrants(t, grants, "/usr/local/share/grain/src")
	if !selfDebug {
		t.Error("RunConfig.SelfDebug = false, want true for a task holding the grant")
	}
	if sourceDir != "/usr/local/share/grain/src" {
		t.Errorf("RunConfig.GrainSourceDir = %q, want the deployment's own source checkout", sourceDir)
	}
}

// Not gated on Interactive, unlike Config.GrantTools: that gate exists
// because selfrepair's tools need a human watching the chat, and nothing
// self-debug offers asks anyone anything. filedTask's own task is not
// interactive, so the test above already proves this; this one proves the
// other half -- a task without the grant is told nothing, including where
// grain's source is.
func TestRunCycleTellsAnOrdinaryTaskNothingAboutSelfDebug(t *testing.T) {
	selfDebug, sourceDir := runWithGrants(t, nil, "/usr/local/share/grain/src")
	if selfDebug {
		t.Error("RunConfig.SelfDebug = true, want false for a task without the grant")
	}
	if sourceDir != "" {
		t.Errorf("RunConfig.GrainSourceDir = %q, want it withheld from a task without the grant", sourceDir)
	}
}
