package dispatch_test

// Cycle against a real embedded SQLite database, the same discipline
// model/sqlite/store_test.go holds to: these prove the dispatch decision
// is correct against the actual views it reads, not a fake standing in
// for them. bwsalmon/agents#219 asked for exactly this and nothing
// more — no sandbox, no GitHub, no agent — so every "outcome" below is
// simulated with the store calls a later, side-effecting change would
// make for real: FinishRun, Observe.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/dispatch"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
)

var (
	now   = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	human = model.Principal{Kind: model.PrincipalHuman, ID: "bwsalmon"}
)

func open(t *testing.T) (*model.Store, context.Context) {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	return store, ctx
}

func task(id string, approved bool, links ...model.Link) model.Task {
	tk := model.Task{
		ID: id, Intent: model.IntentImplement, Title: "task " + id,
		Origin: model.Origin{
			Attribution: model.Attribution{Actor: human},
			Reason:      model.ReasonDirect,
		},
		Binding: model.BindingDirective,
		Links:   links,
	}
	if approved {
		tk.Approval = &model.Attribution{Actor: human}
	}
	return tk
}

// fixTask builds a task the merge queue would file to repair a stuck
// queue head (Origin.Reason == ReasonFix) -- see orchestrator/sync.go's
// fileFixTask. Dispatch order is the only thing distinguishing it from
// an ordinary task here, so nothing else about it needs to differ.
func fixTask(id string) model.Task {
	tk := task(id, true)
	tk.Origin.Reason = model.ReasonFix
	return tk
}

// configurationTask builds an approved task carrying Task.Configuration
// (bwsalmon/agents#621) -- the one property Cycle itself cares about is
// this flag, so nothing else about the task needs to differ from an
// ordinary one built by task().
func configurationTask(id string) model.Task {
	tk := task(id, true)
	tk.Configuration = true
	return tk
}

func putTasks(t *testing.T, store *model.Store, ctx context.Context, tasks ...model.Task) {
	t.Helper()
	for _, tk := range tasks {
		if err := store.PutTask(ctx, tk); err != nil {
			t.Fatalf("put task %s: %v", tk.ID, err)
		}
	}
}

func TestCycleDispatchesReadyTasksInOrderAndNoFurther(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, task("t0", true), task("t1", true), task("t2", true))

	got, err := dispatch.Cycle(ctx, store, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("dispatched %d tasks, want 2 (the whole concurrency limit): %+v", len(got), got)
	}

	dispatched := map[string]bool{}
	runs := map[string]bool{}
	for _, d := range got {
		dispatched[d.TaskID] = true
		runs[d.RunID] = true
		if d.Attempt != 1 {
			t.Errorf("%s dispatched with attempt %d, want 1", d.TaskID, d.Attempt)
		}
	}
	if !dispatched["t0"] || !dispatched["t1"] {
		t.Errorf("expected t0 and t1 dispatched (task_ready's own order), got %+v", got)
	}
	if len(runs) != 2 {
		t.Errorf("two dispatches produced %d distinct run ids, want 2: %+v", len(runs), got)
	}

	for _, id := range []string{"t0", "t1"} {
		if st, _ := store.State(ctx, id); st != model.StateRunning {
			t.Errorf("%s state = %q, want running", id, st)
		}
	}
	if st, _ := store.State(ctx, "t2"); st != model.StateQueued {
		t.Errorf("t2 state = %q, want queued (no capacity left for it)", st)
	}
}

func TestCycleLeavesAnAlreadyRunningTaskAlone(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, task("t0", true))

	first, err := dispatch.Cycle(ctx, store, 2, now)
	if err != nil || len(first) != 1 {
		t.Fatalf("first cycle: %v, %+v", err, first)
	}

	// Two slots, still only one task in the world, and it is already
	// running — task_ready excludes it, so a second cycle must dispatch
	// nothing even though slot-2 has never been used.
	second, err := dispatch.Cycle(ctx, store, 2, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("second cycle dispatched %+v, want nothing: a running task must not be redispatched", second)
	}
}

func TestCycleRespectsTheConcurrencyLimit(t *testing.T) {
	store, ctx := open(t)
	ids := []string{"t0", "t1", "t2", "t3", "t4"}
	for _, id := range ids {
		putTasks(t, store, ctx, task(id, true))
	}
	const maxConcurrent = 2

	first, err := dispatch.Cycle(ctx, store, maxConcurrent, now)
	if err != nil || len(first) != 2 {
		t.Fatalf("first cycle: %v, %+v", err, first)
	}
	live, err := store.LiveRunCount(ctx)
	if err != nil || live != 2 {
		t.Fatalf("live runs after first cycle: %v, %d", err, live)
	}

	// No headroom: nothing more may be dispatched no matter how many
	// tasks are ready.
	second, err := dispatch.Cycle(ctx, store, maxConcurrent, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("cycle at the concurrency limit dispatched %+v", second)
	}

	// Finishing one run frees exactly one unit of capacity, so exactly
	// one more task starts.
	if err := store.FinishRun(ctx, first[0].RunID, now.Add(2*time.Minute), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	third, err := dispatch.Cycle(ctx, store, maxConcurrent, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 1 {
		t.Fatalf("cycle after one finish dispatched %+v, want exactly 1", third)
	}
	if live, err := store.LiveRunCount(ctx); err != nil || live != 2 {
		t.Errorf("live runs after the freed capacity was refilled = %d (%v), want 2", live, err)
	}
}

// TestCycleDispatchesAConfigurationTaskEvenAtTheConcurrencyLimit is
// bwsalmon/agents#621's whole point: the configuration agent must always
// be able to start a sandbox, even when the deployment is already at
// MaxConcurrent -- unlike TestCycleRespectsTheConcurrencyLimit's ordinary
// tasks, which the same setup here leaves stuck at capacity.
func TestCycleDispatchesAConfigurationTaskEvenAtTheConcurrencyLimit(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, task("t0", true), task("t1", true))
	const maxConcurrent = 2

	first, err := dispatch.Cycle(ctx, store, maxConcurrent, now)
	if err != nil || len(first) != 2 {
		t.Fatalf("first cycle: %v, %+v", err, first)
	}
	if live, err := store.LiveRunCount(ctx); err != nil || live != 2 {
		t.Fatalf("live runs after first cycle: %v, %d", err, live)
	}

	// The deployment is now saturated -- an ordinary task would get
	// nothing (TestCycleRespectsTheConcurrencyLimit). Filing the
	// configuration agent into exactly this situation is the scenario
	// bwsalmon/agents#621 exists for.
	putTasks(t, store, ctx, configurationTask("config"))
	second, err := dispatch.Cycle(ctx, store, maxConcurrent, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].TaskID != "config" {
		t.Fatalf("cycle at the concurrency limit = %+v, want the configuration task dispatched anyway", second)
	}
	if live, err := store.LiveRunCount(ctx); err != nil || live != 3 {
		t.Fatalf("live runs after dispatching over the limit = %d (%v), want 3", live, err)
	}
}

// TestCycleDispatchesAConfigurationTaskAheadOfOrdinaryTasksWithoutSpendingTheirCapacity
// checks the other half of dispatchConfiguration's contract: it runs
// before the ordinary capacity-gated loop, but does not itself eat into
// the headroom that loop computes -- an ordinary ready task still gets
// dispatched up to the real limit in the same cycle.
func TestCycleDispatchesAConfigurationTaskAheadOfOrdinaryTasksWithoutSpendingTheirCapacity(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, configurationTask("config"), task("t0", true), task("t1", true))
	const maxConcurrent = 2

	got, err := dispatch.Cycle(ctx, store, maxConcurrent, now)
	if err != nil {
		t.Fatal(err)
	}
	dispatched := map[string]bool{}
	for _, d := range got {
		dispatched[d.TaskID] = true
	}
	if !dispatched["config"] || !dispatched["t0"] || len(got) != 2 {
		t.Fatalf("cycle = %+v, want the configuration task plus exactly one ordinary task", got)
	}
	if live, err := store.LiveRunCount(ctx); err != nil || live != 2 {
		t.Fatalf("live runs after the cycle = %d (%v), want 2", live, err)
	}
}

func TestCycleSkipsBlockedTasksUntilTheirDependencyCloses(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx,
		task("blocker", true),
		task("blocked", true, model.Link{Kind: model.LinkDependsOn, Target: "blocker"}),
	)

	got, err := dispatch.Cycle(ctx, store, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got {
		if d.TaskID == "blocked" {
			t.Fatalf("dispatched a blocked task: %+v", got)
		}
	}
	if len(got) != 1 || got[0].TaskID != "blocker" {
		t.Fatalf("expected only the blocker dispatched, got %+v", got)
	}

	// Closing the dependency — not merely completing it, see schema.go's
	// task_blocked view — is what should unblock the task on the very
	// next cycle, with nothing re-applied to it.
	if err := store.Observe(ctx, model.Observation{TaskID: "blocker", ClosedAt: &now}); err != nil {
		t.Fatal(err)
	}
	// Still a limit of 2, and the blocker's own run is still live, so
	// exactly one unit of capacity is free for the unblocked task.
	got, err = dispatch.Cycle(ctx, store, 2, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TaskID != "blocked" {
		t.Fatalf("expected the formerly-blocked task dispatched after its dependency closed, got %+v", got)
	}
}

// TestCycleDispatchesFixTasksBeforeOrdinaryReadyTasks is bwsalmon/
// agents#389: a fix task the merge queue filed for a stuck repo head
// must take a free slot ahead of ordinary new work, even one that has
// been ready longer and would otherwise win task_ready's own tiebreak
// (task ID) -- otherwise the repair sits behind unrelated tasks while
// the branch it targets keeps moving, and has to be refiled once it
// finally runs.
func TestCycleDispatchesFixTasksBeforeOrdinaryReadyTasks(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, task("a-new-work", true), fixTask("z-fix"))

	got, err := dispatch.Cycle(ctx, store, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TaskID != "z-fix" {
		t.Fatalf("dispatched %+v, want the fix task despite sorting after the other task by ID", got)
	}
}

func TestAttemptNumberIncrementsOnEachRedispatch(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, task("t0", true))
	const maxConcurrent = 1

	first, err := dispatch.Cycle(ctx, store, maxConcurrent, now)
	if err != nil || len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("first dispatch: %v, %+v", err, first)
	}

	// Finishing with no Observation is a requeue, not a completion — the
	// task must come back through task_ready exactly as before.
	if err := store.FinishRun(ctx, first[0].RunID, now.Add(time.Minute), "requeued", ""); err != nil {
		t.Fatal(err)
	}
	second, err := dispatch.Cycle(ctx, store, maxConcurrent, now.Add(2*time.Minute))
	if err != nil || len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("second dispatch: %v, %+v", err, second)
	}
	if second[0].RunID == first[0].RunID {
		t.Errorf("retry reused run id %q", second[0].RunID)
	}
	if n, err := store.Attempts(ctx, "t0"); err != nil || n != 2 {
		t.Fatalf("attempts = %d (%v), want 2", n, err)
	}
}

// TestARecentlyFailedTaskBacksOffWithoutBlockingOthers is bwsalmon/
// agents#403: a task whose last run failed a moment ago must not take a
// free slot again immediately (that is the whole "retried forever with no
// backoff" bug), but it also must not hold up a free slot going to some
// other, unrelated ready task that is not backing off at all.
func TestARecentlyFailedTaskBacksOffWithoutBlockingOthers(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, task("t0", true), task("t1", true))
	const maxConcurrent = 1

	first, err := dispatch.Cycle(ctx, store, maxConcurrent, now)
	if err != nil || len(first) != 1 || first[0].TaskID != "t0" {
		t.Fatalf("first dispatch: %v, %+v", err, first)
	}
	if err := store.FinishRun(ctx, first[0].RunID, now.Add(time.Second), "failed", "simulated failure"); err != nil {
		t.Fatal(err)
	}

	// t0 just failed and has not backed off yet, but t1 has never run at
	// all -- it must be offered the only free slot instead of the cycle
	// giving up because task_ready's first entry (t0) is not eligible.
	second, err := dispatch.Cycle(ctx, store, maxConcurrent, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].TaskID != "t1" {
		t.Fatalf("second dispatch = %+v, want t1 dispatched in t0's place", second)
	}
	finished := now.Add(3 * time.Second)
	if err := store.FinishRun(ctx, second[0].RunID, finished, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	// Mark t1 completed so it does not itself come back through
	// task_ready and mask what this test is actually about: whether t0
	// backs off correctly.
	if err := store.Observe(ctx, model.Observation{TaskID: "t1", CompletedAt: &finished}); err != nil {
		t.Fatal(err)
	}

	// Still too soon after t0's own failure: nothing to dispatch even
	// though the slot is free again.
	third, err := dispatch.Cycle(ctx, store, maxConcurrent, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 0 {
		t.Fatalf("third dispatch = %+v, want nothing: t0 has not backed off yet", third)
	}

	// Comfortably past baseRetryBackoff (30s): t0 is eligible again.
	fourth, err := dispatch.Cycle(ctx, store, maxConcurrent, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(fourth) != 1 || fourth[0].TaskID != "t0" || fourth[0].Attempt != 2 {
		t.Fatalf("fourth dispatch = %+v, want t0's second attempt", fourth)
	}
}

// TestATaskCappedAtMaxConsecutiveFailuresStopsBeingReady is bwsalmon/
// agents#403's own cap: once a task has failed model.MaxConsecutiveFailures
// times in a row with nothing succeeding in between, task_state reads it
// as 'failed' rather than 'queued', so it drops out of Ready and Cycle
// never offers it a slot again on its own.
func TestATaskCappedAtMaxConsecutiveFailuresStopsBeingReady(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, task("t0", true))
	const maxConcurrent = 1

	clock := now
	for i := 0; i < model.MaxConsecutiveFailures; i++ {
		// Advance well past any backoff window so the cap itself, not a
		// still-pending backoff, is what this test exercises.
		clock = clock.Add(time.Hour)
		got, err := dispatch.Cycle(ctx, store, maxConcurrent, clock)
		if err != nil {
			t.Fatalf("attempt %d: Cycle: %v", i+1, err)
		}
		if len(got) != 1 {
			t.Fatalf("attempt %d: dispatched %+v, want exactly one run", i+1, got)
		}
		clock = clock.Add(time.Second)
		if err := store.FinishRun(ctx, got[0].RunID, clock, "failed", "simulated failure"); err != nil {
			t.Fatalf("attempt %d: FinishRun: %v", i+1, err)
		}
	}

	st, err := store.State(ctx, "t0")
	if err != nil {
		t.Fatal(err)
	}
	if st != model.StateFailed {
		t.Fatalf("state after %d consecutive failures = %q, want failed", model.MaxConsecutiveFailures, st)
	}

	clock = clock.Add(24 * time.Hour)
	got, err := dispatch.Cycle(ctx, store, maxConcurrent, clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Cycle dispatched a capped task: %+v", got)
	}

	// A human's own retry request is the only way out: it clears the
	// streak, and the task is dispatchable again despite no run ever
	// having succeeded.
	if err := store.ObserveField(ctx, "t0", clock, func(o *model.Observation) {
		o.RetryRequestedAt = &clock
	}); err != nil {
		t.Fatal(err)
	}
	if st, err := store.State(ctx, "t0"); err != nil || st != model.StateQueued {
		t.Fatalf("state after a retry request = %q (%v), want queued", st, err)
	}
	clock = clock.Add(time.Minute)
	got, err = dispatch.Cycle(ctx, store, maxConcurrent, clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TaskID != "t0" {
		t.Fatalf("Cycle after a retry request = %+v, want t0 dispatched once more", got)
	}
}

// TestInvariantsHoldAcrossManyRandomCycles is the property this package
// exists to guarantee, checked the way a property survives rather than
// the way one example does: build a small DAG of tasks, drive Cycle for
// many cycles against a real store, and after every single one confirm
// three things no cycle may ever violate — no slot holds two runs, no
// task holds two runs, and no blocked task ever runs — while a runs of
// outcomes (retried, completed, closed) is fed back in exactly the way a
// side-effecting layer will once #219 grows one. The seed is fixed so a
// failure is reproducible rather than a flake to chase.
func TestInvariantsHoldAcrossManyRandomCycles(t *testing.T) {
	store, ctx := open(t)
	rng := rand.New(rand.NewPCG(1, 219))

	const numTasks = 10
	ids := make([]string, numTasks)
	dependsOn := map[string]string{}
	for i := 0; i < numTasks; i++ {
		ids[i] = fmt.Sprintf("tk%02d", i)
		var links []model.Link
		if i > 0 && rng.Float64() < 0.5 {
			parent := ids[rng.IntN(i)]
			dependsOn[ids[i]] = parent
			links = []model.Link{{Kind: model.LinkDependsOn, Target: parent}}
		}
		putTasks(t, store, ctx, task(ids[i], true, links...))
	}

	const maxConcurrent = 3
	closed := map[string]bool{}
	liveRunID := map[string]string{}   // task -> its current run, while live
	liveSandbox := map[string]string{} // task -> the sandbox its live run was given
	sandboxOf := map[string]string{}   // sandbox -> task, while that run is live
	attemptsSoFar := map[string]int{}
	clock := now

	unblocked := func(id string) bool {
		parent, ok := dependsOn[id]
		return !ok || closed[parent]
	}

	for cycle := 0; cycle < 60; cycle++ {
		clock = clock.Add(time.Minute)
		dispatches, err := dispatch.Cycle(ctx, store, maxConcurrent, clock)
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}

		for _, d := range dispatches {
			if _, already := liveRunID[d.TaskID]; already {
				t.Fatalf("cycle %d: %s dispatched while already running", cycle, d.TaskID)
			}
			if holder, taken := sandboxOf[d.RunID]; taken {
				t.Fatalf("cycle %d: run id %s handed to %s while still held by %s",
					cycle, d.RunID, d.TaskID, holder)
			}
			if closed[d.TaskID] {
				t.Fatalf("cycle %d: closed task %s was dispatched", cycle, d.TaskID)
			}
			if !unblocked(d.TaskID) {
				t.Fatalf("cycle %d: blocked task %s was dispatched", cycle, d.TaskID)
			}
			if want := attemptsSoFar[d.TaskID] + 1; d.Attempt != want {
				t.Fatalf("cycle %d: %s dispatched with attempt %d, want %d",
					cycle, d.TaskID, d.Attempt, want)
			}
			liveRunID[d.TaskID] = d.RunID
			// A run's sandbox is named after the run itself, which is
			// what orchestrator.runOne records; dispatch never invents
			// one, so the mirror derives it the same way.
			liveSandbox[d.TaskID] = d.RunID
			sandboxOf[d.RunID] = d.TaskID
			attemptsSoFar[d.TaskID]++
		}

		if len(sandboxOf) > maxConcurrent {
			t.Fatalf("cycle %d: %d runs live, limit is %d", cycle, len(sandboxOf), maxConcurrent)
		}
		if occ, err := store.LiveRunCount(ctx); err != nil {
			t.Fatalf("cycle %d: LiveRunCount: %v", cycle, err)
		} else if occ != len(sandboxOf) {
			t.Fatalf("cycle %d: store reports %d live runs, mirror expects %d",
				cycle, occ, len(sandboxOf))
		}

		// Feed back outcomes for a random subset of live runs — exactly
		// the store calls a real dispatcher will make once side effects
		// exist, simulated here since #219 explicitly defers them.
		for _, id := range ids {
			runID, isLive := liveRunID[id]
			if !isLive || rng.Float64() >= 0.4 {
				continue
			}
			clock = clock.Add(time.Minute)
			if err := store.FinishRun(ctx, runID, clock, "outcome", ""); err != nil {
				t.Fatalf("cycle %d: finishing %s: %v", cycle, runID, err)
			}
			delete(sandboxOf, liveSandbox[id])
			delete(liveRunID, id)
			delete(liveSandbox, id)

			switch r := rng.Float64(); {
			case r < 0.5:
				// A plain requeue: no observation, so it is ready again
				// next cycle unless something else blocks it now.
			case r < 0.8:
				if err := store.Observe(ctx, model.Observation{TaskID: id, CompletedAt: &clock}); err != nil {
					t.Fatal(err)
				}
			default:
				if err := store.Observe(ctx, model.Observation{
					TaskID: id, CompletedAt: &clock, ClosedAt: &clock,
				}); err != nil {
					t.Fatal(err)
				}
				closed[id] = true
			}
		}
	}
}

// The window dispatch.Busy exists for, simulated with the store calls
// the orchestrator makes for real: a run whose row is already finished
// but whose result has not yet been turned into an observation. The
// store says the task is queued again, and it is the caller -- still
// holding that result -- that knows better.
func TestCycleSkipsATaskItsCallerIsStillFinishingWith(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, task("t0", true), task("t1", true))

	first, err := dispatch.Cycle(ctx, store, 1, now)
	if err != nil || len(first) != 1 || first[0].TaskID != "t0" {
		t.Fatalf("first cycle: %v, %+v", err, first)
	}
	// The run is over as far as the store is concerned; what it means for
	// the task has not been recorded yet.
	if err := store.FinishRun(ctx, first[0].RunID, now.Add(time.Minute), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := store.State(ctx, "t0"); st != model.StateQueued {
		t.Fatalf("t0 state = %q, want queued -- the premise of this test", st)
	}

	busy := func(taskID string) bool { return taskID == "t0" }
	second, err := dispatch.Cycle(ctx, store, 1, now.Add(2*time.Minute), dispatch.Busy(busy))
	if err != nil {
		t.Fatal(err)
	}
	// t1, not t0: being passed over must not cost t0's capacity, the same
	// way a task still backing off does not cost the one behind it.
	if len(second) != 1 || second[0].TaskID != "t1" {
		t.Fatalf("cycle with t0 busy = %+v, want t1 dispatched into the free capacity instead", second)
	}
	if runs, err := store.Runs(ctx, "t0"); err != nil || len(runs) != 1 {
		t.Fatalf("t0 has %d run(s) (%v), want only the one it was already finishing", len(runs), err)
	}

	// Once the caller is done with it, nothing about the task stops it
	// being dispatched again on whatever the store says then.
	if err := store.FinishRun(ctx, second[0].RunID, now.Add(3*time.Minute), "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	third, err := dispatch.Cycle(ctx, store, 1, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 1 || third[0].TaskID != "t0" {
		t.Fatalf("cycle with nothing busy = %+v, want t0 dispatched again", third)
	}
}

// The configuration agent takes the same exemption from its own dispatch
// path (dispatchConfiguration), which does not share the loop above.
func TestCycleSkipsAConfigurationTaskItsCallerIsStillFinishingWith(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, configurationTask("config"))

	first, err := dispatch.Cycle(ctx, store, 1, now)
	if err != nil || len(first) != 1 || first[0].TaskID != "config" {
		t.Fatalf("first cycle: %v, %+v", err, first)
	}
	if err := store.FinishRun(ctx, first[0].RunID, now.Add(time.Minute), "succeeded", ""); err != nil {
		t.Fatal(err)
	}

	busy := func(taskID string) bool { return taskID == "config" }
	second, err := dispatch.Cycle(ctx, store, 1, now.Add(2*time.Minute), dispatch.Busy(busy))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("cycle with the configuration task busy = %+v, want nothing dispatched", second)
	}
}
