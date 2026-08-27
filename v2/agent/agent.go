// Package agent defines the seam between the controller and whichever
// process actually drives a coding agent. v1 has exactly one implementation
// of this shape, Claude, hardcoded throughout grain/automation/dispatch.py
// -- there is no equivalent interface there to reuse. This is that
// interface's first appearance, with one implementation so far
// (v2/agent/gemini); nothing in v2 constructs a Framework yet, since
// v2/loop has nowhere to run one (no host adapter, no GitHub client -- see
// v2/README.md).
package agent

import "context"

// RunConfig is what one agent run needs to start: a prompt and a directory
// its sandbox tools are confined to.
type RunConfig struct {
	Prompt      string
	SandboxRoot string
	// MaxTurns caps the number of model-response/tool-call round trips
	// before Run gives up and returns an error, guarding against a run
	// that never stops asking for tools. Zero means the framework's own
	// default.
	MaxTurns int
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
}

// Framework drives one agent run to completion, using only the tools its
// own MCP server exposes -- see v2/mcp. Run should not return until the
// agent has produced a final answer or RunConfig.MaxTurns is exhausted.
type Framework interface {
	Run(ctx context.Context, cfg RunConfig) (*Result, error)
}
