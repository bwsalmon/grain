package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/agent"
	"github.com/bwsalmon/grain/v2/pkg/dispatch"
	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

// errTaskClosed is what context.Cause(runCtx) reads once
// watchForTaskClosed has cancelled a run -- RunDispatch checks for it by
// identity (errors.Is) to tell "this run was killed because its task got
// closed" apart from any other reason framework.Run might return an
// error, and to record outcome "cancelled" rather than "failed" for
// exactly that case.
var errTaskClosed = errors.New("orchestrator: task closed while its run was still live")

// checkTaskClosed reads store.State(taskID) once and calls
// cancel(errTaskClosed) if it reads model.StateClosed, reporting whether
// it did. A store error is treated as "not closed" rather than
// propagated: watchForTaskClosed's own caller retries on the next tick
// regardless, so a transient read failure costs one interval of latency,
// not the ability to ever notice a close for the rest of the run.
func checkTaskClosed(ctx context.Context, store *model.Store, taskID string, cancel context.CancelCauseFunc) bool {
	st, err := store.State(ctx, taskID)
	if err != nil {
		return false
	}
	if st == model.StateClosed {
		cancel(errTaskClosed)
		return true
	}
	return false
}

// watchForTaskClosed polls store.State(taskID) every interval and calls
// cancel(errTaskClosed) the moment it reads model.StateClosed -- the
// store-polled cancellation signal RunDispatch needs because grain daemon
// (running the agent) and grain ui (where a close actually lands) are
// separate processes sharing only the store, per bwsalmon/agents#346.
//
// It does not check immediately on entry: RunDispatch itself already
// calls checkTaskClosed synchronously, before framework.Run is ever
// invoked, which is what makes a task already closed by the time
// RunDispatch runs -- dispatch.Cycle claimed the slot before the close
// write landed, and RunDispatch only got around to running it after --
// stop that run's first tool call from ever reaching a real sandbox,
// deterministically, rather than racing this goroutine's own first tick
// against a real subprocess. See RunDispatch's own doc comment.
//
// queryCtx, not runCtx, bounds the store reads: runCtx is what this func
// is about to cancel, so querying with it would make the very last read
// racy against its own effect. It returns as soon as runCtx.Done() fires
// for any reason, which is what bounds this goroutine's lifetime to the
// run's: RunDispatch cancels runCtx itself the moment framework.Run
// returns.
func watchForTaskClosed(runCtx, queryCtx context.Context, store *model.Store, taskID string, interval time.Duration, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-ticker.C:
			if checkTaskClosed(queryCtx, store, taskID, cancel) {
				return
			}
		}
	}
}

// BuildPrompt is the prompt a dispatched run receives — deliberately
// plain: the task's own title and body plus exactly the two facts that
// are grain's own, never the agent's to guess (which branch to push, and
// which repo it lives in), the same "deterministic, not self-reported"
// reasoning model.BranchName's own doc comment gives, restated here at the
// one place that fact reaches the prompt.
func BuildPrompt(task model.Task) string {
	return fmt.Sprintf(
		"%s\n\n%s\n\nWork in %s. Push your change to a new branch named %q -- "+
			"never to the repo's default branch directly.",
		task.Title, task.Body, task.Target, model.BranchName(task.ID),
	)
}

// rootedSandboxes is implemented by a Sandboxes backend that also hands
// out a plain local directory for a slot -- HostSandboxes' own RootFor.
// RunDispatch needs one of these to write a capability's SideSandbox
// placements directly to disk; KonturSandboxes (SSH-backed, with no local
// directory of its own) does not implement it, so a caller dispatching a
// task with Grants against it must resolve that itself before calling
// RunDispatch -- see runOne.
type rootedSandboxes interface {
	RootFor(slot string) (string, error)
}

// RunDispatch drives one dispatch.Dispatch to completion: resolve and
// materialize its task's capabilities (writing any SideSandbox placements
// into sandboxRoot, which may be empty when the task has none to place),
// run the agent against tools (whatever Deps.Sandboxes.ToolsFor produced
// for d.Slot -- see the package doc comment on the local-directory-vs-
// real-VM choice that makes), revoke whatever was materialized, and
// record the run's outcome. Every path here finishes the run, even a
// failing one -- ported from pkg/orchestrate's own runDispatch
// (bwsalmon/agents#254) when that package merged into this one: an
// unfinished run would hold its slot forever. It does not touch
// task_observation or GitHub at all -- see ProcessResult for that half,
// kept separate the same way v2/e2e's own runDispatch and its caller are,
// since deciding what a run produced is a different question from
// deciding what to do about it.
//
// While the agent runs, a background watchForTaskClosed goroutine polls
// store for this task being closed and cancels the ctx the agent itself
// (and every tool call it makes) was given the moment it sees that --
// bwsalmon/agents#346's "actually terminate a running task's sandbox
// process on cancel": a task closed through `grain ui`'s Cancel button
// reaches this run, in whatever separate grain daemon process is
// actually running it, only because both share the one store. A run
// killed this way finishes with outcome "cancelled", distinct from
// "failed", and returns a non-nil error wrapping errTaskClosed.
func RunDispatch(ctx context.Context, store *model.Store, framework agent.Framework,
	cfg Config, task model.Task, d dispatch.Dispatch, tools []mcp.Tool, sandboxRoot string, at time.Time) (*agent.Result, error) {

	run := model.Run{ID: d.RunID, TaskID: d.TaskID, Slot: d.Slot, Sandbox: d.Slot, Attempt: d.Attempt, StartedAt: at}
	cc := model.CapabilityContext{Task: task, Run: run, Now: at, Workdir: sandboxRoot, Credentials: cfg.Credentials}

	materialized, prompt, prepErr := prepareCapabilities(ctx, cfg.Capabilities, cc, sandboxRoot)

	var result *agent.Result
	var runErr error
	outcome := "failed"
	switch {
	case prepErr != nil:
		runErr = fmt.Errorf("orchestrator: preparing %s: %w", d.RunID, prepErr)
	default:
		// runCtx, not ctx, is what framework.Run actually gets: it is
		// what watchForTaskClosed cancels the instant it sees this task
		// closed, which is what makes cancelling this run from outside
		// the process running it possible at all -- see that func's own
		// doc comment. cancelRun(nil) once framework.Run returns is what
		// stops the watcher goroutine either way, whether or not it was
		// the one that ended the run.
		runCtx, cancelRun := context.WithCancelCause(ctx)
		checkTaskClosed(ctx, store, task.ID, cancelRun)
		watcherDone := make(chan struct{})
		go func() {
			defer close(watcherDone)
			watchForTaskClosed(runCtx, ctx, store, task.ID, cfg.cancelPollInterval(), cancelRun)
		}()

		result, runErr = framework.Run(runCtx, agent.RunConfig{Prompt: prompt, Tools: tools, MaxTurns: cfg.MaxAgentTurns})
		cancelRun(nil)
		<-watcherDone

		switch {
		case runErr != nil && errors.Is(context.Cause(runCtx), errTaskClosed):
			outcome = "cancelled"
			runErr = fmt.Errorf("orchestrator: run %s: %w", d.RunID, errTaskClosed)
		case runErr != nil:
			runErr = fmt.Errorf("orchestrator: running %s: %w", d.RunID, runErr)
		default:
			outcome = outcomeOf(result)
		}
	}

	finishErr := store.FinishRun(ctx, d.RunID, at, outcome)
	revokeAll(ctx, store, cc, materialized)
	if finishErr != nil {
		return nil, fmt.Errorf("orchestrator: finishing run %s: %w", d.RunID, finishErr)
	}
	if runErr != nil {
		return nil, runErr
	}
	return result, nil
}

// outcomeOf reads agent.Result.ToolCalls -- the only record of what
// happened inside the run (mcp/mock_tools.go's own sink is internal and
// discarded when Run returns) -- and turns it into a run outcome: any
// error tool call fails the run, and so does a run that made no tool call
// at all, since an agent that never touched run_command did not do the
// work. Ported from pkg/orchestrate's own runAgent (bwsalmon/agents#254).
func outcomeOf(result *agent.Result) string {
	sawTool := false
	for _, c := range result.ToolCalls {
		sawTool = true
		if c.IsError {
			return "failed"
		}
	}
	if !sawTool {
		return "failed"
	}
	return "succeeded"
}

// prepareCapabilities resolves and materializes cc.Task's capability
// grants against reg and assembles the prompt they and the task itself
// contribute, applying every SideSandbox placement under sandboxRoot on
// the way. A nil registry, or a task with no Grants, skips all of this
// and returns BuildPrompt's own prompt unchanged -- a deployment or test
// that grants no capabilities needs to configure none of this. A non-nil
// error means preparation itself failed (or a grant was refused) and the
// caller must not run the agent at all -- the same "a half-materialized
// capability is never described to the agent as present" rule
// model.MaterializeGrants's own doc comment holds to, one level up: an
// agent whose capability request was refused must not run at all, since
// the task it would work almost always depends on it. Ported from
// pkg/orchestrate's own prepare (bwsalmon/agents#254).
func prepareCapabilities(ctx context.Context, reg *model.CapabilityRegistry,
	cc model.CapabilityContext, sandboxRoot string) (materialized []model.Materialized, prompt string, err error) {

	prompt = BuildPrompt(cc.Task)
	if reg == nil || len(cc.Task.Grants) == 0 {
		return nil, prompt, nil
	}

	resolved, err := model.ResolveGrants(ctx, reg, cc)
	if err != nil {
		return nil, prompt, fmt.Errorf("resolving capabilities: %w", err)
	}
	for _, gr := range resolved {
		if gr.Resolution.Refused {
			return nil, prompt, fmt.Errorf("capability %q refused: %s", gr.Grant.Capability, gr.Resolution.Reason)
		}
	}

	materialized, err = model.MaterializeGrants(ctx, reg, cc, resolved)
	if err != nil {
		return materialized, prompt, fmt.Errorf("materializing capabilities: %w", err)
	}
	if err := applyPlacements(sandboxRoot, materialized); err != nil {
		return materialized, prompt, fmt.Errorf("applying placements: %w", err)
	}
	sections, err := model.PromptSections(ctx, cc, materialized)
	if err != nil {
		return materialized, prompt, fmt.Errorf("building prompt sections: %w", err)
	}
	for _, s := range sections {
		prompt += "\n\n" + s
	}
	return materialized, prompt, nil
}

// applyPlacements writes every SideSandbox placement Materialize returned
// into root, the local directory standing in for the sandbox -- see the
// package doc comment. A placement's Path is always the absolute path it
// would land at inside a real sandbox (gcpkey.SandboxKeyPath,
// geminikey.KeyPath); root here plays the same role a chroot's own root
// plays for an absolute path, which is why it is joined after stripping
// the leading separator rather than passed through mcp.resolvePath --
// that function confines an agent's own tool arguments, a different
// question from where a controller-applied placement lands.
//
// A SideController placement is skipped, not written anywhere: no
// current provider returns one (gcpkey and geminikey both mint straight
// into the sandbox), and nothing in v2 has decided what a controller-side
// destination for one even is yet. Ported from pkg/orchestrate
// (bwsalmon/agents#254).
func applyPlacements(root string, materialized []model.Materialized) error {
	for _, m := range materialized {
		for _, p := range m.Materialization.Placements {
			if p.Side != model.SideSandbox {
				continue
			}
			full := filepath.Join(root, strings.TrimPrefix(p.Path, "/"))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return fmt.Errorf("creating %s: %w", filepath.Dir(full), err)
			}
			mode, err := strconv.ParseUint(p.EffectiveMode(), 8, 32)
			if err != nil {
				return fmt.Errorf("placement %s: invalid mode %q: %w", p.Path, p.Mode, err)
			}
			if err := os.WriteFile(full, []byte(p.Content), os.FileMode(mode)); err != nil {
				return fmt.Errorf("writing %s: %w", full, err)
			}
		}
	}
	return nil
}

// revokeAll calls Revoke on every capability materialized carried a Lease
// for, then drops it from the store -- the mirror image of
// prepareCapabilities' MaterializeGrants call, run unconditionally
// (success or failure) so a failed run never strands a minted credential
// the way it would if revocation only ran on the happy path. A revoke or
// DropLease failure is logged rather than propagated: the run itself has
// already been finished by the time this runs, and there is nothing left
// to do differently beyond leaving the lease live until an operator or
// model.Reaper notices. Ported from pkg/orchestrate's own revoke
// (bwsalmon/agents#254).
func revokeAll(ctx context.Context, store *model.Store, cc model.CapabilityContext, materialized []model.Materialized) {
	for _, m := range materialized {
		lease := m.Materialization.Lease
		if lease == nil {
			continue
		}
		if err := m.Provider.Revoke(ctx, cc, *lease); err != nil {
			log.Printf("orchestrator: task %s: revoking capability %q: %v", cc.Task.ID, m.Grant.Capability, err)
			continue
		}
		if err := store.DropLease(ctx, cc.Run.ID, lease.Capability, lease.Resource); err != nil {
			log.Printf("orchestrator: task %s: dropping lease for capability %q: %v", cc.Task.ID, m.Grant.Capability, err)
		}
	}
}
