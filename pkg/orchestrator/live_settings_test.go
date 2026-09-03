package orchestrator_test

// The settings a cycle itself is the consumer of, changed while the
// daemon runs. Deps.MaxWorkers has its own test alongside the rest of
// the concurrency ones (concurrent_dispatch_test.go); this is its
// sibling, Config.MaxAgentTurns, which reaches the agent as
// agent.RunConfig.MaxTurns and so is observable at the far end of a real
// dispatch.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// turnsFramework reports the turn cap each dispatch handed it, so a test
// can see which value actually reached the agent rather than which one
// the caller's own Deps still says.
type turnsFramework struct{ turns chan int }

func (f *turnsFramework) Run(_ context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	f.turns <- cfg.MaxTurns
	return toolResult(agent.ToolCall{
		Name:      "comment_on_issue",
		Arguments: map[string]any{"comment": "done"},
	}), nil
}

// A change to max-agent-turns made through the store -- `grain
// settings`, or the UI's Settings page -- takes effect on the next
// dispatch, with no restart and no change to the Deps a long-lived
// daemon process already built, exactly as max-workers does.
func TestACycleAdoptsAMaxAgentTurnsChangeFromTheStoreWithoutRestart(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)
	_, client := newSim(t, "acme", "widgets", "main")

	fw := &turnsFramework{turns: make(chan int, 4)}
	deps := orchestrator.Deps{
		Store:     store,
		Client:    client,
		Sandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		Framework: orchestrator.StaticFramework(fw),
		// What this process started with -- the flag that seeded the row,
		// or the value stored the last time it was restarted.
		Config:     orchestrator.Config{MaxAgentTurns: 10},
		MaxWorkers: 1,
	}

	if err := store.PutConfig(ctx, model.Config{MaxWorkers: 1, MaxAgentTurns: 40}); err != nil {
		t.Fatalf("PutConfig: %v", err)
	}

	// With no InFlight to park it in, the cycle waits for the run it
	// starts, so the cap the agent was handed is on the channel by the
	// time this returns.
	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	select {
	case got := <-fw.turns:
		if got != 40 {
			t.Fatalf("the agent was given a %d-turn budget, want the stored 40 rather than the 10 this "+
				"process started with", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no agent ran")
	}
}

// promptFramework is turnsFramework's counterpart for the prompt: it
// reports the prompt each dispatch was built with, which is where the
// deployment-wide prompt extension ends up (grain/task-114).
type promptFramework struct{ prompts chan string }

func (f *promptFramework) Run(_ context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	f.prompts <- cfg.Prompt
	return toolResult(agent.ToolCall{
		Name:      "comment_on_issue",
		Arguments: map[string]any{"comment": "done"},
	}), nil
}

// The same "no restart" property for the deployment-wide prompt
// extension: an operator who changes what every run is told in Settings
// is changing the next run, not the next process. It is worth its own
// test rather than trusting the field's neighbour above, because this
// one is only live if RunCycle actually refreshes it -- a Config field
// read straight from whatever the daemon started with would pass every
// other test this package has.
func TestACycleAdoptsAPromptExtensionChangeFromTheStoreWithoutRestart(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)
	_, client := newSim(t, "acme", "widgets", "main")

	fw := &promptFramework{prompts: make(chan string, 4)}
	deps := orchestrator.Deps{
		Store:     store,
		Client:    client,
		Sandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		Framework: orchestrator.StaticFramework(fw),
		// What this process started with -- in practice nothing at all,
		// since no daemon flag seeds this one.
		Config:     orchestrator.Config{},
		MaxWorkers: 1,
	}

	const text = "Run `make lint` before you push."
	if err := store.PutConfig(ctx, model.Config{MaxWorkers: 1, PromptExtension: text}); err != nil {
		t.Fatalf("PutConfig: %v", err)
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}

	select {
	case got := <-fw.prompts:
		if !strings.Contains(got, text) {
			t.Fatalf("the agent's prompt does not carry the stored standing instructions: %q", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no agent ran")
	}
}
