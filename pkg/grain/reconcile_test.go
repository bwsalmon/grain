package grain_test

import (
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/grain"
	"github.com/bwsalmon/grain/pkg/granule"
)

var start = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func live() *grain.RunRow {
	return &grain.RunRow{ID: "task-7-1", TaskID: "task-7", Live: true}
}

// kinds is what a case actually asserts on: the sequence of decisions,
// not the prose that goes with them.
func kinds(actions []grain.Action) []grain.ActionKind {
	out := make([]grain.ActionKind, len(actions))
	for i, a := range actions {
		out[i] = a.Kind
	}
	return out
}

func equal(got, want []grain.ActionKind) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The whole of the controller's per-grain policy, exercised with no
// backend, no store and no VM -- which is the point of Reconcile being
// pure. Each case is one row of the table in docs/grain.md.
func TestReconcile(t *testing.T) {
	policy := grain.Policy{ProvisionBudget: 10 * time.Minute, MaxRebuilds: 3}

	cases := []struct {
		name string
		obs  grain.Observed
		want []grain.ActionKind
	}{{
		name: "a grain the store knows nothing about is released",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseRunning, Since: start},
			Run:    nil,
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionRelease},
	}, {
		name: "a grain whose run was already finished is released",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseSucceeded, Since: start},
			Run:    &grain.RunRow{ID: "task-7-1", Live: false},
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionRelease},
	}, {
		name: "an already-released grain needs nothing",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseReleased, Since: start},
			Run:    &grain.RunRow{ID: "task-7-1", Live: false},
			Now:    start.Add(time.Minute),
		},
		want: nil,
	}, {
		// The container went away without the run being finished: the
		// row is all that is left of it and still needs an ending.
		name: "a released grain under a live run is failed",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseReleased, Since: start},
			Run:    live(),
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail},
	}, {
		name: "a lost container is failed and released, with no repair attempt",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseLost, Since: start, Activity: "running the test suite"},
			Run:    live(),
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail, grain.ActionRelease},
	}, {
		name: "a finished grain is finished with and released",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseSucceeded, Since: start,
				Result: &granule.Result{Outcome: granule.OutcomeSucceeded}},
			Run: live(),
			Now: start.Add(time.Hour),
		},
		want: []grain.ActionKind{grain.ActionFinish, grain.ActionRelease},
	}, {
		// Case 3 before case 5: a result that already exists beats
		// destroying the grain that holds it.
		name: "a finished grain whose task was closed is still finished with",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseFailed, Since: start,
				Result: &granule.Result{Outcome: granule.OutcomeFailed}},
			Run: &grain.RunRow{ID: "task-7-1", Live: true, TaskClosed: true},
			Now: start.Add(time.Hour),
		},
		want: []grain.ActionKind{grain.ActionFinish, grain.ActionRelease},
	}, {
		// Rebuilding is the grain's own business right up until it stops
		// converging.
		name: "rebuilds within the cap are left alone",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseRunning, Since: start, Rebuilds: 3},
			Run:    live(),
			Now:    start.Add(time.Minute),
		},
		want: nil,
	}, {
		name: "rebuilds past the cap end the grain",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseRunning, Since: start, Rebuilds: 4},
			Run:    live(),
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail, grain.ActionRelease},
	}, {
		// Killed, in one tick. SIGTERM plus a grace period is the graceful
		// path -- it is what the shim needs to end the run and write its
		// Result -- so there is nothing a separate cancel would buy. The
		// controller supplies the outcome because SIGKILL may win.
		name: "a closed task is failed and destroyed in one tick",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseRunning, Since: start},
			Run:    &grain.RunRow{ID: "task-7-1", Live: true, TaskClosed: true},
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail, grain.ActionRelease},
	}, {
		name: "a paused deployment stops its grains",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseRunning, Since: start},
			Run:    &grain.RunRow{ID: "task-7-1", Live: true, Paused: true},
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail, grain.ActionRelease},
	}, {
		// A grain being stopped is not also reattached.
		name: "a cancelled grain is failed and destroyed",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseRunning, Since: start, Activity: "building"},
			Run:    &grain.RunRow{ID: "task-7-1", Live: true, TaskClosed: true},
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail, grain.ActionRelease},
	}, {
		name: "a boot within budget is waited on",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseProvisioning, Since: start},
			Run:    live(),
			Now:    start.Add(9 * time.Minute),
		},
		want: nil,
	}, {
		name: "a boot past budget is failed and released",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseProvisioning, Since: start,
				Activity: "cloning acme/widgets"},
			Run: live(),
			Now: start.Add(11 * time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail, grain.ActionRelease},
	}, {
		// A grain has its prompt as a file from the moment it is created,
		// so provisioning runs straight into running with nothing to wait
		// for in between.
		name: "a provisioning grain within budget is left to get on with it",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseProvisioning, Since: start,
				Activity: "cloning acme/widgets"},
			Run: &grain.RunRow{ID: "task-7-1", Live: true, Activity: "cloning acme/widgets"},
			Now: start.Add(2 * time.Minute),
		},
		want: nil,
	}, {
		name: "a running grain with nothing outstanding needs nothing",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseRunning, Since: start, Activity: "building"},
			Run:    &grain.RunRow{ID: "task-7-1", Live: true, Activity: "building"},
			Now:    start.Add(time.Minute),
		},
		want: nil,
	}, {
		name: "a running grain's activity is mirrored onto its row",
		obs: grain.Observed{
			Status: granule.Status{Phase: granule.PhaseRunning, Since: start, Activity: "waiting for CI"},
			Run:    &grain.RunRow{ID: "task-7-1", Live: true, Activity: "building"},
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionRecordActivity},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kinds(grain.Reconcile(tc.obs, policy))
			if !equal(got, tc.want) {
				t.Fatalf("Reconcile = %v, want %v", got, tc.want)
			}
		})
	}
}

// A grain that reports the same state twice must be decided the same way
// twice: the controller re-reads everything every tick, so a Reconcile
// that drifted on a second look would make skipping a tick unsafe.
func TestReconcileIsLevelTriggered(t *testing.T) {
	obs := grain.Observed{
		Status: granule.Status{Phase: granule.PhaseRunning, Since: start, Activity: "building"},
		Run:    live(),
		Now:    start.Add(time.Minute),
	}
	first := kinds(grain.Reconcile(obs, grain.DefaultPolicy()))
	second := kinds(grain.Reconcile(obs, grain.DefaultPolicy()))
	if !equal(first, second) {
		t.Fatalf("Reconcile is not level-triggered: %v then %v", first, second)
	}
}

// Releasing is the one action that must be reachable for every phase a
// grain can be found in with no live run behind it, since that is the
// whole of orphan reaping -- there is no separate pass.
func TestEveryOrphanPhaseIsReleased(t *testing.T) {
	phases := []granule.Phase{
		granule.PhaseProvisioning, granule.PhaseRunning, granule.PhaseSucceeded, granule.PhaseFailed,
		granule.PhaseCancelled, granule.PhaseLost,
	}
	for _, p := range phases {
		got := kinds(grain.Reconcile(grain.Observed{
			Status: granule.Status{Phase: p, Since: start},
			Now:    start.Add(time.Minute),
		}, grain.DefaultPolicy()))
		if !equal(got, []grain.ActionKind{grain.ActionRelease}) {
			t.Errorf("orphan in phase %q: Reconcile = %v, want [release]", p, got)
		}
	}
}
