package e2e

// TestRefusedCapabilityGrantFailsTheRunBeforeTheAgentStartsAndRequeues is
// bwsalmon/agents#330: the capability-refusal counterpart to
// TestFailedRunReturnsTaskToQueueForRetry in e2e_test.go, proving the same
// "the task comes back around for a retry" outcome but reached through a
// refused model.Grant instead of a denied push. pkg/orchestrator/run_test.go's
// TestRunDispatchFinishesTheRunAsFailedWhenACapabilityIsRefused already
// proves the refusal itself, directly against orchestrator.RunDispatch;
// this proves it reached from a real dispatch.Cycle decision against this
// package's own store/git rig, and that no branch and no Observation ever
// result from it.

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/dispatch"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

// refusingCapability is a model.CapabilityProvider whose Resolve always
// refuses, mirroring pkg/model/capability_test.go's own
// mintCapability{refuse: ...} stand-in.
type refusingCapability struct {
	model.BaseCapability
	name   string
	reason string
}

func (c refusingCapability) Spec() model.CapabilitySpec {
	return model.CapabilitySpec{Name: c.name, Label: "grain-" + c.name, Source: model.GrantByLabel, Provision: model.ProvisionGrant}
}

func (c refusingCapability) Resolve(context.Context, model.CapabilityContext) (model.Resolution, error) {
	return model.RefusedBecause(c.reason), nil
}

// panicIfRun is an agent.Framework that panics the moment Run is called --
// prepareCapabilities' own doc comment says a refused grant "must not run
// [the agent] at all," so this framework makes the test fail loudly,
// rather than quietly pass, if that short-circuit ever slips.
type panicIfRun struct{}

func (panicIfRun) Run(context.Context, agent.RunConfig) (*agent.Result, error) {
	panic("agent framework Run must not be called when a capability grant was refused")
}

func TestRefusedCapabilityGrantFailsTheRunBeforeTheAgentStartsAndRequeues(t *testing.T) {
	const slot = "sandbox-bd453be9-cap-1"
	w := newWorld(t, []string{slot})
	w.newRepo("acme", "widgets")

	clock := baseTime
	task := fileIssue(w, "iss-cap", human("dave"), model.RepoRef{Owner: "acme", Name: "widgets"})
	task.Grants = []model.Grant{{Capability: "locked", Via: model.GrantByLabel}}
	if err := w.store.PutTask(w.ctx, task); err != nil {
		t.Fatalf("attaching a grant to iss-cap: %v", err)
	}
	assertState(w, "iss-cap", model.StateQueued, false)

	dispatches, err := dispatch.Cycle(w.ctx, w.store, []string{slot}, clock)
	if err != nil || len(dispatches) != 1 || dispatches[0].TaskID != "iss-cap" {
		t.Fatalf("Cycle: %v, %+v", err, dispatches)
	}
	d := dispatches[0]
	assertState(w, "iss-cap", model.StateRunning, true)

	dispatched, err := w.store.GetTask(w.ctx, "iss-cap")
	if err != nil || dispatched == nil {
		t.Fatalf("reading dispatched task: %v (nil=%v)", err, dispatched == nil)
	}

	cap := refusingCapability{name: "locked", reason: "not for you"}
	cfg := orchestrator.Config{Capabilities: model.NewCapabilityRegistry(cap)}

	clock = clock.Add(time.Minute)
	if _, err := orchestrator.RunDispatch(w.ctx, w.store, panicIfRun{}, cfg, *dispatched, d, nil, w.roots[slot], clock); err == nil {
		t.Fatal("expected RunDispatch to report the refusal")
	}

	branch := model.BranchName("iss-cap")
	if w.branchExists("acme", "widgets", branch) {
		t.Fatal("a refused capability grant must never push a branch")
	}

	occupied, err := w.store.OccupiedSlots(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(occupied) != 0 {
		t.Errorf("occupied slots after a refused capability = %v, want none", occupied)
	}

	obs, err := w.store.GetObservation(w.ctx, "iss-cap")
	if err != nil {
		t.Fatal(err)
	}
	if obs != nil {
		t.Errorf("observation after a refused capability = %+v, want none -- a refused run reaches no agent to observe anything", obs)
	}

	// Requeued, not stuck: the same "half-materialized capability is
	// never described to the agent as present" rule prepareCapabilities'
	// own doc comment holds to means this run failed before it could do
	// anything at all, so the task must be dispatchable again.
	assertState(w, "iss-cap", model.StateQueued, false)

	second, err := dispatch.Cycle(w.ctx, w.store, []string{slot}, clock)
	if err != nil || len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("retry Cycle: %v, %+v, want attempt 2", err, second)
	}
}
