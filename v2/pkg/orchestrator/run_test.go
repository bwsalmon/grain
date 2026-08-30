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

// BuildPrompt tells the agent about a read-only repo, but the wording
// makes clear it grants nothing beyond a fetch -- gitproxy/authorize.go
// is what actually refuses a push to one; this only informs.
func TestBuildPromptMentionsReadOnlyRepos(t *testing.T) {
	task := model.Task{
		ID: "t1", Title: "Do the thing", Body: "details",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"},
		Reads: []model.RepoRef{
			{Owner: "acme", Name: "shared-lib"},
			{Owner: "acme", Name: "schema"},
		},
	}
	prompt := orchestrator.BuildPrompt(task)
	if !strings.Contains(prompt, "acme/shared-lib") || !strings.Contains(prompt, "acme/schema") {
		t.Fatalf("prompt does not mention both read-only repos: %q", prompt)
	}
	if !strings.Contains(prompt, "never push") {
		t.Fatalf("prompt does not warn against pushing to a read-only repo: %q", prompt)
	}
}

func TestBuildPromptOmitsReadsSectionWhenThereAreNone(t *testing.T) {
	task := model.Task{
		ID: "t1", Title: "Do the thing", Body: "details",
		Target: &model.RepoRef{Owner: "acme", Name: "widgets"},
	}
	prompt := orchestrator.BuildPrompt(task)
	if strings.Contains(prompt, "read") {
		t.Fatalf("prompt mentions reading a repo with no Reads set: %q", prompt)
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

// TestRunDispatchRecordsTheAgentsTranscript covers bwsalmon/agents#446:
// a framework's own agent.Result.Transcript should end up readable back
// off the store, against the task and attempt number RunDispatch was
// given -- the one thing FinishRun's own outcome/detail never carried.
func TestRunDispatchRecordsTheAgentsTranscript(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", Slot: "local", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return &agent.Result{
			FinalText:  "pushed the change",
			ToolCalls:  []agent.ToolCall{{Name: "run_command", Text: "ok"}},
			Transcript: "> run_command(...)\nok\n\npushed the change",
		}, nil
	})
	cfg := orchestrator.Config{}

	if _, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}

	transcript, found, err := store.RunTranscript(ctx, "t1", 1)
	if err != nil || !found {
		t.Fatalf("RunTranscript: (%q, %v, %v)", transcript, found, err)
	}
	if !strings.Contains(transcript, "pushed the change") {
		t.Errorf("transcript = %q, want it to contain the agent's own transcript text", transcript)
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

// TestRunDispatchIncludesTheCommentThreadOnARedispatch is
// bwsalmon/agents#402's own scenario: a run parks on ask_question,
// ProcessResult relays that question into the store the same way a real
// dispatch would, a human answers it exactly as `grain comment` or the UI
// would, and the task's second dispatch must see both -- otherwise the
// redispatched agent has no way to know its question was already
// answered and asks it again, forever. This drives two full RunDispatch
// calls (with a real ProcessResult and a real store-backed comment
// between them) rather than calling BuildPrompt directly, since the bug
// this reproduces was in what RunDispatch's caller never did, not in
// BuildPrompt itself.
func TestRunDispatchIncludesTheCommentThreadOnARedispatch(t *testing.T) {
	store, ctx := openStore(t)
	sim, client := newSim(t, "acme", "widgets", "main")
	_ = sim
	task := dispatchTask(t, ctx, store, "t1")
	task.Target = &model.RepoRef{Owner: "acme", Name: "widgets"}
	if err := store.PutTask(ctx, task); err != nil {
		t.Fatalf("re-filing task with a real target: %v", err)
	}

	d1 := dispatch.Dispatch{TaskID: "t1", Slot: "local", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d1, baseTime)

	const question = "should the new field be snake_case or camelCase?"
	fw1 := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		return &agent.Result{ToolCalls: []agent.ToolCall{
			{Name: "ask_question", Arguments: map[string]any{"question": question}},
		}}, nil
	})
	got, err := store.GetTask(ctx, "t1")
	if err != nil || got == nil {
		t.Fatalf("reading task: %v", err)
	}
	result1, err := orchestrator.RunDispatch(ctx, store, fw1, orchestrator.Config{}, *got, d1, nil, t.TempDir(), baseTime)
	if err != nil {
		t.Fatalf("first RunDispatch: %v", err)
	}
	if err := orchestrator.ProcessResult(ctx, store, client, *got, result1, d1.RunID, baseTime); err != nil {
		t.Fatalf("ProcessResult after the question: %v", err)
	}

	const answer = "snake_case, to match the rest of the schema"
	if _, err := store.AddComment(ctx, model.Comment{
		TaskID:    "t1",
		Author:    model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "alice"}},
		Body:      answer,
		CreatedAt: baseTime.Add(time.Minute),
	}); err != nil {
		t.Fatalf("answering the question: %v", err)
	}

	d2 := dispatch.Dispatch{TaskID: "t1", Slot: "local", RunID: "r2", Attempt: 2}
	startRun(t, ctx, store, d2, baseTime.Add(2*time.Minute))
	got2, err := store.GetTask(ctx, "t1")
	if err != nil || got2 == nil {
		t.Fatalf("reading task: %v", err)
	}

	var gotPrompt string
	fw2 := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt = cfg.Prompt
		return pushed(), nil
	})
	if _, err := orchestrator.RunDispatch(ctx, store, fw2, orchestrator.Config{}, *got2, d2, nil, t.TempDir(), baseTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("second RunDispatch: %v", err)
	}

	if !strings.Contains(gotPrompt, question) {
		t.Errorf("second prompt does not contain the question it asked itself: %q", gotPrompt)
	}
	if !strings.Contains(gotPrompt, answer) {
		t.Errorf("second prompt does not contain the human's answer: %q", gotPrompt)
	}
}

// TestRunDispatchOmitsTheCommentThreadOnAFirstDispatch checks that a
// task's first dispatch, with no conversation yet, gets exactly
// BuildPrompt's own prompt back -- no empty "conversation so far" section
// tacked onto every prompt whether or not there is anything to say.
func TestRunDispatchOmitsTheCommentThreadOnAFirstDispatch(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", Slot: "local", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	var gotPrompt string
	fw := agentFunc(func(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		gotPrompt = cfg.Prompt
		return pushed(), nil
	})
	if _, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), baseTime); err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	if gotPrompt != orchestrator.BuildPrompt(*task) {
		t.Errorf("prompt = %q, want exactly BuildPrompt's own prompt with no conversation yet", gotPrompt)
	}
}

// closeTask marks id closed by writing task_observation directly, the
// same effect ui.Client.Close has -- restated here rather than importing
// pkg/ui, matching orchestrator_test.go's own "duplicated per file" style
// note on newSim.
func closeTask(t *testing.T, ctx context.Context, store *model.Store, id string, at time.Time) {
	t.Helper()
	if err := store.ObserveField(ctx, id, at, func(o *model.Observation) { o.ClosedAt = &at }); err != nil {
		t.Fatalf("closing task %s: %v", id, err)
	}
}

// TestRunDispatchCancelsTheAgentWhenItsTaskIsClosedMidFlight is
// bwsalmon/agents#346's own scenario: a task closed while its run is
// still live must actually stop that run's agent, not just prevent
// dispatch.Cycle from starting another one and ProcessResult from opening
// a pull request for it (e2e/close_while_live_test.go already covered
// those two). fw here blocks on the very ctx RunDispatch hands
// framework.Run until it is cancelled, so this test only passes if
// closing the task mid-run actually reaches that ctx -- proving
// watchForTaskClosed's store-polled cancellation signal works end to end,
// deterministically and fast (CancelPollInterval is set to a few
// milliseconds), rather than relying on a real subprocess's own timing.
func TestRunDispatchCancelsTheAgentWhenItsTaskIsClosedMidFlight(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", Slot: "local", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	started := make(chan struct{})
	fw := agentFunc(func(runCtx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		close(started)
		<-runCtx.Done()
		return nil, runCtx.Err()
	})
	cfg := orchestrator.Config{CancelPollInterval: 5 * time.Millisecond}

	type runOutcome struct {
		result *agent.Result
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		result, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), baseTime)
		done <- runOutcome{result, err}
	}()

	<-started
	closeTask(t, ctx, store, "t1", baseTime)

	select {
	case out := <-done:
		if out.result != nil {
			t.Errorf("result = %+v, want nil for a run cancelled mid-flight", out.result)
		}
		if out.err == nil || !strings.Contains(out.err.Error(), "closed") {
			t.Errorf("RunDispatch err = %v, want an error naming the task's closure", out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunDispatch did not return after its task was closed mid-flight -- the agent's ctx was never cancelled")
	}

	occupied, err := store.OccupiedSlots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(occupied) != 0 {
		t.Errorf("occupied slots after a cancelled run = %v, want none: FinishRun still frees the slot", occupied)
	}
}

// TestRunDispatchNeverLetsAnAlreadyClosedTaskReachARealToolCall is the
// race e2e/close_while_live_test.go itself exercises: dispatch.Cycle
// claims a slot while a task is still running, the task is closed before
// RunDispatch ever gets called for that already-claimed run, and only
// then does RunDispatch actually run. Leaving this to
// watchForTaskClosed's own polling ticker would make whether the agent's
// first tool call ever reaches a real sandbox a race against
// CancelPollInterval; RunDispatch instead checks synchronously, before
// framework.Run is ever invoked, which this proves by using the default
// (multi-second) CancelPollInterval and still finishing fast, and by
// checking that framework.Run's own ctx already reads cancelled the
// instant it starts.
func TestRunDispatchNeverLetsAnAlreadyClosedTaskReachARealToolCall(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", Slot: "local", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}
	closeTask(t, ctx, store, "t1", baseTime)

	sawCancelledCtx := false
	fw := agentFunc(func(runCtx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		sawCancelledCtx = runCtx.Err() != nil
		return nil, runCtx.Err()
	})

	start := time.Now()
	// Config{} leaves CancelPollInterval at its multi-second default --
	// deliberately, so this test can only pass quickly because of the
	// synchronous check, not because a short poll interval happened to
	// win a race.
	result, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), baseTime)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("RunDispatch took %s against an already-closed task, want near-instant (no waiting on CancelPollInterval)", elapsed)
	}

	if result != nil {
		t.Errorf("result = %+v, want nil for a task closed before its run ever started", result)
	}
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("err = %v, want an error naming the task's closure", err)
	}
	if !sawCancelledCtx {
		t.Error("framework.Run's own ctx was not already cancelled when it started -- an already-closed task's first tool call could still reach a real sandbox")
	}
}
