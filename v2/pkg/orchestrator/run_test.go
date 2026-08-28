package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/dispatch"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// agentFunc adapts a plain function to agent.Framework, so each test can
// script exactly the run it wants without a scripted Gemini transcript --
// ported from pkg/orchestrate's own test helper (bwsalmon/agents#254) when
// that package's capability-handling tests moved here (bwsalmon/agents#263).
type agentFunc func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error)

func (f agentFunc) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	return f(ctx, cfg)
}

func pushed() *agent.Result {
	return &agent.Result{
		FinalText: "pushed the change",
		ToolCalls: []agent.ToolCall{{Name: "run_command", Text: "ok"}},
	}
}

// fakeCapability is a model.CapabilityProvider a test configures to
// refuse, or to mint a lease and a placement, recording every Revoke call
// it gets. Ported from pkg/orchestrate's own test helper (bwsalmon/agents#254).
type fakeCapability struct {
	name    string
	refuse  string // non-empty means Resolve refuses with this reason
	path    string // placement path (absolute, like a real provider's)
	content string

	mu      sync.Mutex
	revoked []model.Lease
}

func (c *fakeCapability) Spec() model.CapabilitySpec {
	return model.CapabilitySpec{Name: c.name, Provision: model.ProvisionMint}
}

func (c *fakeCapability) Resolve(ctx context.Context, cc model.CapabilityContext) (model.Resolution, error) {
	if c.refuse != "" {
		return model.RefusedBecause(c.refuse), nil
	}
	return model.Honoured(), nil
}

func (c *fakeCapability) Materialize(ctx context.Context, cc model.CapabilityContext) (model.Materialization, error) {
	return model.Materialization{
		Lease: &model.Lease{Capability: c.name, Resource: "res-1", MintedBy: model.CredentialRef{Name: "test"}, IssuedAt: cc.Now},
		Placements: []model.Placement{
			{Side: model.SideSandbox, Path: c.path, Content: c.content},
		},
	}, nil
}

func (c *fakeCapability) PromptSection(ctx context.Context, cc model.CapabilityContext, placements []model.Placement) (string, error) {
	return "capability " + c.name + " is ready", nil
}

func (c *fakeCapability) Revoke(ctx context.Context, cc model.CapabilityContext, lease model.Lease) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked = append(c.revoked, lease)
	return nil
}

// dispatchTask puts an approved (human-filed) task directly, standing in
// for what dispatch.Cycle would already have found ready by the time
// RunDispatch runs -- these tests are about what RunDispatch does with a
// task's own Grants, not about approval or scheduling.
func dispatchTask(t *testing.T, ctx context.Context, store *model.Store, id string, grants ...model.Grant) model.Task {
	t.Helper()
	human := model.Principal{Kind: model.PrincipalHuman, ID: "alice"}
	task := model.Task{
		ID: id, Intent: model.IntentImplement, Title: "Do the thing", Body: "details",
		Origin:   model.Origin{Attribution: model.Attribution{Actor: human}, Reason: model.ReasonDirect},
		Approval: &model.Attribution{Actor: human},
		Target:   &model.RepoRef{Owner: "acme", Name: "widgets"},
		Binding:  model.BindingDirective,
		Grants:   grants,
	}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("filing task %s: %v", id, err)
	}
	return task
}

// startRun records the task_run row dispatch.Cycle would already have written
// by the time RunDispatch ever sees a Dispatch -- RunDispatch only ever
// UPDATEs it (via store.FinishRun), the same "the run is already durable"
// assumption pkg/orchestrate's own runDispatch documented.
func startRun(t *testing.T, ctx context.Context, store *model.Store, d dispatch.Dispatch, at time.Time) {
	t.Helper()
	if err := store.StartRun(ctx, model.Run{
		ID: d.RunID, TaskID: d.TaskID, Slot: d.Slot, Sandbox: d.Slot, Attempt: d.Attempt, StartedAt: at,
	}); err != nil {
		t.Fatalf("starting run %s: %v", d.RunID, err)
	}
}

func TestRunDispatchMaterializesAppliesPromptsAndRevokesACapability(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1", model.Grant{Capability: "keyed", Via: model.GrantByLabel})
	d := dispatch.Dispatch{TaskID: "t1", Slot: "local", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	root := t.TempDir()
	var gotPrompt string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt = cfg.Prompt
		return pushed(), nil
	})
	cap := &fakeCapability{name: "keyed", path: "/home/debian/.secret", content: "sh-sh-sh"}
	cfg := orchestrator.Config{Capabilities: model.NewCapabilityRegistry(cap)}

	result, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, root, baseTime)
	if err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	if result == nil {
		t.Fatal("expected a result")
	}

	if want := "capability keyed is ready"; !strings.Contains(gotPrompt, want) {
		t.Errorf("prompt %q does not mention %q", gotPrompt, want)
	}
	placed := filepath.Join(root, "home/debian/.secret")
	data, err := os.ReadFile(placed)
	if err != nil {
		t.Fatalf("placement was not written to %s: %v", placed, err)
	}
	if string(data) != "sh-sh-sh" {
		t.Errorf("placement content = %q", data)
	}

	if len(cap.revoked) != 1 || cap.revoked[0].Resource != "res-1" {
		t.Fatalf("revoked = %+v, want exactly one lease for res-1", cap.revoked)
	}
	live, err := store.LiveLeases(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("live leases after revoke = %+v, want none", live)
	}
}

func TestRunDispatchFinishesTheRunAsFailedWhenACapabilityIsRefused(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1", model.Grant{Capability: "locked", Via: model.GrantByLabel})
	d := dispatch.Dispatch{TaskID: "t1", Slot: "local", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	ran := false
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		ran = true
		return pushed(), nil
	})
	cap := &fakeCapability{name: "locked", refuse: "not for you"}
	cfg := orchestrator.Config{Capabilities: model.NewCapabilityRegistry(cap)}

	if _, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), baseTime); err == nil {
		t.Fatal("expected RunDispatch to report the refusal")
	}
	if ran {
		t.Fatal("agent must not run when a capability was refused")
	}

	// A failed run still gets finished -- an unfinished one would hold its
	// slot forever -- and returns the task straight to queued, for a
	// retry, the same semantics e2e's TestFailedRunReturnsTaskToQueueForRetry
	// exercises one layer up through a real push denial instead.
	occupied, err := store.OccupiedSlots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(occupied) != 0 {
		t.Errorf("occupied slots after a refused capability = %v, want none", occupied)
	}
	state, err := store.State(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateQueued {
		t.Errorf("state = %q, want %q", state, model.StateQueued)
	}
}

func TestRunDispatchFailsARunThatMadeNoToolCall(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", Slot: "local", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return &agent.Result{FinalText: "nothing to do here"}, nil
	})

	result, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), baseTime)
	if err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	if result == nil || len(result.ToolCalls) != 0 {
		t.Fatalf("result = %+v, want the agent's own (tool-call-less) result back", result)
	}

	occupied, err := store.OccupiedSlots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(occupied) != 0 {
		t.Errorf("occupied slots after a tool-call-less run = %v, want none", occupied)
	}
	state, err := store.State(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if state != model.StateQueued {
		t.Errorf("state = %q, want %q (a failed run with nothing observed is eligible for retry)", state, model.StateQueued)
	}
}
