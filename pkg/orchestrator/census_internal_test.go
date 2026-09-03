package orchestrator

// What a run's tool calls become, before anything stores them.

import (
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/agent"
	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
)

func call(name, text string, isError bool) agent.ToolCall {
	return agent.ToolCall{Name: name, Text: text, IsError: isError}
}

func command(cmd, text string, isError bool) agent.ToolCall {
	return agent.ToolCall{
		Name: "run_command", Arguments: map[string]any{"command": cmd},
		Text: text, IsError: isError,
	}
}

func TestRunTelemetryCountsCallsErrorsAndSizesPerTool(t *testing.T) {
	result := &agent.Result{ToolCalls: []agent.ToolCall{
		command("go test ./...", strings.Repeat("x", 5000), true),
		command("go test ./pkg/model", strings.Repeat("x", 100), false),
		call("edit_file", "String not found", true),
		call("edit_file", "Edited", false),
		call("read_file", strings.Repeat("y", 30), false),
	}}

	got := runTelemetry(result)
	if len(got.Tools) != 3 {
		t.Fatalf("census = %+v, want a row per tool", got.Tools)
	}
	// Order is the order the run first reached for each tool.
	if got.Tools[0].Tool != "run_command" || got.Tools[1].Tool != "edit_file" || got.Tools[2].Tool != "read_file" {
		t.Errorf("tools came back %q, %q, %q", got.Tools[0].Tool, got.Tools[1].Tool, got.Tools[2].Tool)
	}
	cmd := got.Tools[0]
	if cmd.Calls != 2 || cmd.Errored != 1 {
		t.Errorf("run_command = %d calls, %d errored; want 2 and 1", cmd.Calls, cmd.Errored)
	}
	if cmd.ResultBytes != 5100 || cmd.MaxResultBytes != 5000 {
		t.Errorf("run_command sizes = %d total, %d max; want 5100 and 5000", cmd.ResultBytes, cmd.MaxResultBytes)
	}
	if cmd.Sizes.Total() != 2 || cmd.Sizes[model.SizeBucket(5000)] != 1 {
		t.Errorf("run_command histogram = %v, want both its results in it", cmd.Sizes)
	}
	// An errored call is an ordinary turn of the agent's own loop, and is
	// counted as a call as well as an error -- an error rate over calls
	// that excluded them could never exceed zero.
	if edits := got.Tools[1]; edits.Calls != 2 || edits.Errored != 1 {
		t.Errorf("edit_file = %+v, want 2 calls of which 1 errored", edits)
	}
}

// A run that never got as far as a tool records nothing, rather than a
// row of zeroes that would drag every rate it was averaged into.
func TestRunTelemetryIsEmptyForARunThatCalledNothing(t *testing.T) {
	if !runTelemetry(nil).Empty() {
		t.Error("a run with no result recorded a census")
	}
	if !runTelemetry(&agent.Result{}).Empty() {
		t.Error("a run that made no calls recorded a census")
	}
}

// The timeout count is the sandbox's own health, and it comes from the
// bound's notice rather than from the exit status: exit=-1 is also what
// "could not start the command at all" looks like.
func TestRunTelemetryCountsOnlyTheCommandsTheBoundKilled(t *testing.T) {
	// A copy of the notice pkg/mcp appends, checked against the reader
	// that owns it before it is relied on: a stale copy here fails as
	// "this literal is out of date" rather than as "the census stopped
	// counting timeouts".
	killed := "exit=-1\nstdout:\nbuilding\nstderr:\n\n\n[grain] Killed after 300s by " +
		"run_command's default bound, which this call did not choose -- it passed no `timeout`."
	if !mcp.RunCommandTimedOut(killed) {
		t.Fatalf("this test's copy of run_command's timeout notice is stale:\n%s", killed)
	}
	result := &agent.Result{ToolCalls: []agent.ToolCall{
		command("sleep 600", killed, true),
		command("false", "exit=1\nstdout:\n\nstderr:\n", true),
		command("true", "exit=0\nstdout:\n\nstderr:\n", false),
	}}
	got := runTelemetry(result)
	if len(got.Tools) != 1 {
		t.Fatalf("census = %+v, want one row", got.Tools)
	}
	if use := got.Tools[0]; use.Calls != 3 || use.Errored != 2 || use.TimedOut != 1 {
		t.Errorf("run_command = %+v; want 3 calls, 2 errored, 1 killed by the bound", use)
	}
}

// The CI loop, in the order the run went round it: a push, a wait that
// failed, another push, a wait that passed. PushesBefore on that last
// wait is the answer to "how many pushes did this run take to go green".
func TestRunTelemetryRecordsTheCILoopInOrder(t *testing.T) {
	result := &agent.Result{ToolCalls: []agent.ToolCall{
		command("cd work && git commit -am wip", "exit=0\nstdout:\n\nstderr:\n", false),
		command("cd work && git push origin grain/task-1", "exit=0\nstdout:\n\nstderr:\n", false),
		call("wait_for_checks", "Waited 4m12s for CI on grain/task-1 at abc1234.\n\n"+
			"Checks against abc1234:\n  FAILING tests\n\n1 failing, 0 not finished, 0 otherwise done.\n\n"+
			"CI has failed, so I stopped waiting. Reproduce those failures", false),
		command("cd work && git push origin grain/task-1", "exit=0\nstdout:\n\nstderr:\n", false),
		call("wait_for_checks", "Waited 9m0s for CI on grain/task-1 at def5678.\n\n"+
			"Checks against def5678:\n  ok tests\n\n0 failing, 0 not finished, 1 otherwise done.\n\n"+
			"Every check against def5678 finished and none of them failed. Note that", false),
	}}

	got := runTelemetry(result)
	if len(got.CheckWaits) != 2 {
		t.Fatalf("recorded %d CI waits, want 2: %+v", len(got.CheckWaits), got.CheckWaits)
	}
	first, second := got.CheckWaits[0], got.CheckWaits[1]
	if first.Seq != 0 || first.Verdict != mcp.WaitVerdictFailed ||
		first.Waited != 4*time.Minute+12*time.Second || first.PushesBefore != 1 {
		t.Errorf("first wait = %+v", first)
	}
	if second.Seq != 1 || second.Verdict != mcp.WaitVerdictPassed ||
		second.Waited != 9*time.Minute || second.PushesBefore != 2 {
		t.Errorf("second wait = %+v", second)
	}
}

// A push that GitHub refused is not a push. PushesBefore feeds "how many
// pushes before green", where an invented one is worse than a missed one.
func TestRunTelemetryDoesNotCountARejectedPush(t *testing.T) {
	result := &agent.Result{ToolCalls: []agent.ToolCall{
		command("cd work && git push origin grain/task-1", "exit=1\nstdout:\n\nstderr:\nrejected", true),
		command("cd work && git status", "exit=0\nstdout:\n\nstderr:\n", false),
		call("wait_for_checks", "Waited 30s for CI on grain/task-1 at abc1234.\n\n"+
			"GitHub still reports no checks at all against abc1234 after 30s. Either this repo", false),
	}}
	got := runTelemetry(result)
	if len(got.CheckWaits) != 1 {
		t.Fatalf("recorded %d CI waits, want 1", len(got.CheckWaits))
	}
	if got.CheckWaits[0].Verdict != mcp.WaitVerdictNoChecks || got.CheckWaits[0].PushesBefore != 0 {
		t.Errorf("wait = %+v, want no checks and no pushes before it", got.CheckWaits[0])
	}
}
