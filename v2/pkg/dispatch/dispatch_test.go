package dispatch_test

// Cycle against a real embedded Dolt database, the same discipline
// model/dolt/store_test.go holds to: these prove the dispatch decision
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

	"github.com/bwsalmon/grain/v2/pkg/dispatch"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
)

var (
	now   = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	human = model.Principal{Kind: model.PrincipalHuman, ID: "bwsalmon"}
)

func open(t *testing.T) (*model.Store, context.Context) {
	t.Helper()
	db, err := dolt.Open(dolt.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening embedded dolt: %v", err)
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

	got, err := dispatch.Cycle(ctx, store, []string{"slot-1", "slot-2"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("dispatched %d tasks, want 2 (one per free slot): %+v", len(got), got)
	}

	dispatched := map[string]bool{}
	slots := map[string]bool{}
	for _, d := range got {
		dispatched[d.TaskID] = true
		slots[d.Slot] = true
		if d.Attempt != 1 {
			t.Errorf("%s dispatched with attempt %d, want 1", d.TaskID, d.Attempt)
		}
	}
	if !dispatched["t0"] || !dispatched["t1"] {
		t.Errorf("expected t0 and t1 dispatched (task_ready's own order), got %+v", got)
	}
	if len(slots) != 2 {
		t.Errorf("two dispatches landed on %d distinct slots, want 2: %+v", len(slots), got)
	}

	for _, id := range []string{"t0", "t1"} {
		if st, _ := store.State(ctx, id); st != model.StateRunning {
			t.Errorf("%s state = %q, want running", id, st)
		}
	}
	if st, _ := store.State(ctx, "t2"); st != model.StateQueued {
		t.Errorf("t2 state = %q, want queued (no free slot left for it)", st)
	}
}

func TestCycleLeavesAnAlreadyRunningTaskAlone(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, task("t0", true))

	first, err := dispatch.Cycle(ctx, store, []string{"slot-1", "slot-2"}, now)
	if err != nil || len(first) != 1 {
		t.Fatalf("first cycle: %v, %+v", err, first)
	}

	// Two slots, still only one task in the world, and it is already
	// running — task_ready excludes it, so a second cycle must dispatch
	// nothing even though slot-2 has never been used.
	second, err := dispatch.Cycle(ctx, store, []string{"slot-1", "slot-2"}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("second cycle dispatched %+v, want nothing: a running task must not be redispatched", second)
	}
}

func TestCycleRespectsTheSlotCount(t *testing.T) {
	store, ctx := open(t)
	ids := []string{"t0", "t1", "t2", "t3", "t4"}
	for _, id := range ids {
		putTasks(t, store, ctx, task(id, true))
	}
	slots := []string{"slot-1", "slot-2"}

	first, err := dispatch.Cycle(ctx, store, slots, now)
	if err != nil || len(first) != 2 {
		t.Fatalf("first cycle: %v, %+v", err, first)
	}
	occupied, err := store.OccupiedSlots(ctx)
	if err != nil || len(occupied) != 2 {
		t.Fatalf("occupied slots after first cycle: %v, %+v", err, occupied)
	}

	// No free slot: nothing more may be dispatched no matter how many
	// tasks are ready.
	second, err := dispatch.Cycle(ctx, store, slots, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("cycle with no free slot dispatched %+v", second)
	}

	// Finishing one run frees exactly one slot, so exactly one more task
	// starts, in the freed slot.
	if err := store.FinishRun(ctx, first[0].RunID, now.Add(2*time.Minute), "succeeded"); err != nil {
		t.Fatal(err)
	}
	third, err := dispatch.Cycle(ctx, store, slots, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 1 {
		t.Fatalf("cycle after one finish dispatched %+v, want exactly 1", third)
	}
	if third[0].Slot != first[0].Slot {
		t.Errorf("dispatch landed on slot %q, want the freed slot %q", third[0].Slot, first[0].Slot)
	}
}

func TestCycleSkipsBlockedTasksUntilTheirDependencyCloses(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx,
		task("blocker", true),
		task("blocked", true, model.Link{Kind: model.LinkDependsOn, Target: "blocker"}),
	)

	got, err := dispatch.Cycle(ctx, store, []string{"slot-1", "slot-2"}, now)
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
	got, err = dispatch.Cycle(ctx, store, []string{"slot-2"}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TaskID != "blocked" {
		t.Fatalf("expected the formerly-blocked task dispatched after its dependency closed, got %+v", got)
	}
}

func TestAttemptNumberIncrementsOnEachRedispatch(t *testing.T) {
	store, ctx := open(t)
	putTasks(t, store, ctx, task("t0", true))
	slots := []string{"slot-1"}

	first, err := dispatch.Cycle(ctx, store, slots, now)
	if err != nil || len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("first dispatch: %v, %+v", err, first)
	}

	// Finishing with no Observation is a requeue, not a completion — the
	// task must come back through task_ready exactly as before.
	if err := store.FinishRun(ctx, first[0].RunID, now.Add(time.Minute), "requeued"); err != nil {
		t.Fatal(err)
	}
	second, err := dispatch.Cycle(ctx, store, slots, now.Add(2*time.Minute))
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

	slots := []string{"slot-1", "slot-2", "slot-3"}
	closed := map[string]bool{}
	liveRunID := map[string]string{}  // task -> its current run, while live
	liveSlot := map[string]string{}   // task -> the slot it occupies, while live
	occupiedBy := map[string]string{} // slot -> task, while occupied
	attemptsSoFar := map[string]int{}
	clock := now

	unblocked := func(id string) bool {
		parent, ok := dependsOn[id]
		return !ok || closed[parent]
	}

	for cycle := 0; cycle < 60; cycle++ {
		clock = clock.Add(time.Minute)
		dispatches, err := dispatch.Cycle(ctx, store, slots, clock)
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}

		for _, d := range dispatches {
			if _, already := liveRunID[d.TaskID]; already {
				t.Fatalf("cycle %d: %s dispatched while already running", cycle, d.TaskID)
			}
			if occupant, taken := occupiedBy[d.Slot]; taken {
				t.Fatalf("cycle %d: slot %s given to %s while still holding %s",
					cycle, d.Slot, d.TaskID, occupant)
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
			liveSlot[d.TaskID] = d.Slot
			occupiedBy[d.Slot] = d.TaskID
			attemptsSoFar[d.TaskID]++
		}

		if len(occupiedBy) > len(slots) {
			t.Fatalf("cycle %d: %d slots occupied, only %d exist", cycle, len(occupiedBy), len(slots))
		}
		if occ, err := store.OccupiedSlots(ctx); err != nil {
			t.Fatalf("cycle %d: OccupiedSlots: %v", cycle, err)
		} else if len(occ) != len(occupiedBy) {
			t.Fatalf("cycle %d: store reports %d occupied slots, mirror expects %d: %v",
				cycle, len(occ), len(occupiedBy), occ)
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
			if err := store.FinishRun(ctx, runID, clock, "outcome"); err != nil {
				t.Fatalf("cycle %d: finishing %s: %v", cycle, runID, err)
			}
			delete(occupiedBy, liveSlot[id])
			delete(liveRunID, id)
			delete(liveSlot, id)

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
