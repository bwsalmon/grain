// Whether a cycle can still dispatch while an earlier run is in flight
// -- the thing a deployment loses when RunCycle waits for the agents it
// started. Every test here files its second task *after* the first run
// is already inside framework.Run, which is the ordinary case (a person
// files a task while something is running) and the one that used to wait
// out the whole run however much of MaxConcurrent was idle.
package orchestrator_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// blockingFramework is an agent that never finishes on its own: it
// announces the run it was given (by the sandbox directory that run was
// named after) and then waits to be released, so a test can hold a run
// live for as long as it needs to watch what the cycles around it do.
type blockingFramework struct {
	entered chan string
	release chan struct{}
}

func newBlockingFramework() *blockingFramework {
	return &blockingFramework{entered: make(chan string, 8), release: make(chan struct{})}
}

func (f *blockingFramework) Run(ctx context.Context, cfg agent.RunConfig) (*agent.Result, error) {
	f.entered <- filepath.Base(cfg.SandboxRoot)
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return toolResult(agent.ToolCall{Name: "comment_on_issue", Arguments: map[string]any{"comment": "done"}}), nil
}

// releaseAll lets every run still inside Run finish, and every later one
// pass straight through.
func (f *blockingFramework) releaseAll() { close(f.release) }

// enteredRun is the next run blockingFramework was handed, or a failure
// naming what the test was waiting for -- never a hang, which is what a
// blocked cycle would otherwise look like.
func enteredRun(t *testing.T, f *blockingFramework, want string) {
	t.Helper()
	select {
	case got := <-f.entered:
		if got != want {
			t.Fatalf("the agent ran for %s, want %s", got, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("no agent ran for %s", want)
	}
}

// cycle runs one, failing rather than hanging if it does not return --
// "does not return while a run is live" being exactly the bug these
// tests exist for.
func cycle(t *testing.T, ctx context.Context, deps orchestrator.Deps) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- orchestrator.RunCycle(ctx, deps, baseTime) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunCycle: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunCycle did not return while a dispatched run was still in flight")
	}
}

// asyncDeps is the wiring a deployment has: an InFlight to park runs in,
// so a cycle is over once its decisions are made.
func asyncDeps(t *testing.T, store *model.Store, fw *blockingFramework, maxConcurrent int) (orchestrator.Deps, *orchestrator.InFlight) {
	t.Helper()
	_, client := newSim(t, "acme", "widgets", "main")
	runs := &orchestrator.InFlight{}
	return orchestrator.Deps{
		Store:         store,
		Client:        client,
		Sandboxes:     orchestrator.NewHostSandboxes(t.TempDir()),
		Framework:     orchestrator.StaticFramework(fw),
		MaxConcurrent: maxConcurrent,
		Runs:          runs,
	}, runs
}

func TestACycleDispatchesATaskFiledWhileAnotherRunIsLive(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	fw := newBlockingFramework()
	deps, runs := asyncDeps(t, store, fw, 2)

	cycle(t, ctx, deps)
	enteredRun(t, fw, "t1-1")

	filedTask(t, ctx, store, "t2", repo)
	cycle(t, ctx, deps)
	enteredRun(t, fw, "t2-1")

	if got, err := store.LiveRunCount(ctx); err != nil {
		t.Fatal(err)
	} else if got != 2 {
		t.Fatalf("live runs = %d, want both t1's and t2's", got)
	}

	fw.releaseAll()
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := runs.Wait(waitCtx); err != nil {
		t.Fatalf("waiting for the dispatched runs: %v", err)
	}
	for _, id := range []string{"t1", "t2"} {
		finishedRun(t, ctx, store, id)
	}
}

// The other half of the same property: dispatching without waiting must
// not dispatch past the limit. The store is what enforces it (StartRun
// counts live runs inside its own transaction), and a run's row stays
// live across ticks precisely because the goroutine that finishes it
// outlives the cycle -- so a second tick sees the first run's capacity
// still spent.
func TestACycleDoesNotDispatchPastMaxConcurrentWhileARunIsLive(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	fw := newBlockingFramework()
	deps, runs := asyncDeps(t, store, fw, 1)

	cycle(t, ctx, deps)
	enteredRun(t, fw, "t1-1")

	filedTask(t, ctx, store, "t2", repo)
	cycle(t, ctx, deps)

	if got, err := store.Runs(ctx, "t2"); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("t2 was dispatched (%+v) with the deployment's one run already live", got)
	}

	fw.releaseAll()
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := runs.Wait(waitCtx); err != nil {
		t.Fatalf("waiting for t1's run: %v", err)
	}

	// And the capacity comes back: the very next tick, with nothing else
	// having changed, starts the task that was waiting.
	cycle(t, ctx, deps)
	enteredRun(t, fw, "t2-1")
	if err := runs.Wait(waitCtx); err != nil {
		t.Fatalf("waiting for t2's run: %v", err)
	}
	finishedRun(t, ctx, store, "t2")
}

// A change to max-concurrent made through the store -- `grain settings`,
// or the UI's Settings page, either of which ends in Store.PutConfig --
// takes effect on the next cycle, with no restart and no change to the
// Deps a long-lived daemon process already built: RunCycle itself
// re-reads it (RunCycle's own doc comment).
func TestACycleAdoptsAMaxConcurrentChangeFromTheStoreWithoutRestart(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)
	filedTask(t, ctx, store, "t2", repo)

	fw := newBlockingFramework()
	deps, runs := asyncDeps(t, store, fw, 1)

	cycle(t, ctx, deps)
	enteredRun(t, fw, "t1-1")

	if got, err := store.Runs(ctx, "t2"); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("t2 was dispatched (%+v) with the deployment's one run already live", got)
	}

	if err := store.PutConfig(ctx, model.Config{MaxConcurrent: 2}); err != nil {
		t.Fatalf("PutConfig: %v", err)
	}

	// deps itself still says MaxConcurrent: 1 -- exactly what a running
	// daemon's own copy would say, since nothing restarted it.
	cycle(t, ctx, deps)
	enteredRun(t, fw, "t2-1")

	if got, err := store.LiveRunCount(ctx); err != nil {
		t.Fatal(err)
	} else if got != 2 {
		t.Fatalf("live runs = %d, want both t1's and t2's", got)
	}

	fw.releaseAll()
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := runs.Wait(waitCtx); err != nil {
		t.Fatalf("waiting for the dispatched runs: %v", err)
	}
	for _, id := range []string{"t1", "t2"} {
		finishedRun(t, ctx, store, id)
	}
}

// Deps with no InFlight keeps the old shape -- the cycle waits for the
// run it started -- which every one-shot caller in-tree relies on.
func TestACycleWithNowhereToParkRunsStillWaitsForThem(t *testing.T) {
	store, ctx := openStore(t)
	repo := model.RepoRef{Owner: "acme", Name: "widgets"}
	filedTask(t, ctx, store, "t1", repo)

	fw := newBlockingFramework()
	fw.releaseAll() // nothing to hold: this run must finish inside the cycle
	deps, _ := asyncDeps(t, store, fw, 1)
	deps.Runs = nil

	if err := orchestrator.RunCycle(ctx, deps, baseTime); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if got, err := store.LiveRunCount(ctx); err != nil {
		t.Fatal(err)
	} else if got != 0 {
		t.Fatalf("live runs after a cycle that should have waited = %d, want 0", got)
	}
	finishedRun(t, ctx, store, "t1")
}

// finishedRun asserts taskID has exactly one run and that it is over.
func finishedRun(t *testing.T, ctx context.Context, store *model.Store, taskID string) {
	t.Helper()
	got, err := store.Runs(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%s has %d run(s), want exactly one", taskID, len(got))
	}
	if got[0].FinishedAt == nil {
		t.Errorf("%s's run %s is still live after its agent returned", taskID, got[0].ID)
	}
}
