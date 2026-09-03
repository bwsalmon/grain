package orchestrator

// The run's own account of its tool use, turned into rows.
//
// toolCallSummary has always counted this -- every tool a run called, and
// how many of those calls came back as errors -- and has always rendered
// it into English for task_run.detail, where a human reads one run at a
// time and nothing can aggregate it. That sentence stays exactly as it
// is. This is the same census as data, so that "is edit_file's error rate
// climbing?", "how big does a run_command answer actually get?" and "how
// many pushes does a run make before its checks go green?" have answers
// over a window rather than per anecdote (docs/agent-ergonomics.md,
// findings 11 to 13).
//
// It is computed here, at the one moment it can be: agent.Result carries
// every call the run made and what each returned, and is discarded when
// RunDispatch returns. Nothing downstream can recover it -- the
// transcript is per-framework prose that a tool's own output can forge
// (outcomeOf's own doc comment on why counting calls out of it is
// unsound), and the store holds nothing else.
//
// Counts and sizes only. No argument, no result and no fragment of
// output crosses into the census: what a run's commands said is the
// task's business and stays in the transcript, and a measurement table
// that quoted them would be a second copy of it with none of its
// bounds.

import (
	"strings"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
)

// runTelemetry builds the census for one finished run.
//
// A run with no calls produces nothing at all (RunTelemetry.Empty), not a
// row of zeroes: "this run called nothing" is already recorded in its
// outcome and its detail, and a zero row here would only dilute every
// per-tool rate it was averaged into.
func runTelemetry(result *agent.Result) model.RunTelemetry {
	if result == nil || len(result.ToolCalls) == 0 {
		return model.RunTelemetry{}
	}
	var out model.RunTelemetry
	// The census keeps the order the run first reached for each tool,
	// which is stable and reproducible; the store's own read orders by
	// name for a report.
	var order []string
	byTool := map[string]*model.RunToolUse{}
	pushes := 0

	for _, call := range result.ToolCalls {
		use, seen := byTool[call.Name]
		if !seen {
			use = &model.RunToolUse{Tool: call.Name, Sizes: model.SizeHistogram{}}
			byTool[call.Name] = use
			order = append(order, call.Name)
		}
		use.Calls++
		if call.IsError {
			use.Errored++
		}
		size := int64(len(call.Text))
		use.ResultBytes += size
		if size > use.MaxResultBytes {
			use.MaxResultBytes = size
		}
		use.Sizes.Add(size)

		switch call.Name {
		case runCommandToolName:
			if mcp.RunCommandTimedOut(call.Text) {
				use.TimedOut++
			}
			// Counted before the wait below, so that a wait_for_checks
			// call records the pushes that preceded it -- the point of
			// PushesBefore is what this run had already pushed by the
			// time it asked.
			if isPush(call) {
				pushes++
			}
		case waitForChecksToolName:
			if wait, ok := mcp.ReadCheckWait(call.Text); ok {
				out.CheckWaits = append(out.CheckWaits, model.RunCheckWait{
					Seq:          len(out.CheckWaits),
					Verdict:      wait.Verdict,
					Waited:       wait.Waited,
					PushesBefore: pushes,
				})
			}
		}
	}

	for _, name := range order {
		out.Tools = append(out.Tools, *byTool[name])
	}
	return out
}

// The two tools the census reads more than a count off. Named here rather
// than spelled inline for the reason agent.ToolCall.Name's own doc
// comment gives: these are pkg/mcp's bare names, matched exactly, and a
// mismatch is silent.
const (
	runCommandToolName    = "run_command"
	waitForChecksToolName = "wait_for_checks"
)

// isPush reports whether a run_command call was the run pushing its
// branch.
//
// A heuristic, and the only one available: grain does not do the pushing,
// git does, inside the sandbox, at the agent's own hand. Reading the
// command it asked for is the one place a push is visible at all. It is
// deliberately narrow -- the literal `git push`, and only a call that
// came back without an error, since a rejected push is not a push -- so
// that it under-counts rather than over-counts. What it feeds is
// "how many pushes before the checks went green", where an invented push
// would be worse than a missed one.
func isPush(call agent.ToolCall) bool {
	if call.IsError {
		return false
	}
	command, ok := call.Arguments["command"].(string)
	if !ok {
		return false
	}
	return strings.Contains(command, "git push")
}
