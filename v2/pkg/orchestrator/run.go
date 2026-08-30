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
// plain: the task's own title and body plus the facts that are grain's
// own, never the agent's to guess (which branch to push, which repo it
// lives in, and which other repos it may read), the same "deterministic,
// not self-reported" reasoning model.BranchName's own doc comment gives,
// restated here at the one place those facts reach the prompt.
//
// checkoutDir, when non-empty, is the directory RunDispatch already
// cloned the target into (prepareCheckout's own CheckoutDir) -- said here
// because an agent that is not told cannot know, which is the whole
// failure that made the clone happen up front. Empty is a sandbox left
// bare, and leaves this prompt exactly as it always read.
//
// task.Reads is mentioned but not enforced here: the git proxy already
// allows a fetch against any of them and refuses a push to any but
// task.Target (gitproxy/authorize.go), so this line is purely
// informational -- it tells the agent those repos exist and are safe to
// clone, rather than granting anything itself.
func BuildPrompt(task model.Task, checkoutDir string) string {
	branch := model.BranchName(task.ID)
	prompt := fmt.Sprintf(
		"%s\n\n%s\n\nWork in %s. Push your change to a new branch named %q -- "+
			"never to the repo's default branch directly.",
		task.Title, task.Body, task.Target, branch,
	)
	if checkoutDir != "" {
		prompt += fmt.Sprintf(
			"\n\nThat repo is already cloned for you at ./%s, with %q checked out and "+
				"its remote pointing at the only address you can reach it through -- "+
				"work in that directory rather than cloning anything yourself, and "+
				"push with `git push origin %s`.",
			checkoutDir, branch, branch,
		)
	}
	if len(task.Reads) > 0 {
		names := make([]string, len(task.Reads))
		for i, r := range task.Reads {
			names[i] = r.String()
		}
		prompt += fmt.Sprintf(
			"\n\nYou may also read %s for reference -- clone them if you need to, "+
				"but never push to them.",
			strings.Join(names, ", "),
		)
	}
	return prompt
}

// commentThreadSection renders task's conversation into a prompt section,
// or "" if there is none yet -- a task's first dispatch always gets "",
// since nothing has been said about it until a run itself says something
// or a human replies to it.
//
// Without this, a redispatched run has no way to see a human's answer to
// a question it parked on with ask_question, or any other comment left
// while it wasn't running: RunDispatch used to build every dispatch's
// prompt from task.Title and task.Body alone, so a task that asked a
// question and got answered would redispatch, ask the identical question
// again, and go straight back to awaiting_reply -- forever, since nothing
// about the run ever changed (bwsalmon/agents#402).
func commentThreadSection(comments []model.Comment) string {
	if len(comments) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Conversation on this task so far, oldest first -- read it before doing " +
		"anything else, since it may already answer a question you would otherwise ask again:\n")
	for _, c := range comments {
		fmt.Fprintf(&b, "- %s: %s\n", attributionLabel(c.Author), c.Body)
	}
	return b.String()
}

// attributionLabel renders who said a comment, from the redispatched
// run's own point of view -- "you" for grain relaying this same task's
// own earlier ask_question or comment_on_issue call (Attribution.OnBehalfOf
// naming a PrincipalAgent, exactly relayComment's own attribution in
// finish.go), so a run recognizes its own prior words rather than reading
// them as some third party's, and "a human" for a direct reply, which is
// the answer a parked ask_question was waiting on.
func attributionLabel(a model.Attribution) string {
	if a.OnBehalfOf != nil {
		switch a.OnBehalfOf.Kind {
		case model.PrincipalAgent:
			return "you, in an earlier attempt at this task"
		case model.PrincipalHuman:
			return "a human"
		}
	}
	switch a.Actor.Kind {
	case model.PrincipalHuman:
		return "a human"
	case model.PrincipalAgent:
		return "an earlier attempt at this task"
	default:
		return "grain"
	}
}

// addendumText renders one just-arrived comment for a Framework to fold
// into its conversation mid-run -- present tense, unlike
// attributionLabel's own labels ("an earlier attempt at this task"),
// which are phrased for a redispatched run looking back at its whole
// history rather than for something that landed seconds ago in the run
// currently reading it.
func addendumText(c model.Comment) string {
	who := "a human"
	if c.Author.OnBehalfOf == nil && c.Author.Actor.Kind != model.PrincipalHuman {
		who = "grain"
	}
	return fmt.Sprintf(
		"%s just added this to the task while you're already working on it -- read it and factor it in:\n\n%s",
		who, c.Body,
	)
}

// addendaPoller returns an agent.RunConfig.Addenda function bound to
// store and taskID, for the one Framework that can actually use it
// (agent.RunConfig.Addenda's own doc comment) to pick up a comment
// posted while this run is still live, rather than only seeing it folded
// into the task's next dispatch (commentThreadSection).
//
// seen is the same slice RunDispatch already read to build this run's
// own prompt -- its highest comment id seeds the cursor here, so the
// first poll only ever returns what arrived after dispatch, never
// something already sitting in the prompt this run started with.
func addendaPoller(store *model.Store, taskID string, seen []model.Comment) func(context.Context) ([]string, error) {
	var lastID int64
	for _, c := range seen {
		if c.ID > lastID {
			lastID = c.ID
		}
	}
	return func(ctx context.Context) ([]string, error) {
		comments, err := store.Comments(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: polling %s's conversation for addenda: %w", taskID, err)
		}
		var addenda []string
		for _, c := range comments {
			if c.ID <= lastID {
				continue
			}
			lastID = c.ID
			addenda = append(addenda, addendumText(c))
		}
		return addenda, nil
	}
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

	comments, err := store.Comments(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: reading %s's conversation: %w", task.ID, err)
	}

	// Before capabilities, and before the agent's first turn: a run whose
	// sandbox never got its repo has nothing to do, and there is no point
	// minting a capability's credentials for it. See prepareCheckout.
	//
	// Skipped for a task already closed by the time its dispatch got
	// here -- the same race the synchronous checkTaskClosed below exists
	// for (bwsalmon/agents#346), whose rule is that such a run never
	// reaches its sandbox at all. A clone is the run's own first touch of
	// that sandbox, so it has to observe the rule too; the closed case
	// then falls through to the cancellation path below and finishes
	// "cancelled" exactly as it did before. A read error here is treated
	// as "not closed": the cancellation path re-reads it anyway, so the
	// cost of being wrong is one clone, not a run that ignores a close.
	var checkoutDir string
	var checkoutErr error
	if closed, err := taskClosed(ctx, store, task.ID); err != nil || !closed {
		checkoutDir, checkoutErr = prepareCheckout(ctx, tools, cfg.GitRemoteBase, task)
	}

	var materialized []model.Materialized
	var prompt string
	var prepErr error
	if checkoutErr == nil {
		materialized, prompt, prepErr = prepareCapabilities(ctx, cfg.Capabilities, cc, sandboxRoot, comments, checkoutDir)
	}

	var result *agent.Result
	var runErr error
	outcome, detail := "failed", ""
	// transcriptPath names a file cfg.TranscriptDir's own doc comment says
	// a Framework may mirror this run's transcript-in-progress into live;
	// "" (cfg.TranscriptDir unset) leaves agent.RunConfig.TranscriptPath
	// empty, which every Framework already treats as "no caller wants
	// this". Computed before the switch below because the cleanup after
	// it (once the store already has this run's own final story) needs it
	// too, whether or not framework.Run ever actually ran.
	var transcriptPath string
	if cfg.TranscriptDir != "" {
		transcriptPath = filepath.Join(cfg.TranscriptDir, d.RunID)
	}
	switch {
	case checkoutErr != nil:
		runErr = fmt.Errorf("orchestrator: preparing %s: %w", d.RunID, checkoutErr)
		detail = checkoutErr.Error()
	case prepErr != nil:
		runErr = fmt.Errorf("orchestrator: preparing %s: %w", d.RunID, prepErr)
		detail = prepErr.Error()
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

		result, runErr = framework.Run(runCtx, agent.RunConfig{
			Prompt: prompt, Tools: tools, MaxTurns: cfg.MaxAgentTurns, TranscriptPath: transcriptPath,
			Addenda: addendaPoller(store, task.ID, comments),
		})
		cancelRun(nil)
		<-watcherDone

		switch {
		case runErr != nil && errors.Is(context.Cause(runCtx), errTaskClosed):
			outcome = "cancelled"
			detail = "the task was closed while this run was still live"
			runErr = fmt.Errorf("orchestrator: run %s: %w", d.RunID, errTaskClosed)
		case runErr != nil:
			// runErr's own text, not the wrapped form below: that form
			// repeats d.RunID, which is already this row's own id, and
			// buries the one thing worth a human's own read (gemini's
			// "exceeded max turns (20) without a final answer", a tool
			// framework's own connection error) under two layers of
			// "orchestrator: running ...: ".
			//
			// A framework may hand back what the run managed to do before
			// it broke (see agent.Framework), and that half answers the
			// question the error itself raises. "exceeded max turns (100)
			// without a final answer" says only that the budget ran out;
			// followed by the tools it spent that budget on, it says
			// whether the run was working or spinning.
			detail = runErr.Error() + partialWorkSuffix(result)
			runErr = fmt.Errorf("orchestrator: running %s: %w", d.RunID, runErr)
		default:
			outcome, detail = outcomeOf(result)
		}
	}

	finishErr := store.FinishRun(ctx, d.RunID, at, outcome, detail)
	if finishErr == nil && result != nil && result.Transcript != "" {
		// A separate write, after FinishRun's own -- see
		// Store.SetRunTranscript's own doc comment on why (bwsalmon/
		// agents#446). Skipped once finishErr is already non-nil: a run
		// whose own outcome failed to record is not worth a second write
		// attempt on top of it.
		finishErr = store.SetRunTranscript(ctx, d.RunID, result.Transcript)
	}
	// Only now, with the store already carrying whatever final story this
	// run has to tell (FinishRun and SetRunTranscript, just above), does
	// the live file cfg.TranscriptDir named stop mattering: a caller
	// still polling AttemptTranscript sees FinishedAt set and reads the
	// store from here on, never this file again, so removing it earlier
	// (before either write above landed) would have opened a window where
	// a still-"running" attempt suddenly read back as empty. Best-effort:
	// a Framework never given a TranscriptPath (or one that errored before
	// opening it) leaves nothing here to remove, and os.Remove's own
	// error for that is ignored the same way any other remove failure
	// would be -- there is nothing left to do differently about a stray
	// file at this point beyond leaving it for an operator to notice.
	if transcriptPath != "" {
		os.Remove(transcriptPath)
	}
	revokeAll(ctx, store, cc, materialized)
	if finishErr != nil {
		return nil, fmt.Errorf("orchestrator: finishing run %s: %w", d.RunID, finishErr)
	}
	if runErr != nil {
		// result, not nil: a failed run that pushed a branch before it
		// broke still left work on GitHub, and the caller needs the
		// result to find it. See agent.Framework.
		return result, runErr
	}
	return result, nil
}

// partialWorkSuffix names what a failed run got done before it failed, or
// nothing at all when the framework returned no result to say. Kept to
// names and counts, like noActionDetail: this lands in a stored outcome
// column that `grain get` prints, not a transcript.
func partialWorkSuffix(result *agent.Result) string {
	if result == nil || len(result.ToolCalls) == 0 {
		return ""
	}
	return fmt.Sprintf("; the run made %d tool call(s)%s first",
		len(result.ToolCalls), toolCallSummary(result))
}

// outcomeOf reads agent.Result.ToolCalls -- the only record of what
// happened inside the run (mcp/mock_tools.go's own sink is internal and
// discarded when Run returns) -- and turns it into a run outcome and a
// short reason for it: any error tool call fails the run, and so does a
// run that made no tool call at all, since an agent that never touched
// run_command did not do the work. Ported from pkg/orchestrate's own
// runAgent (bwsalmon/agents#254), extended with detail for
// bwsalmon/agents#403's own "a human should see why, not just that".
func outcomeOf(result *agent.Result) (outcome, detail string) {
	sawTool := false
	for _, c := range result.ToolCalls {
		sawTool = true
		if c.IsError {
			return "failed", fmt.Sprintf("tool call %q failed: %s", c.Name, c.Text)
		}
	}
	if !sawTool {
		return "failed", "the agent made no tool calls at all"
	}
	return "succeeded", ""
}

// prepareCapabilities resolves and materializes cc.Task's capability
// grants against reg and assembles the prompt they, the task itself, and
// comments (its conversation so far -- see commentThreadSection) all
// contribute, applying every SideSandbox placement under sandboxRoot on
// the way. A nil registry, or a task with no Grants, skips capability
// resolution and returns BuildPrompt's own prompt plus the comment thread
// unchanged -- a deployment or test that grants no capabilities needs to
// configure none of this. A non-nil error means preparation itself failed
// (or a grant was refused) and the caller must not run the agent at all
// -- the same "a half-materialized capability is never described to the
// agent as present" rule model.MaterializeGrants's own doc comment holds
// to, one level up: an agent whose capability request was refused must
// not run at all, since the task it would work almost always depends on
// it. Ported from pkg/orchestrate's own prepare (bwsalmon/agents#254).
func prepareCapabilities(ctx context.Context, reg *model.CapabilityRegistry,
	cc model.CapabilityContext, sandboxRoot string, comments []model.Comment,
	checkoutDir string) (materialized []model.Materialized, prompt string, err error) {

	prompt = BuildPrompt(cc.Task, checkoutDir)
	if thread := commentThreadSection(comments); thread != "" {
		prompt += "\n\n" + thread
	}
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
