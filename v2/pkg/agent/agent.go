// Package agent defines the seam between the controller and whichever
// process actually drives a coding agent. v1 has exactly one implementation
// of this shape, Claude, hardcoded throughout grain/automation/dispatch.py
// -- there is no equivalent interface there to reuse. This is that
// interface's first appearance, with one implementation so far
// (v2/agent/gemini); nothing in v2 constructs a Framework yet, since
// v2/pkg/dispatch has nowhere to run one (no host adapter, no GitHub
// client -- see v2/README.md).
package agent

import (
	"context"

	"github.com/bwsalmon/grain/v2/pkg/mcp"
)

// RunConfig is what one agent run needs to start: a prompt and where its
// sandbox tools should reach. Exactly one of SandboxRoot or Tools is
// expected to be set. SandboxRoot is the original, simpler shape --
// confine the run to a local directory, which a Framework turns into
// mcp.NewSandboxTools itself. Tools is the more general seam a caller that
// already has a real sandbox to hand over uses instead -- an
// orchestrator.KonturSandboxes-built mcp.NewSSHSandboxTools set, for
// instance -- letting a Framework stay agnostic about what backs a run's
// tool calls rather than only ever knowing how to build the local-
// directory kind itself.
type RunConfig struct {
	Prompt      string
	SandboxRoot string
	Tools       []mcp.Tool
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
	// (bwsalmon/agents#467, extended to agent/gemini by bwsalmon/
	// agents#513). "" means no caller wants this. The exact file format
	// is a Framework's own business -- pkg/agent/claude and pkg/agent/
	// gemini's own doc comments on Framework.Run say what each writes
	// there and how a reader gets a still-in-progress run's transcript
	// back out of it; the two formats differ (claude mirrors its
	// subprocess's raw stream-json, gemini writes the same
	// already-human-readable narrative it builds Result.Transcript
	// from), so each package also owns its own reader rather than
	// sharing one.
	TranscriptPath string
	// Addenda, if set, is polled by a Framework whose own loop has a
	// "between turns" to poll at (agent/gemini's Run) for anything a
	// human has added to the task's conversation since the last poll --
	// a comment posted while this very run is still in flight, not just
	// the ones already folded into Prompt at dispatch. It returns new
	// addenda oldest first, or nil if there are none; a Framework that
	// finds one folds it into the conversation as a fresh user turn, the
	// same way a redispatch already folds the whole thread in up front
	// (orchestrator.commentThreadSection).
	//
	// claude.Framework.Run never calls this: `claude -p` is one blocking
	// subprocess call with its whole prompt written to stdin before the
	// process starts, so there is no turn boundary here to poll at. A
	// comment posted while a claude run is in flight waits for the
	// task's next dispatch instead, exactly as it always has -- see that
	// package's own Run doc comment.
	Addenda func(ctx context.Context) ([]string, error)
}

// ToolCall records one function call an agent made and what it got back,
// for a caller that wants the play-by-play rather than just the outcome --
// the closest v2 equivalent of v1's transcript.py.
type ToolCall struct {
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
// own MCP server exposes -- see v2/mcp. Run should not return until the
// agent has produced a final answer or RunConfig.MaxTurns is exhausted.
//
// Run may return a non-nil Result together with a non-nil error, and a
// caller must read both: an error means the run did not finish, not that
// it did nothing. A run that edits files, commits, pushes and only then
// exhausts MaxTurns has already changed the world, and a caller that
// treats the error as "no result" strands that work -- see
// gemini.Framework.Run's own comment for the failure that taught this.
// A nil Result with an error means the run never started.
type Framework interface {
	Run(ctx context.Context, cfg RunConfig) (*Result, error)
}
