package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/dispatch"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// A run whose framework reports the agent's own usage limit records
// model.PausedOutcome, not "failed": nothing about the task was wrong,
// the deployment ran out of budget. The detail carries both halves an
// operator needs -- what the provider said, and when grain will try
// again.
func TestRunDispatchRecordsAUsageLimitAsPaused(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	reset := baseTime.Add(90 * time.Minute)
	fw := agentFunc(func(context.Context, agent.RunConfig) (*agent.Result, error) {
		return pushed(), &agent.UsageLimitError{
			Framework: "claude", Message: "Claude AI usage limit reached", ResetAt: reset,
		}
	})
	pause := &orchestrator.Pause{}
	cfg := orchestrator.Config{Pause: pause, Now: func() time.Time { return baseTime }}

	result, runErr := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), "", nil, baseTime)
	if runErr == nil {
		t.Fatal("RunDispatch err = nil, want the limit reported")
	}
	if _, ok := agent.UsageLimit(runErr); !ok {
		t.Errorf("RunDispatch err = %v, want it to still carry the agent.UsageLimitError", runErr)
	}
	// The branch this run pushed before it met the limit still comes
	// back, so runOne can salvage it (agent.Framework's own contract).
	if result == nil {
		t.Error("RunDispatch returned no result, want the work the run did before the limit")
	}

	runs, err := store.Runs(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want exactly one", runs)
	}
	if runs[0].Outcome != model.PausedOutcome {
		t.Errorf("run outcome = %q, want %q", runs[0].Outcome, model.PausedOutcome)
	}
	if !strings.Contains(runs[0].Detail, "usage limit reached") {
		t.Errorf("run detail = %q, want it to say what the provider said", runs[0].Detail)
	}
	if !strings.Contains(runs[0].Detail, reset.Format(time.RFC3339)) {
		t.Errorf("run detail = %q, want it to name when dispatch resumes (%s)", runs[0].Detail, reset)
	}

	if until, _ := pause.Until(); !until.Equal(reset) {
		t.Errorf("pause until = %s, want the provider's own reset %s", until, reset)
	}
	if _, _, blocked := pause.Blocked(baseTime); !blocked {
		t.Error("dispatch was left open after a run met the agent's usage limit")
	}
}

// The other half of the response: a run already in flight when somebody
// else's limit lands is cancelled rather than left to grind through the
// same refusal, and records the same outcome with a detail that says it
// was not at fault.
func TestRunDispatchCancelsALiveRunWhenAnotherMeetsTheLimit(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	pause := &orchestrator.Pause{}
	started := make(chan struct{})
	fw := agentFunc(func(runCtx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		close(started)
		<-runCtx.Done()
		return nil, runCtx.Err()
	})
	cfg := orchestrator.Config{Pause: pause, Now: func() time.Time { return baseTime }}

	done := make(chan error, 1)
	go func() {
		_, err := orchestrator.RunDispatch(ctx, store, fw, cfg, *task, d, nil, t.TempDir(), "", nil, baseTime)
		done <- err
	}()

	<-started
	// Some other run, on some other task, met the limit.
	pause.Begin(baseTime, &agent.UsageLimitError{Framework: "claude", ResetAt: baseTime.Add(time.Hour)})

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunDispatch err = nil, want the cancellation reported")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunDispatch did not return -- a live run was not cancelled by the pause")
	}

	runs, err := store.Runs(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want exactly one", runs)
	}
	if runs[0].Outcome != model.PausedOutcome {
		t.Errorf("run outcome = %q, want %q", runs[0].Outcome, model.PausedOutcome)
	}
	if !strings.Contains(runs[0].Detail, "another run") {
		t.Errorf("run detail = %q, want it to say this run was not the one at fault", runs[0].Detail)
	}
}

// Without a Pause wired (a test, a one-shot cycle) the limit is still
// recorded on the run that met it -- there is simply nothing to pause.
// RunDispatch calls every Pause method unguarded, so this is also the
// nil-safety check for that path.
func TestRunDispatchToleratesNoPause(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	fw := agentFunc(func(context.Context, agent.RunConfig) (*agent.Result, error) {
		return nil, &agent.UsageLimitError{Framework: "antigravity", Message: "quota exceeded"}
	})

	if _, err := orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{}, *task, d, nil, t.TempDir(), "", nil, baseTime); err == nil {
		t.Fatal("RunDispatch err = nil, want the limit reported")
	}
	runs, err := store.Runs(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Outcome != model.PausedOutcome {
		t.Errorf("run outcome = %q, want %q", runs[0].Outcome, model.PausedOutcome)
	}
	if strings.Contains(runs[0].Detail, "paused until") {
		t.Errorf("run detail = %q, want no resume time named when nothing is paused", runs[0].Detail)
	}
}

// The deployment-wide half: while the window is shut, a cycle dispatches
// nothing at all -- and everything else it does goes on as before, since
// syncing pull requests costs no agent tokens.
func TestRunCycleDispatchesNothingWhilePaused(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")

	var runs int
	fw := agentFunc(func(context.Context, agent.RunConfig) (*agent.Result, error) {
		runs++
		return pushed(), nil
	})
	pause := &orchestrator.Pause{}
	pause.Begin(baseTime, &agent.UsageLimitError{Framework: "claude", ResetAt: baseTime.Add(time.Hour)})

	_, client := newSim(t, "acme", "widgets", "main")
	deps := orchestrator.Deps{
		Store: store, Client: client, Sandboxes: orchestrator.NewHostSandboxes(t.TempDir()),
		Framework:  orchestrator.StaticFramework(fw),
		MaxWorkers: 2,
		Config:     orchestrator.Config{Pause: pause, Now: func() time.Time { return baseTime }},
	}

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if runs != 0 {
		t.Errorf("dispatched %d run(s) while paused, want none", runs)
	}
	if got, err := store.Runs(ctx, "t1"); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Errorf("runs = %+v, want none started while paused", got)
	}

	// And once the provider's window has passed, the very next tick
	// dispatches again with nothing else having to happen.
	after := baseTime.Add(2 * time.Hour)
	if err := orchestrator.RunCycle(ctx, deps, after); err != nil {
		t.Fatalf("RunCycle after the window reset: %v", err)
	}
	if runs != 1 {
		t.Errorf("dispatched %d run(s) after the pause expired, want 1", runs)
	}
}

// A run stopped by a usage limit must not spend the task's own retry
// budget: model.FailureStreak passes it over, so the task is dispatchable
// again the moment the pause lifts rather than backing off as though it
// had failed.
func TestAPausedRunDoesNotCountAgainstTheTask(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	if err := store.FinishRun(ctx, d.RunID, baseTime.Add(time.Minute), model.PausedOutcome,
		"the agent's usage limit was reached"); err != nil {
		t.Fatal(err)
	}

	streak, err := store.FailureStreak(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if streak == nil {
		t.Fatal("FailureStreak = nil, want the run reported with a zero count")
	}
	if streak.Count != 0 {
		t.Errorf("FailureStreak.Count = %d after a paused run, want 0", streak.Count)
	}

	// And the task is offered for dispatch again immediately: no
	// backoff, because nothing about it failed.
	ready, err := dispatch.Cycle(ctx, store, model.Limits{Workers: 1}, baseTime.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].TaskID != "t1" {
		t.Errorf("dispatch.Cycle = %+v, want t1 dispatched again with no backoff", ready)
	}
}

// A run cancelled by a pause is not a run the wall-clock/task-closed
// paths should claim: their causes and this one have to stay distinct,
// since only this one means "the deployment is out of budget".
func TestUsageLimitCancellationIsItsOwnCause(t *testing.T) {
	store, ctx := openStore(t)
	dispatchTask(t, ctx, store, "t1")
	d := dispatch.Dispatch{TaskID: "t1", RunID: "r1", Attempt: 1}
	startRun(t, ctx, store, d, baseTime)
	task, err := store.GetTask(ctx, "t1")
	if err != nil || task == nil {
		t.Fatalf("reading task: %v", err)
	}

	pause := &orchestrator.Pause{}
	started := make(chan struct{})
	var cause error
	fw := agentFunc(func(runCtx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
		close(started)
		<-runCtx.Done()
		cause = context.Cause(runCtx)
		return nil, runCtx.Err()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		orchestrator.RunDispatch(ctx, store, fw, orchestrator.Config{Pause: pause}, *task, d, nil, t.TempDir(), "", nil, baseTime)
	}()
	<-started
	pause.Begin(time.Now(), &agent.UsageLimitError{})
	<-done

	if cause == nil || errors.Is(cause, context.Canceled) && strings.Contains(cause.Error(), "closed") {
		t.Fatalf("cause = %v, want the usage-limit cause", cause)
	}
	if !strings.Contains(cause.Error(), "usage limit") {
		t.Errorf("cause = %v, want it to name the usage limit", cause)
	}
}
