package orchestrator_test

// Which framework a dispatch asks for, and what happens when it cannot
// be built. Deps.Framework used to be a bare factory taking nothing: one
// framework, chosen at daemon startup, for every run. It takes the
// task's own model.Task.AgentFramework now, and may refuse -- the
// credential it needs is set from the UI and may simply not be there
// yet.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

func TestDispatchAsksForTheTaskOwnAgentFramework(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}

	task := filedTask(t, ctx, store, "t1", repo)
	task.AgentFramework = model.AgentFrameworkClaude
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	var asked []string
	deps := orchestrator.Deps{
		Store:     store,
		Client:    client,
		Sandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		Framework: func(_ context.Context, framework string) (agent.Framework, error) {
			asked = append(asked, framework)
			return stubFramework{result: toolResult(agent.ToolCall{
				Name:      "comment_on_issue",
				Arguments: map[string]any{"comment": "done"},
			})}, nil
		},
		MaxConcurrent: 1,
	}
	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	if len(asked) != 1 || asked[0] != model.AgentFrameworkClaude {
		t.Fatalf("Deps.Framework was asked for %v, want one call for %q", asked, model.AgentFrameworkClaude)
	}
}

func TestDispatchAsksForTheDeploymentDefaultWhenATaskNamesNoFramework(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	var asked []string
	deps := orchestrator.Deps{
		Store:     store,
		Client:    client,
		Sandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		Framework: func(_ context.Context, framework string) (agent.Framework, error) {
			asked = append(asked, framework)
			return stubFramework{result: toolResult(agent.ToolCall{
				Name:      "comment_on_issue",
				Arguments: map[string]any{"comment": "done"},
			})}, nil
		},
		MaxConcurrent: 1,
	}
	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	// "" is what a task with no override says, and resolving it into a
	// real framework is deliberately the factory's job (cmd/grain's own
	// defaultAgentFramework), not this package's: only the caller knows
	// what this deployment's default currently is.
	if len(asked) != 1 || asked[0] != "" {
		t.Fatalf("Deps.Framework was asked for %v, want one call for the empty (default) framework", asked)
	}
}

func TestDispatchFinishesTheRunAsSetupFailedWhenTheFrameworkCannotBeBuilt(t *testing.T) {
	store, ctx := openStore(t)
	_, client := newSim(t, "acme", "widgets", "main")
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	task := filedTask(t, ctx, store, "t1", repo)

	missingKey := errors.New("no Claude Code OAuth token is configured: set one in the UI")
	deps := orchestrator.Deps{
		Store:     store,
		Client:    client,
		Sandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		Framework: func(context.Context, string) (agent.Framework, error) {
			return nil, missingKey
		},
		MaxConcurrent: 1,
	}
	err := orchestrator.RunCycle(ctx, deps, baseTime)
	if !errors.Is(err, missingKey) {
		t.Fatalf("RunCycle error = %v, want it to carry the framework failure", err)
	}

	// The run must be finished, not left open: the point of building the
	// framework inside runOne's setup guard is that a deployment missing
	// a key ends up with a run an operator can read the reason off,
	// rather than a task that silently never moves.
	runs, err := store.Runs(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].FinishedAt == nil {
		t.Fatal("the run was left open after its framework could not be built")
	}
	if runs[0].Outcome != "setup-failed" {
		t.Fatalf("outcome = %q, want setup-failed", runs[0].Outcome)
	}
	if !strings.Contains(runs[0].Detail, "set one in the UI") {
		t.Fatalf("detail = %q, want it to carry the reason the framework could not be built", runs[0].Detail)
	}
}
