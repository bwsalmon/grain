// Package agent defines the seam between the controller and whichever
// process actually drives a coding agent. v1 has exactly one implementation
// of this shape, Claude, hardcoded throughout grain/automation/dispatch.py
// -- there is no equivalent interface there to reuse. This is that
// interface's first appearance. Both implementations today
// (pkg/agent/antigravity, pkg/agent/claude) drive a real CLI as a
// subprocess; cmd/grain/daemon.go's agentFrameworks is what chooses
// between them, per run, from the task's own agent-framework setting or
// the deployment-wide default.
package agent

import (
	"context"

	"github.com/bwsalmon/grain/pkg/mcp"
)

// RunConfig is what one agent run needs to start: a prompt and where its
// sandbox tools should reach. A Framework that forks a real MCP client
// as a subprocess and lets it manage its own connection -- which both
// agent/antigravity and agent/claude do -- cannot consume Tools at all,
// there being no in-process registry to hand a forked process, so it
// reads SandboxRoot or KonturVM instead, whichever is set, and builds the
// arguments its own forked "mcpserver" subcommand needs to reach that
// sandbox: -sandbox-root for a local directory, or -kontur-vm for a named
// orchestrator.KonturSandboxes VM.
//
// Tools therefore has no consumer at present. It was read by the
// in-process Gemini runtime agent/antigravity replaced, and RunDispatch
// still populates it from orchestrator.Config.GrantTools (whose own doc
// comment, and selfrepair.Confirm's, say what that costs); it is kept
// here rather than deleted because it belongs to the interface, not to
// this moment's two implementations.
type RunConfig struct {
	Prompt      string
	SandboxRoot string
	// KonturVM, when SandboxRoot is empty, names a bwsalmon/kontur-managed
	// VM (orchestrator.KonturSandboxes.VMNameFor's own result, not the
	// sandbox/run's own name) a Framework with no in-process route to a
	// sandbox can point its own forked "mcpserver -kontur-vm" subprocess
	// at instead of a local directory -- see agent/antigravity's and
	// agent/claude's Framework.Run.
	KonturVM string
	// Repo ("owner/name") and Branch are the repository this run pushes
	// to and the branch it pushes -- model.BranchName's answer for this
	// task, the same pair BuildPrompt already names in the prompt. A
	// Framework passes them to its forked "mcpserver" subprocess so
	// pkg/mcp's pull_request_status can read CI for exactly that branch
	// and no other (cmd/grain/mcpserver.go's -pr-repo/-pr-branch).
	//
	// Both empty is a task with no repo attached, which is a real case
	// (Target is a pointer, and BuildPrompt has a sentence for it): the
	// tool then reports that there is nothing to look at rather than
	// disappearing from the roster.
	//
	// They are deliberately not derived from SandboxRoot or from
	// anything the agent can influence. Where a run's tools may look is
	// grain's to decide, the same "deterministic, not self-reported"
	// rule model.BranchName's own doc comment sets out for the branch
	// itself.
	Repo   string
	Branch string
	// TaskID is the task this run belongs to -- the one fact a forked
	// "mcpserver" subprocess needs before it can ask the daemon to act on
	// this run's behalf rather than only on its sandbox. It is passed as
	// that subprocess's own -task, alongside the daemon URL the Framework
	// itself was constructed with (agent/claude's WithGrainServer), and
	// what it buys is the open_pull_request tool: a run that can open its
	// own pull request while it still has turns left to react to CI.
	//
	// Empty -- a caller that has no task, or a Framework never told where
	// its daemon is -- simply leaves that tool unregistered, and the run
	// works exactly as it did before: its branch still becomes a pull
	// request when orchestrator.ProcessResult finishes the run.
	TaskID string
	Tools  []mcp.Tool
	// MaxTurns caps the number of model-response/tool-call round trips
	// before Run gives up and returns an error, guarding against a run
	// that never stops asking for tools. Zero means the framework's own
	// default.
	MaxTurns int
	// TranscriptPath, if set, is a file a Framework may write its own
	// transcript-in-progress to as the run proceeds, rather than only
	// handing one back in Result.Transcript once Run returns -- what
	// lets a caller with filesystem access to that path show a
	// still-running run's output, not just a finished one's
	// (bwsalmon/agents#467). "" means no caller wants this. The exact
	// file format is a Framework's own business -- pkg/agent/claude and
	// pkg/agent/antigravity's own doc comments on Framework.Run say what
	// each writes there and how a reader gets a still-in-progress run's
	// transcript back out of it. Both mirror their subprocess's raw
	// stream-json, but the two event vocabularies differ, so each
	// package owns its own reader rather than sharing one.
	TranscriptPath string
	// Addenda, if set, is polled by a Framework whose own loop has a
	// "between turns" to poll at, for anything a
	// human has added to the task's conversation since the last poll --
	// a comment posted while this very run is still in flight, not just
	// the ones already folded into Prompt at dispatch. It returns new
	// addenda oldest first, or nil if there are none; a Framework that
	// finds one folds it into the conversation as a fresh user turn, the
	// same way a redispatch already folds the whole thread in up front
	// (orchestrator.commentThreadSection).
	//
	// Neither Framework calls this today: both run one blocking
	// subprocess with the whole prompt written to stdin before the
	// process starts, so there is no turn boundary here to poll at. A
	// comment posted while a run is in flight waits for the task's next
	// dispatch instead -- see each package's own Run doc comment. It was
	// polled by the in-process Gemini runtime agent/antigravity
	// replaced, which owned its own turn loop and so had a "between
	// turns" to poll at.
	Addenda func(ctx context.Context) ([]string, error)
}

// ToolCall records one function call an agent made and what it got back,
// for a caller that wants the play-by-play rather than just the outcome --
// the closest v2 equivalent of v1's transcript.py.
type ToolCall struct {
	// Name is the tool's own name, as pkg/mcp registers it
	// ("ask_question", "run_command"), never a CLI's namespaced
	// spelling of it. Both frameworks drive a CLI that reports a tool
	// loaded from an MCP config as "mcp__grain-sandbox__<tool>", so both
	// put a reported name through mcp.BareToolName before recording it
	// here, and any Framework added later must do the same: a consumer
	// downstream of Result matches this field against the tool
	// vocabulary pkg/mcp defines -- orchestrator.ProcessResult decides
	// whether a run asked a question or left a closing comment by
	// comparing against "ask_question" and "comment_on_issue" exactly --
	// and a prefixed name silently matches none of them.
	Name      string
	Arguments map[string]any
	Text      string
	IsError   bool
}

// Result is what a run produced: the agent's final text answer plus every
// tool call it made along the way.
type Result struct {
	FinalText string
	ToolCalls []ToolCall
	// Transcript is a human-readable, chronological narrative of the run
	// -- its thinking and text output interleaved with each tool call and
	// what it got back -- for a caller that wants to read the whole story
	// rather than just its outcome (bwsalmon/agents#446's "show attempt
	// agent logs": a single scrolling pane to debug or check up on a run
	// with). It is best-effort: "" means the framework that produced this
	// Result does not build one, not that the run said nothing.
	Transcript string
}

// Framework drives one agent run to completion, using only the tools its
// own MCP server exposes -- see mcp. Run should not return until the
// agent has produced a final answer or RunConfig.MaxTurns is exhausted.
//
// Run may return a non-nil Result together with a non-nil error, and a
// caller must read both: an error means the run did not finish, not that
// it did nothing. A run that edits files, commits, pushes and only then
// exhausts MaxTurns has already changed the world, and a caller that
// treats the error as "no result" strands that work.
// A nil Result with an error means the run never started -- a line
// orchestrator.RunDispatch reads literally, so a Framework must not
// manufacture an empty Result for a run that produced nothing (see
// antigravity.partialResult).
type Framework interface {
	Run(ctx context.Context, cfg RunConfig) (*Result, error)
}

