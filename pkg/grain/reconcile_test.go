package grain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/grain"
)

var start = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

// attached is the ordinary steady state: a grain the controller is
// already connected to. Cases that do not set it get an attach action,
// which is correct and would otherwise noise up every expectation.
func attached() grain.Upstream { return grain.Upstream{Attached: true} }

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
			Status: grain.Status{Phase: grain.PhaseRunning, Since: start},
			Run:    nil,
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionRelease},
	}, {
		name: "a grain whose run was already finished is released",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseSucceeded, Since: start},
			Run:    &grain.RunRow{ID: "task-7-1", Live: false},
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionRelease},
	}, {
		name: "an already-released grain needs nothing",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseReleased, Since: start},
			Run:    &grain.RunRow{ID: "task-7-1", Live: false},
			Now:    start.Add(time.Minute),
		},
		want: nil,
	}, {
		// The container went away without the run being finished: the
		// row is all that is left of it and still needs an ending.
		name: "a released grain under a live run is failed",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseReleased, Since: start},
			Run:    live(),
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail},
	}, {
		name: "a lost container is failed and released, with no repair attempt",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseLost, Since: start, Activity: "running the test suite"},
			Run:    live(),
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail, grain.ActionRelease},
	}, {
		name: "a finished grain is finished with and released",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseSucceeded, Since: start,
				Result: &grain.Result{Outcome: grain.OutcomeSucceeded}},
			Run: live(),
			Now: start.Add(time.Hour),
		},
		want: []grain.ActionKind{grain.ActionFinish, grain.ActionRelease},
	}, {
		// Case 3 before case 5: a result that already exists beats
		// destroying the grain that holds it.
		name: "a finished grain whose task was closed is still finished with",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseFailed, Since: start,
				Result: &grain.Result{Outcome: grain.OutcomeFailed}},
			Run: &grain.RunRow{ID: "task-7-1", Live: true, TaskClosed: true},
			Now: start.Add(time.Hour),
		},
		want: []grain.ActionKind{grain.ActionFinish, grain.ActionRelease},
	}, {
		// Rebuilding is the grain's own business right up until it stops
		// converging.
		name: "rebuilds within the cap are left alone",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseRunning, Since: start, Rebuilds: 3,
				Upstream: attached()},
			Run: live(),
			Now: start.Add(time.Minute),
		},
		want: nil,
	}, {
		name: "rebuilds past the cap end the grain",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseRunning, Since: start, Rebuilds: 4},
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
			Status: grain.Status{Phase: grain.PhaseRunning, Since: start},
			Run:    &grain.RunRow{ID: "task-7-1", Live: true, TaskClosed: true},
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail, grain.ActionRelease},
	}, {
		name: "a paused deployment stops its grains",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseRunning, Since: start},
			Run:    &grain.RunRow{ID: "task-7-1", Live: true, Paused: true},
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail, grain.ActionRelease},
	}, {
		// A grain being stopped is not also reattached.
		name: "a cancelled grain is not also attached to",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseRunning, Since: start, Activity: "building"},
			Run:    &grain.RunRow{ID: "task-7-1", Live: true, TaskClosed: true},
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionFail, grain.ActionRelease},
	}, {
		name: "a boot within budget is waited on",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseProvisioning, Since: start,
				Upstream: attached()},
			Run: live(),
			Now: start.Add(9 * time.Minute),
		},
		want: nil,
	}, {
		name: "a boot past budget is failed and released",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseProvisioning, Since: start,
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
			Status: grain.Status{Phase: grain.PhaseProvisioning, Since: start,
				Activity: "cloning acme/widgets", Upstream: attached()},
			Run: &grain.RunRow{ID: "task-7-1", Live: true, Activity: "cloning acme/widgets"},
			Now: start.Add(2 * time.Minute),
		},
		want: nil,
	}, {
		name: "a running grain with nothing outstanding needs nothing",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseRunning, Since: start, Activity: "building",
				Upstream: attached()},
			Run: &grain.RunRow{ID: "task-7-1", Live: true, Activity: "building"},
			Now: start.Add(time.Minute),
		},
		want: nil,
	}, {
		name: "a detached grain is attached to and its activity mirrored",
		obs: grain.Observed{
			Status: grain.Status{Phase: grain.PhaseBlocked, Since: start, Activity: "waiting for CI"},
			Run:    &grain.RunRow{ID: "task-7-1", Live: true, Activity: "building"},
			Now:    start.Add(time.Minute),
		},
		want: []grain.ActionKind{grain.ActionAttach, grain.ActionRecordActivity},
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
		Status: grain.Status{Phase: grain.PhaseRunning, Since: start, Activity: "building",
			Upstream: grain.Upstream{Attached: false, Pending: 1}},
		Run: live(),
		Now: start.Add(time.Minute),
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
	phases := []grain.Phase{
		grain.PhaseProvisioning, grain.PhaseRunning,
		grain.PhaseBlocked, grain.PhaseSucceeded, grain.PhaseFailed,
		grain.PhaseCancelled, grain.PhaseLost,
	}
	for _, p := range phases {
		got := kinds(grain.Reconcile(grain.Observed{
			Status: grain.Status{Phase: p, Since: start},
			Now:    start.Add(time.Minute),
		}, grain.DefaultPolicy()))
		if !equal(got, []grain.ActionKind{grain.ActionRelease}) {
			t.Errorf("orphan in phase %q: Reconcile = %v, want [release]", p, got)
		}
	}
}

// A grain the controller is not connected to cannot serve any tool but
// its own six, and one that has never been connected has not started its
// agent at all -- so attaching is what a tick does about it, every tick,
// until it takes.
func TestReconcileAttachesADetachedGrain(t *testing.T) {
	obs := grain.Observed{
		Status: grain.Status{Phase: grain.PhaseRunning, Since: start, Activity: "building",
			Upstream: grain.Upstream{Attached: false, Pending: 2}},
		Run: &grain.RunRow{ID: "task-7-1", Live: true, Activity: "building"},
		Now: start.Add(time.Minute),
	}
	got := kinds(grain.Reconcile(obs, grain.DefaultPolicy()))
	if !equal(got, []grain.ActionKind{grain.ActionAttach}) {
		t.Fatalf("Reconcile = %v, want [attach]", got)
	}

	// And stops once it has: attaching twice would displace a live
	// connection the agent is mid-call on.
	obs.Status.Upstream = attached()
	if got := kinds(grain.Reconcile(obs, grain.DefaultPolicy())); len(got) != 0 {
		t.Fatalf("Reconcile on an attached grain = %v, want nothing", got)
	}
}

// A grain waits for its first attach before starting its agent, because
// an absent tool is not an error: an agent never told open_pull_request
// exists finishes without opening one and reports success. So the wait
// has to be visible, and it has to end -- loudly -- rather than becoming
// a run that quietly did less than it should have.
func TestAGrainNeverAttachedToIsAttachedThenEventuallyFailed(t *testing.T) {
	waiting := grain.Observed{
		Status: grain.Status{Phase: grain.PhaseProvisioning, Since: start,
			Activity: grain.AwaitingUpstreamNote},
		Run: &grain.RunRow{ID: "task-7-1", Live: true, Activity: grain.AwaitingUpstreamNote},
		Now: start.Add(time.Minute),
	}
	if got := kinds(grain.Reconcile(waiting, grain.DefaultPolicy())); !equal(got, []grain.ActionKind{grain.ActionAttach}) {
		t.Fatalf("Reconcile = %v, want [attach] for a grain nobody has connected to", got)
	}

	// Past the budget it is failed and released, and the detail carries
	// the phrase -- which is what turns "provisioning timed out" into
	// "nobody ever attached".
	waiting.Now = start.Add(11 * time.Minute)
	actions := grain.Reconcile(waiting, grain.DefaultPolicy())
	if got := kinds(actions); !equal(got, []grain.ActionKind{grain.ActionFail, grain.ActionRelease}) {
		t.Fatalf("Reconcile = %v, want [fail release] past the provision budget", got)
	}
	if !strings.Contains(actions[0].Detail, grain.AwaitingUpstreamNote) {
		t.Errorf("the failure detail does not say what it was waiting for: %q", actions[0].Detail)
	}
}
