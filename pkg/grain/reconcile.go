package grain

import (
	"fmt"
	"time"
)

// Policy is what the controller decides rather than the grain. Each of
// these is a judgement that needs a view from outside one grain: how long
// a boot is allowed to take before it is a failure rather than a wait, and
// how much self-repair is repair rather than thrashing.
type Policy struct {
	// ProvisionBudget bounds PhaseProvisioning. It covers the one stretch
	// a grain cannot bound for itself: a container that never started, or
	// a guest that never answered, is exactly the grain that cannot
	// report being stuck.
	ProvisionBudget time.Duration
	// MaxRebuilds backstops Limits.MaxRebuilds, which the shim enforces.
	// Both exist because they fail differently: the shim's is the fast
	// one, and this is the one that still works when the shim is what is
	// wrong.
	MaxRebuilds int
}

// DefaultPolicy is the shape a deployment gets when it names nothing. The
// provision budget is generous because a real guest boot is genuinely
// slow (orchestrator.KonturConfig.readyTimeout is two minutes for the VM
// alone, before a clone or a setup command).
func DefaultPolicy() Policy {
	return Policy{ProvisionBudget: 10 * time.Minute, MaxRebuilds: 3}
}

// RunRow is the controller's own record of the run a grain is serving --
// the fields of model.Run and its task that Reconcile needs, and no more,
// so that the decision below can be exercised without a store.
type RunRow struct {
	ID     string
	TaskID string
	// Live is false once the run has been finished in the store. A grain
	// still standing against a finished row is one whose finish already
	// happened and whose release has not.
	Live bool
	// TaskClosed is a task closed while its grain was still running --
	// the case orchestrator.watchForTaskClosed polls for today.
	TaskClosed bool
	// Paused is the deployment having met the agent's own usage limit,
	// which stops every grain rather than letting each spend an agent's
	// worth of wall clock discovering the same refusal.
	Paused bool
	// PromptSent records that SignalPrompt has already been delivered, so
	// a controller that polls again before the phase moves does not send
	// a second one.
	PromptSent bool
	// PendingAddenda are comments added since this grain started, oldest
	// first, that have not been delivered yet.
	PendingAddenda []string
	// Activity is what the row currently says the grain is doing, so a
	// tick that changes nothing writes nothing.
	Activity string
}

// Observed is one grain as one tick sees it: what the grain says about
// itself, what the store says about the run behind it, and when.
//
// Run is nil for a grain the store knows nothing about, which is the
// ordinary shape of an orphan -- a grain whose controller died between
// creating it and recording it, or one whose run was finished by an
// earlier tick.
type Observed struct {
	Status Status
	Run    *RunRow
	Now    time.Time
}

// ActionKind is what the controller does about a grain this tick.
type ActionKind string

const (
	// ActionSendPrompt is the second half of the two-phase start: build
	// the prompt now that the checkout can be read, and Signal it.
	ActionSendPrompt ActionKind = "send-prompt"
	// ActionSignal delivers Action.Signal.
	ActionSignal ActionKind = "signal"
	// ActionAnswer serves Action.Request -- the one thing that needs the
	// store, GitHub or a human.
	ActionAnswer ActionKind = "answer"
	// ActionRecordActivity copies the grain's own phrase onto the task's
	// row.
	ActionRecordActivity ActionKind = "record-activity"
	// ActionFinish records the grain's own Result and carries out what it
	// implies -- orchestrator's FinishRun followed by ProcessResult.
	ActionFinish ActionKind = "finish"
	// ActionFail finishes a run whose grain never produced a Result of
	// its own: the controller supplies the outcome, because the grain is
	// in no position to.
	ActionFail ActionKind = "fail"
	// ActionRelease destroys the grain.
	ActionRelease ActionKind = "release"
)

// Action is one thing to do. Actions come back in the order they should
// be carried out, and a failure part-way through is safe to abandon: the
// next tick observes whatever actually happened and decides again.
type Action struct {
	Kind     ActionKind
	Signal   Signal
	Request  RequestID
	Outcome  string
	Detail   string
	Activity string
}

// Reconcile decides what to do about one grain this tick. It is pure: no
// store, no backend, no clock of its own -- so the whole of the
// controller's per-grain policy is a table test rather than something
// that needs a VM to exercise. That is the point of splitting it out.
// pkg/orchestrator's equivalent is runOne plus RunDispatch, ~730 lines
// whose behaviour can only be observed by dispatching a real run.
//
// The ordering below is the decision, not an implementation detail:
//
//  1. A grain no live run claims is released, whatever it thinks it is
//     doing. Nothing else can be true of it that matters.
//  2. A lost container is failed and released. There is no repair path,
//     because a wedged guest never reaches here -- the shim rebuilds
//     that itself.
//  3. A grain that has finished is finished with, even if its task was
//     closed or the deployment paused in the meantime. Acting on a
//     result that already exists beats delivering a signal nothing will
//     read.
//  4. Thrashing is checked before the budget: a grain rebuilding in a
//     loop is failing for a reason worth naming, not merely slow.
//  5. Cancellation and pause come before the ordinary steady state, so
//     a grain being stopped is not also handed addenda.
//  6. A provisioning grain past its budget is failed. Nothing else about
//     a grain that cannot finish booting is worth doing.
//  7. Otherwise: start it if it is waiting for a prompt, and keep it
//     served if it is running.
func Reconcile(o Observed, p Policy) []Action {
	st := o.Status

	// 1. Orphan. A grain with no live run behind it holds a container, a
	// VMM and a guest for a run that is over or was never recorded.
	// Releasing an already-released grain is a no-op, so this is stated
	// as a phase check rather than tracked.
	if o.Run == nil || !o.Run.Live {
		if st.Phase == PhaseReleased {
			return nil
		}
		return []Action{{Kind: ActionRelease}}
	}
	if st.Phase == PhaseReleased {
		// Released out from under a live row: something destroyed the
		// container without the run being finished. The row is all that
		// is left, and it needs an ending.
		return []Action{{Kind: ActionFail, Outcome: OutcomeLost,
			Detail: "this run's grain was released while the run was still live"}}
	}

	// 2. The container is gone.
	if st.Phase == PhaseLost {
		return []Action{
			{Kind: ActionFail, Outcome: OutcomeLost, Detail: lostDetail(st)},
			{Kind: ActionRelease},
		}
	}

	// 3. The grain finished on its own terms.
	if st.Phase.Terminal() {
		return []Action{{Kind: ActionFinish}, {Kind: ActionRelease}}
	}

	// 4. Self-repair that is not converging.
	if p.MaxRebuilds > 0 && st.Rebuilds > p.MaxRebuilds {
		return []Action{
			{Kind: ActionFail, Outcome: OutcomeThrashing,
				Detail: fmt.Sprintf("this run's grain rebuilt its sandbox %d times, past the %d it is allowed",
					st.Rebuilds, p.MaxRebuilds)},
			{Kind: ActionRelease},
		}
	}

	// 5. Stop it. Cancelling is a Signal rather than a Release so the
	// grain gets to end on its own terms and report a Result; the next
	// tick sees a terminal phase and releases through case 3. A grain
	// that ignores it is released by case 6's budget or by the run's own
	// MaxRuntime, so cooperation is preferred but not required.
	if o.Run.TaskClosed {
		return []Action{{Kind: ActionSignal, Signal: Signal{Kind: SignalCancel, Reason: "the task was closed"}}}
	}
	if o.Run.Paused {
		return []Action{{Kind: ActionSignal, Signal: Signal{Kind: SignalPause, Reason: "the deployment met its usage limit"}}}
	}

	// 6. Stuck before there was ever an agent.
	if st.Phase == PhaseProvisioning && p.ProvisionBudget > 0 &&
		o.Now.Sub(st.Since) > p.ProvisionBudget {
		return []Action{
			{Kind: ActionFail, Outcome: OutcomeSetupFailed,
				Detail: fmt.Sprintf("this run's sandbox was still being prepared after %s: %s",
					o.Now.Sub(st.Since).Round(time.Second), provisioningDetail(st))},
			{Kind: ActionRelease},
		}
	}

	// 7a. Second half of the start. The checkout exists, so the prompt
	// can be assembled and this is the moment to do it -- late enough to
	// include anything a human added since dispatch, and early enough
	// that no model tokens have been spent on a grain whose checkout
	// failed.
	if st.Phase == PhaseProvisioned && !o.Run.PromptSent {
		return []Action{{Kind: ActionSendPrompt}}
	}

	// 7b. Steady state: serve what it is waiting on, tell it what it
	// missed, and keep its row honest. All three are independent, and a
	// tick that has nothing to do returns nothing at all.
	var out []Action
	for _, r := range st.Requests {
		out = append(out, Action{Kind: ActionAnswer, Request: r.ID})
	}
	if len(o.Run.PendingAddenda) > 0 {
		out = append(out, Action{Kind: ActionSignal,
			Signal: Signal{Kind: SignalAddenda, Addenda: o.Run.PendingAddenda}})
	}
	if st.Activity != o.Run.Activity {
		out = append(out, Action{Kind: ActionRecordActivity, Activity: st.Activity})
	}
	return out
}

// lostDetail says what was last known about a grain whose container is
// gone. Whatever phrase was standing when it went is left standing,
// deliberately: where a grain got to before it vanished is exactly the
// context the outcome alone does not carry.
func lostDetail(st Status) string {
	if st.Activity == "" {
		return "this run's grain could no longer be reached"
	}
	return "this run's grain could no longer be reached, last seen " + st.Activity
}

// provisioningDetail is the same courtesy for a grain that never finished
// booting: the phrase it was on says which step of a preparation ran out
// of budget.
func provisioningDetail(st Status) string {
	if st.Activity == "" {
		return "it reported no progress"
	}
	return "it was still " + st.Activity
}
