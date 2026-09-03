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

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/dispatch"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
)

// errTaskClosed is what context.Cause(runCtx) reads once
// watchForTaskClosed has cancelled a run -- RunDispatch checks for it by
// identity (errors.Is) to tell "this run was killed because its task got
// closed" apart from any other reason framework.Run might return an
// error, and to record outcome "cancelled" rather than "failed" for
// exactly that case.
var errTaskClosed = errors.New("orchestrator: task closed while its run was still live")

// errRunTimedOut is context.Cause's own report, by identity, for a run
// RunDispatch cancelled itself because it outran cfg.maxRunRuntime() --
// Config.MaxRunRuntime's own doc comment has the reasoning
// (bwsalmon/agents#575). Checked the same way errTaskClosed is, and
// recorded as outcome "cancelled" for the same reason: the run did not
// fail on its own, RunDispatch ended it.
var errRunTimedOut = errors.New("orchestrator: run exceeded its wall-clock time limit")

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
// bare, and leaves this prompt exactly as it always read -- except when
// task.Target is nil, which is a sandbox that was always going to be
// bare (checkout.go's prepareCheckout skips cloning outright for a task
// with no target) and gets its own sentence explaining why, rather than
// silently reading like a clone that simply failed.
//
// The one thing here that is advice rather than fact is
// proposalSection's follow-on task etiquette, and it is here for the same
// reason the rest is: the facts it stands on (this run's task id, whether
// that task auto-merges) are grain's own, and an agent told neither
// cannot fill in a proposal's depends_on or decide its auto_merge at all.
//
// task.Reads is mentioned but not enforced here: the git proxy already
// allows a fetch against any of them and refuses a push to any but
// task.Target (gitproxy/authorize.go), so this line is purely
// informational -- it tells the agent those repos exist and are safe to
// clone, rather than granting anything itself.
//
// The last paragraph, for a task that has a target at all, is the
// push/check/repair loop pkg/mcp's pull_request_status exists for. It is
// informational in the same way: nothing here grants a second push, the
// proxy already allowed every push to task.Target -- what it grants is
// the knowledge that the loop is available, which no tool description
// on its own can convey (see the comment at that paragraph).
func BuildPrompt(task model.Task, checkoutDir string) string {
	branch := model.BranchName(task.ID)
	var prompt string
	if task.Target == nil {
		prompt = fmt.Sprintf(
			"%s\n\n%s\n\nThis task has no repo attached -- there is nothing to clone, "+
				"check out, or push a branch to. Do the work directly in the sandbox "+
				"rather than expecting a git checkout to exist.",
			task.Title, task.Body,
		)
	} else {
		prompt = fmt.Sprintf(
			"%s\n\n%s\n\nWork in %s. Push your change to a new branch named %q -- "+
				"never to the repo's default branch directly.",
			task.Title, task.Body, task.Target, branch,
		)
	}
	if checkoutDir != "" {
		prompt += fmt.Sprintf(
			"\n\nThat repo is already cloned for you at ./%s, with %q checked out and "+
				"its remote pointing at the only address you can reach it through -- "+
				"work in that directory rather than cloning anything yourself, and "+
				"push with `git push origin %s`.",
			checkoutDir, branch, branch,
		)
	}
	// Said out loud because neither half is discoverable from the tools
	// alone. Nothing stops a run pushing repeatedly -- the branch is its
	// own and the proxy authorizes every push to it (gitproxy/authorize.go)
	// -- but the sentences above read as one final act, and a run that
	// treats them that way has no reason to ever call pull_request_status,
	// whose whole value is being called again after the next push. Leaving
	// that loop implicit is what left a red build to the merge queue's
	// separate fix task (sync.go's fileFixTask) even when the run that
	// caused it was still running.
	if task.Target != nil {
		prompt += fmt.Sprintf(
			"\n\nPush as often as you like: %q is your branch, and each push reruns CI "+
				"against the new commit. After a push, call `pull_request_status` to "+
				"see what GitHub's checks made of it -- that is how you find out "+
				"whether tests you cannot run in the sandbox actually pass. If any "+
				"check fails, fix it, push again and check again, rather than "+
				"finishing on a red build.",
			branch,
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
	prompt += proposalSection(task)
	return prompt
}

// proposalSection is the follow-on task etiquette every dispatch is told,
// and the two facts an agent cannot work out for itself that it needs to
// follow it: which task it is running as, and whether that task is an
// auto-merge job.
//
// Both are grain's own facts, the same reason BuildPrompt names the
// branch rather than letting an agent pick one. Without the task id, an
// agent splitting a piece out of the work it is doing has nothing to put
// in that proposal's depends_on -- it would have to reverse the id out of
// the branch name -- and relayProposedTasks resolves depends_on against
// real task ids, so a proposal that names nothing is filed unblocked and
// can be approved and dispatched beside the task it was meant to follow.
// Without knowing its own task auto-merges, an agent has no way to tell
// whether propose_task's auto_merge is even open to it: proposedAutoMerge
// caps a proposal at the proposing task's own setting, so the sentence is
// omitted, rather than negated, for a task that is not one -- there is
// nothing an agent could usefully do with "you may not ask for this".
func proposalSection(task model.Task) string {
	s := fmt.Sprintf(
		"\n\nYou are running as task %s. Anything you split out with propose_task "+
			"should say what it has to wait on in depends_on: task %s itself, when "+
			"the follow-up only makes sense once this task's own change has landed, "+
			"and the id you gave an earlier proposal in this same run that it builds "+
			"on. A proposal that names nothing is unblocked the moment a human "+
			"approves it, and can be dispatched beside work it was supposed to "+
			"follow.",
		task.ID, task.ID,
	)
	if task.AutoMerge {
		s += " This task is an auto-merge job: its pull request merges on its own " +
			"once its checks pass, with no human review. A proposal that is a piece " +
			"of this same task inherits that -- pass auto_merge: false on one that " +
			"is separate work and deserves a human's own review."
	}
	return s
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

// RunDispatch drives one dispatch.Dispatch to completion: resolve and
// materialize its task's capabilities (writing any SideSandbox placements
// through placer, or into sandboxRoot when the sandbox offers no placer --
// both may be empty when the task has nothing to place),
// run the agent against tools (whatever this run's own Sandbox.Tools
// produced -- see the package doc comment on the local-directory-vs-
// real-VM choice that makes), revoke whatever was materialized, and
// record the run's outcome. Every path here finishes the run, even a
// failing one -- ported from pkg/orchestrate's own runDispatch
// (bwsalmon/agents#254) when that package merged into this one: an
// unfinished run would hold its share of the concurrency limit forever. It does not touch
// task_observation or GitHub at all -- see ProcessResult for that half,
// kept separate the same way e2e's own runDispatch and its caller are,
// since deciding what a run produced is a different question from
// deciding what to do about it.
//
// sandboxRoot and konturVM both also travel straight into the
// agent.RunConfig framework.Run is given, for a Framework with no
// in-process route to tools to reach the same sandbox by itself -- see
// RunConfig's own doc comment. Either may be empty; runOne computes
// whichever this run's own Sandbox can actually offer. placer is the
// third of those, and unlike the other two it is used here rather than
// forwarded: it is how a sandbox with no local directory receives a
// placement at all (see SandboxPlacer), and nil for one that has no
// route of its own.
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
	cfg Config, task model.Task, d dispatch.Dispatch, tools []mcp.Tool, sandboxRoot, konturVM string,
	placer SandboxPlacer, at time.Time) (*agent.Result, error) {

	run := model.Run{ID: d.RunID, TaskID: d.TaskID, Sandbox: d.RunID, Attempt: d.Attempt, StartedAt: at}
	cc := model.CapabilityContext{Task: task, Run: run, Now: at, Workdir: sandboxRoot, Credentials: cfg.Credentials}

	comments, err := store.Comments(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: reading %s's conversation: %w", task.ID, err)
	}
	attachments, err := store.Attachments(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: reading %s's attachments: %w", task.ID, err)
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
		materialized, prompt, prepErr = prepareCapabilities(ctx, cfg.Capabilities, cc, sandboxRoot, placer, tools, comments, attachments, checkoutDir)
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
		// runCtx, not ctx, is what watchForTaskClosed cancels the instant
		// it sees this task closed, which is what makes cancelling this
		// run from outside the process running it possible at all -- see
		// that func's own doc comment. cancelRun(nil) once framework.Run
		// returns is what stops the watcher goroutine either way, whether
		// or not it was the one that ended the run.
		//
		// agentCtx, not runCtx, is what framework.Run actually gets: a
		// child of runCtx carrying its own deadline, cfg.maxRunRuntime()
		// out from now, so a run that outlives it gets cancelled the same
		// way a task-closed run does (Config.MaxRunRuntime's own doc
		// comment, bwsalmon/agents#575) without the deadline itself ever
		// touching runCtx or the watcher goroutine reading it. Its cause,
		// once framework.Run returns, tells the two forms of cancellation
		// apart from each other and from any other error framework.Run
		// might return, and from a runCtx cancellation propagated down to
		// it: context.Cause walks up to whichever ancestor was cancelled
		// first.
		runCtx, cancelRun := context.WithCancelCause(ctx)
		agentCtx, cancelAgentCtx := context.WithTimeoutCause(runCtx, cfg.maxRunRuntime(), errRunTimedOut)
		checkTaskClosed(ctx, store, task.ID, cancelRun)
		watcherDone := make(chan struct{})
		go func() {
			defer close(watcherDone)
			watchForTaskClosed(runCtx, ctx, store, task.ID, cfg.cancelPollInterval(), cancelRun)
		}()

		// Repo/Branch are the same pair BuildPrompt names in the prompt,
		// passed structurally as well so a Framework can scope its
		// forked mcpserver's pull_request_status to exactly this run's
		// branch. Empty for a task with no target, which
		// agent.RunConfig.Repo's own doc comment covers.
		var repo string
		if task.Target != nil {
			repo = task.Target.String()
		}
		result, runErr = framework.Run(agentCtx, agent.RunConfig{
			Prompt: prompt, Tools: tools, SandboxRoot: sandboxRoot, KonturVM: konturVM,
			Repo: repo, Branch: model.BranchName(task.ID),
			// TaskID is what lets a Framework's own forked mcpserver ask
			// the daemon to act for this run rather than only on its
			// sandbox -- open_pull_request, today (see RunConfig.TaskID).
			TaskID:   task.ID,
			MaxTurns: cfg.MaxAgentTurns, TranscriptPath: transcriptPath,
			Addenda: addendaPoller(store, task.ID, comments),
		})
		cancelAgentCtx()
		cancelRun(nil)
		<-watcherDone

		switch {
		case runErr != nil && errors.Is(context.Cause(agentCtx), errTaskClosed):
			outcome = "cancelled"
			detail = "the task was closed while this run was still live"
			runErr = fmt.Errorf("orchestrator: run %s: %w", d.RunID, errTaskClosed)
		case runErr != nil && errors.Is(context.Cause(agentCtx), errRunTimedOut):
			outcome = "cancelled"
			detail = fmt.Sprintf("the run exceeded its %s wall-clock limit", cfg.maxRunRuntime()) + partialWorkSuffix(result)
			runErr = fmt.Errorf("orchestrator: run %s: %w", d.RunID, errRunTimedOut)
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

	// cfg.now(), not at: at is the moment RunCycle began this whole
	// cycle, recorded as this run's StartedAt above, and passing it here
	// as well stamped finished_at with the start time -- so every run
	// ever recorded, however long the agent actually worked, read back
	// as having taken zero seconds. That reading is what `grain get` and
	// the UI's own attempt timeline show, and it hid the difference
	// between a run that failed on its first turn and one that worked
	// for an hour before it did -- the first question anyone asks of a
	// failed run. See Config.Now.
	finishErr := store.FinishRun(ctx, d.RunID, cfg.now(), outcome, detail)
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
// grants against reg and assembles the prompt they, the task itself,
// comments (its conversation so far -- see commentThreadSection) and
// attachments (every file the task or its conversation carries -- see
// placeAttachments) all contribute, applying every SideSandbox placement
// under sandboxRoot and every attachment under AttachmentsDir on the way.
// A nil registry, or a task with no Grants, skips capability resolution
// and returns the rest of the prompt unchanged -- a deployment or test
// that grants no capabilities needs to configure none of this. A non-nil
// error means preparation itself failed (or a grant was refused) and the
// caller must not run the agent at all -- the same "a half-materialized
// capability is never described to the agent as present" rule
// model.MaterializeGrants's own doc comment holds to, one level up: an
// agent whose capability request was refused must not run at all, since
// the task it would work almost always depends on it. Ported from
// pkg/orchestrate's own prepare (bwsalmon/agents#254).
//
// That rule holds for every grant, including one a task was filed with
// because the deployment attaches it to everything, or because the repo
// it targets adds it (model.Config.DefaultCapabilities,
// model.RepoConfig.DefaultCapabilities, model.GrantByDefault). There is no
// "degrade rather than fail" tier here, and it is not an oversight: v1
// needed one because it minted a GCP key per dispatch for every sandbox,
// with no task holding the request and nowhere to record that it had
// failed, so swallowing the error was the only way a broken minter did
// not stop the deployment. A default here is instead seeded onto the
// task at creation, so a failed mint fails one task, says so on it, and
// is fixed either by fixing the capability or by detaching it -- from
// that task, or from the default set, whichever the operator meant.
// Silently running an agent without a capability its task is recorded as
// holding would trade that for a run that quietly does the wrong work.
func prepareCapabilities(ctx context.Context, reg *model.CapabilityRegistry,
	cc model.CapabilityContext, sandboxRoot string, placer SandboxPlacer, tools []mcp.Tool, comments []model.Comment,
	attachments []model.Attachment, checkoutDir string) (materialized []model.Materialized, prompt string, err error) {

	prompt = BuildPrompt(cc.Task, checkoutDir)
	if thread := commentThreadSection(comments); thread != "" {
		prompt += "\n\n" + thread
	}
	attachmentsSection, err := placeAttachments(ctx, tools, attachments)
	if err != nil {
		return nil, "", err
	}
	prompt += attachmentsSection
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
	if err := applyPlacements(ctx, sandboxRoot, placer, materialized); err != nil {
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

// applyPlacements delivers every SideSandbox placement Materialize
// returned into the sandbox, by whichever of the two routes that sandbox
// has: placer, when it can write into itself over its own transport (a
// kontur VM, over the same SSH/docker-exec runner its tool calls use --
// see SandboxPlacer), or root, the local directory standing in for the
// sandbox otherwise -- see the package doc comment.
//
// placer wins where both exist, because it writes into the sandbox
// itself: a sandbox that offers one is telling this function where its
// filesystem actually is, and a root alongside it (no current Sandbox has
// both) could only be a staging copy of the same material on the
// controller's own disk -- a second copy of a credential is exactly what
// a placement should not leave behind.
//
// A placement's Path is always the absolute path it would land at inside
// a real sandbox (gcpkey.SandboxKeyPath, geminikey.KeyPath), which placer
// takes as given. root plays the same role a chroot's own root plays for
// such a path, which is why it is joined after stripping the leading
// separator rather than passed through mcp.resolvePath -- that function
// confines an agent's own tool arguments, a different question from where
// a controller-applied placement lands.
//
// A SideController placement is skipped, not written anywhere: no
// current provider returns one (gcpkey and geminikey both mint straight
// into the sandbox), and nothing in v2 has decided what a controller-side
// destination for one even is yet. Ported from pkg/orchestrate
// (bwsalmon/agents#254).
//
// Both root and placer are empty for a sandbox that offers neither. That
// is only a problem for a grant that actually materializes a SideSandbox
// placement (gcpkey, geminikey, githubsandbox): most grants, including
// the Configuration agent's own self-debug/self-repair, materialize none
// and so never reach the loop body below at all (bwsalmon/agents#643).
func applyPlacements(ctx context.Context, root string, placer SandboxPlacer, materialized []model.Materialized) error {
	for _, m := range materialized {
		for _, p := range m.Materialization.Placements {
			if p.Side != model.SideSandbox {
				continue
			}
			// Parsed before either branch so an unusable mode is
			// rejected the same way whichever route the placement
			// takes -- placer is handed the octal string itself,
			// since a remote `install -m` is what applies it there.
			mode, err := strconv.ParseUint(p.EffectiveMode(), 8, 32)
			if err != nil {
				return fmt.Errorf("placement %s: invalid mode %q: %w", p.Path, p.Mode, err)
			}
			if placer != nil {
				if err := placer.PlaceFile(ctx, p.Path, p.Content, p.EffectiveMode()); err != nil {
					return fmt.Errorf("capability %q placing %s in the sandbox: %w", m.Grant.Capability, p.Path, err)
				}
				continue
			}
			if root == "" {
				return fmt.Errorf("capability %q placed %s in the sandbox, but this sandbox has no local directory to place it in and no way to place it remotely", m.Grant.Capability, p.Path)
			}
			full := filepath.Join(root, strings.TrimPrefix(p.Path, "/"))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return fmt.Errorf("creating %s: %w", filepath.Dir(full), err)
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
