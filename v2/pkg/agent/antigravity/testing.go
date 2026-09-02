package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/mcp"
)

// Step is one turn a scripted run takes: either a tool call (Tool named,
// Args its arguments) or the final text answer that ends the run (Tool
// empty, Text set). It is this package's replacement for the
// *genai.GenerateContentResponse values the former in-process Gemini
// runtime's tests scripted -- the same two shapes a model turn ever had,
// named directly instead of through the Gemini API's own types, so a test
// scripting a run no longer depends on that API at all.
type Step struct {
	Tool string
	Args map[string]any
	Text string
}

// ToolStep scripts one tool call.
func ToolStep(name string, args map[string]any) Step {
	return Step{Tool: name, Args: args}
}

// TextStep scripts a final text answer, ending the run.
func TextStep(text string) Step { return Step{Text: text} }

// Script decides what a scripted run does, one turn at a time. Next is
// called once per turn and, crucially, only after every earlier step's
// tool call has actually finished -- the same contract the Gemini
// runtime's own contentGenerator seam had, which is what lets a script
// decide its next move from work the previous one really did (and lets
// the load test measure a dispatch's real duration at the moment it
// yields its last step). ok false ends the run.
//
// prompt is the run's own prompt, so a script can choose what to do from
// it -- what the randomized cluster test's generator reads its target
// repo and branch out of.
type Script interface {
	Next(prompt string) (step Step, ok bool)
}

// ScriptFunc adapts a plain func to Script.
type ScriptFunc func(prompt string) (Step, bool)

// Next implements Script.
func (f ScriptFunc) Next(prompt string) (Step, bool) { return f(prompt) }

// Steps returns a Script that plays a fixed list of steps in order -- the
// common case, and the direct equivalent of handing the former Gemini
// runtime a fixed []*genai.GenerateContentResponse.
func Steps(steps ...Step) Script {
	i := 0
	return ScriptFunc(func(string) (Step, bool) {
		if i >= len(steps) {
			return Step{}, false
		}
		s := steps[i]
		i++
		return s, true
	})
}

// NewForTest returns a Framework that runs script instead of the real agy
// binary, for callers outside this package that need to drive the
// tool-call loop without a live credential -- pkg/gitproxy's live_test.go,
// notably, which drives a full clone/commit/push through a real git proxy
// this way, and the e2e suite, whose whole point is that a real agent run
// really does reach a real sandbox.
//
// The fake is deliberately not a canned transcript. It stands up the same
// in-process pkg/mcp registry the "mcpserver" subcommand would have
// exposed to a real agy, executes every scripted tool call against it for
// real, and emits the same stream-json events a real agy emits as it goes
// -- so what a test exercises is the actual tool dispatch and this
// package's actual parser, with only the model's choice of moves faked.
// A canned stream would have executed nothing, and quietly turned every
// end-to-end test that pushes a branch into one that does not.
func NewForTest(script Script, opts ...Option) *Framework {
	f := newFramework(&scriptRunner{script: script}, "test-grain-binary", opts...)
	return f
}

// scriptRunner is the runner NewForTest installs: a fake agy.
type scriptRunner struct {
	script Script
}

// Run implements runner. It reads the prompt back out of the stdin user
// event Framework.Run wrote, finds the sandbox this run was pointed at in
// the same --add-dir argument a real agy would have read, and plays the
// script against a live in-process MCP registry rooted there.
//
// Every event is written to tee as it is produced, not buffered until the
// end, because the two things spliced into tee -- the live transcript
// mirror and the turn cap -- both exist precisely to act on a run that is
// still going.
func (r *scriptRunner) Run(ctx context.Context, args []string, stdin string, _ []string, dir string, tee io.Writer) (string, error) {
	prompt, err := promptFromStdin(stdin)
	if err != nil {
		return "", err
	}
	root := flagValue(args, "--add-dir")
	if root == "" {
		root = dir
	}

	registry := mcp.NewRegistry()
	registry.Register(mcp.NewSandboxTools(root)...)
	registry.Register(mcp.NewMockTools(&mcp.MockSink{})...)
	client := mcp.NewInProcess(ctx, registry)
	defer client.Close()

	var out strings.Builder
	emit := func(v any) error {
		line, err := json.Marshal(v)
		if err != nil {
			return err
		}
		line = append(line, '\n')
		out.Write(line)
		if tee != nil {
			if _, err := tee.Write(line); err != nil {
				return err
			}
		}
		return nil
	}

	if err := emit(map[string]any{
		"event": "init",
		"init":  map[string]any{"cwd": root, "tools": allowedTools(), "permission_mode": "bypass"},
	}); err != nil {
		return out.String(), err
	}

	// idx numbers every step in one sequence, the way agy does -- and,
	// crucially, a tool call is preceded by the agent_response step of
	// the model turn that decided to make it. Emitting only the tool
	// step would make this fake's stream something no real agy produces,
	// and would leave Framework.Run's turn cap -- which counts completed
	// agent_response steps -- with nothing to count on a run that only
	// ever calls tools.
	idx := 0
	for {
		// Cancellation is honoured between turns, which is where the
		// turn cap's own cancel lands: a real agy killed mid-run stops
		// producing events, and so does this.
		if err := ctx.Err(); err != nil {
			return out.String(), err
		}
		next, ok := r.script.Next(prompt)
		if !ok {
			// A script that runs out without a final text answer is a
			// run that stopped saying anything -- report it the way a
			// real agy reports a run that ended without one.
			if err := emit(resultEvent("FAILURE", "", "script exhausted without a final answer")); err != nil {
				return out.String(), err
			}
			return out.String(), nil
		}
		if next.Tool == "" {
			if err := emit(agentResponseEvent(idx, next.Text)); err != nil {
				return out.String(), err
			}
			if err := emit(resultEvent("SUCCESS", next.Text, "")); err != nil {
				return out.String(), err
			}
			return out.String(), nil
		}

		// The model turn that chose this tool. It says nothing, which is
		// ordinary: parseEvents renders no paragraph for a silent turn,
		// and the turn cap still counts it.
		if err := emit(agentResponseEvent(idx, "")); err != nil {
			return out.String(), err
		}
		idx++
		if err := emit(toolEvent(idx, stateActive, next.Tool, next.Args, "", false)); err != nil {
			return out.String(), err
		}
		text, isError := callTool(ctx, client, next.Tool, next.Args)
		state := stateDone
		if isError {
			state = stateError
		}
		if err := emit(toolEvent(idx, state, next.Tool, next.Args, text, isError)); err != nil {
			return out.String(), err
		}
		idx++
	}
}

// callTool runs one scripted tool call for real and flattens the two ways
// it can fail -- a transport error and a tool-reported error -- into the
// one {text, isError} pair a stream-json tool step carries.
func callTool(ctx context.Context, client *mcp.Client, name string, args map[string]any) (string, bool) {
	res, err := client.CallTool(ctx, name, args)
	if err != nil {
		return err.Error(), true
	}
	return res.Text(), res.IsError
}

func agentResponseEvent(index int, text string) map[string]any {
	return map[string]any{
		"event": "step_update",
		"step_update": map[string]any{
			"step_index": index,
			"state":      stateDone,
			"step_type":  stepTypeAgentResponse,
			"text_delta": text,
		},
	}
}

func toolEvent(index int, state, name string, args map[string]any, output string, isError bool) map[string]any {
	info := map[string]any{"name": name, "parameters": args}
	if state != stateActive {
		if isError {
			info["error"] = map[string]any{"type": "tool_error", "message": output}
		} else {
			info["output"] = output
		}
	}
	return map[string]any{
		"event": "step_update",
		"step_update": map[string]any{
			"step_index": index,
			"state":      state,
			"step_type":  stepTypeTool,
			"tool_name":  name,
			"tool_info":  info,
		},
	}
}

func resultEvent(status, response, errText string) map[string]any {
	return map[string]any{
		"event": "result",
		"result": map[string]any{
			"status":   status,
			"response": response,
			"error":    errText,
		},
	}
}

// promptFromStdin reads back the user event Framework.Run wrote, so the
// fake sees exactly what a real agy would have been given rather than
// being handed the prompt by a side channel this package's real path does
// not have.
func promptFromStdin(stdin string) (string, error) {
	line := strings.TrimSpace(stdin)
	if line == "" {
		return "", fmt.Errorf("antigravity: no user event on stdin")
	}
	var ev struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return "", fmt.Errorf("antigravity: decoding the stdin user event: %w", err)
	}
	return ev.Message.Content, nil
}

// flagValue returns the value of a "--name value" pair in args, or "".
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
